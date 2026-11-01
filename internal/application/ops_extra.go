package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/memory/corpus"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/providers/websearch"
	"masamong/internal/storage"
)

// ---------------------------------------------------------------------------
// provider health: last success, last rejection, key age
// ---------------------------------------------------------------------------

// ProviderHealth is what the console shows next to a key: when it last worked and why it last failed.
type ProviderHealth struct {
	LastOK       string `json:"last_ok,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	LastErrorAt  string `json:"last_error_at,omitempty"`
	Rejected     bool   `json:"rejected"` // the provider refused the key (401/403): replace it
	KeyUpdatedAt string `json:"key_updated_at,omitempty"`
}

var providerHealthMu sync.Mutex

// noteProvider records the outcome of a live provider call. Writes are skipped while nothing changes
// (a success within the last ten minutes), so busy tools do not write on every call.
func (s *Service) noteProvider(id string, err error) {
	if id == "" || s.Store == nil || s.practice {
		return
	}
	providerHealthMu.Lock()
	defer providerHealthMu.Unlock()
	key := "provider:" + id
	var h ProviderHealth
	_, _ = s.kvGet(key, &h)
	now := s.Clock.Now().UTC()
	if err == nil {
		if last, e := time.Parse(time.RFC3339, h.LastOK); e == nil && now.Sub(last) < 10*time.Minute && !h.Rejected {
			return
		}
		h.LastOK, h.Rejected = now.Format(time.RFC3339), false
	} else {
		if errors.Is(err, errQuota) || errors.Is(err, providers.ErrNoCredential) {
			return // our own limits and missing keys are shown elsewhere
		}
		h.LastError, h.LastErrorAt = clip(err.Error(), 300), now.Format(time.RFC3339)
		h.Rejected = errors.Is(err, openai.ErrAuth) || errors.Is(err, websearch.ErrAuth)
		var pe *providers.Error
		if errors.As(err, &pe) && (pe.Status == 401 || pe.Status == 403) {
			h.Rejected = true
		}
	}
	_ = s.kvSet(key, h)
}

// providerHealth returns every provider's health with the key file's age.
func (s *Service) providerHealth() map[string]ProviderHealth {
	out := map[string]ProviderHealth{}
	for id, p := range s.Doc().Providers {
		var h ProviderHealth
		if s.Store != nil {
			_, _ = s.kvGet("provider:"+id, &h)
		}
		if path, ok := strings.CutPrefix(p.CredentialRef, "file:"); ok {
			if fi, err := os.Stat(path); err == nil {
				h.KeyUpdatedAt = fi.ModTime().UTC().Format(time.RFC3339)
			}
		}
		out[id] = h
	}
	return out
}

// ---------------------------------------------------------------------------
// config snapshots: named copies of the whole document
// ---------------------------------------------------------------------------

var snapshotSQLite = `CREATE TABLE IF NOT EXISTS config_snapshots (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	at TEXT NOT NULL,
	revision INTEGER NOT NULL,
	actor TEXT NOT NULL DEFAULT '',
	yaml TEXT NOT NULL
)`

var snapshotTiDB = `CREATE TABLE IF NOT EXISTS config_snapshots (
	id VARCHAR(64) NOT NULL PRIMARY KEY,
	name VARCHAR(128) NOT NULL,
	note VARCHAR(512) NOT NULL DEFAULT '',
	at VARCHAR(64) NOT NULL,
	revision INT NOT NULL,
	actor VARCHAR(128) NOT NULL DEFAULT '',
	yaml MEDIUMTEXT NOT NULL
)`

func (s *Service) snapshotsAction(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	in := inputMap(req)
	cur, _ := config.Marshal(s.Doc())
	switch req.Action {
	case "config.snapshot":
		name := strings.TrimSpace(strVal(in, "name", ""))
		if name == "" || len([]rune(name)) > 80 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "name required (80 characters at most)", nil)
		}
		id := newID("snap_")
		if _, err := db.DB.Exec(`INSERT INTO config_snapshots(id, name, note, at, revision, actor, yaml) VALUES(?,?,?,?,?,?,?)`,
			id, name, clip(strVal(in, "note", ""), 400), s.nowString(), s.Revision(), actorName(req), string(cur)); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return action.OK(req, map[string]any{"id": id, "name": name, "revision": s.Revision()})
	case "config.snapshot.delete":
		if _, err := db.DB.Exec(`DELETE FROM config_snapshots WHERE id=?`, strVal(in, "id", "")); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return action.OK(req, map[string]any{"deleted": strVal(in, "id", "")})
	case "config.snapshot.restore":
		var yaml, name string
		if err := db.DB.QueryRow(`SELECT yaml, name FROM config_snapshots WHERE id=?`, strVal(in, "id", "")).Scan(&yaml, &name); err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "no such snapshot", nil)
		}
		if boolVal(in, "dry_run") {
			out := map[string]any{"dry_run": true, "name": name, "diff": lineDiff(string(cur), yaml), "changed": yaml != string(cur)}
			if changes, err := configChanges(cur, []byte(yaml)); err == nil {
				out["changes"] = changes
			}
			return action.OK(req, out)
		}
		base := s.Revision()
		if req.BaseRevision > 0 {
			base = req.BaseRevision
		}
		doc, err := config.ValidateCandidate([]byte(yaml))
		if err != nil {
			return action.Fail(req, errcode.ConfigInvalid, "generic.error", err.Error(), nil)
		}
		// keep what must follow this machine, not the snapshot: instance identity and console address
		doc.Instance.ID, doc.Instance.DataDir, doc.Admin = s.Doc().Instance.ID, s.Doc().Instance.DataDir, s.Doc().Admin
		raw, err := config.Marshal(doc)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return s.applyCandidate(req, raw, base)
	}
	rows, err := db.DB.Query(`SELECT id, name, note, at, revision, actor, yaml FROM config_snapshots ORDER BY at DESC LIMIT 200`)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id, name, note, at, actor, yaml string
		var rev int
		if rows.Scan(&id, &name, &note, &at, &rev, &actor, &yaml) != nil {
			continue
		}
		list = append(list, map[string]any{"id": id, "name": name, "note": note, "at": at, "revision": rev, "actor": actor,
			"bytes": len(yaml), "same_as_current": yaml == string(cur)})
	}
	return action.OK(req, map[string]any{"snapshots": list})
}

// ---------------------------------------------------------------------------
// reports: a period summary built from the turn log, spend and tool usage
// ---------------------------------------------------------------------------

func (s *Service) reportCreate(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	in := inputMap(req)
	days := min(max(intVal(in, "days", 30), 1), 365)
	loc := s.instanceZone()
	now := s.Clock.Now().In(loc)
	from := now.AddDate(0, 0, -days+1).Format("2006-01-02")
	to := now.Format("2006-01-02")
	outcomes := map[string]int{}
	var total, latSum, latN int
	if rows, err := db.DB.Query(`SELECT outcome, COUNT(*), COALESCE(SUM(latency_ms),0) FROM turn_log WHERE lab=0 AND day>=? GROUP BY outcome`, from); err == nil {
		for rows.Next() {
			var o string
			var c, lat int
			if rows.Scan(&o, &c, &lat) == nil {
				outcomes[o] = c
				total += c
				if o == "answered" {
					latSum, latN = latSum+lat, latN+c
				}
			}
		}
		rows.Close()
	}
	type kv struct {
		K string  `json:"key"`
		V float64 `json:"value"`
	}
	top := func(q string, args ...any) []kv {
		out := []kv{}
		rows, err := db.DB.Query(q, args...)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var x kv
			if rows.Scan(&x.K, &x.V) == nil {
				out = append(out, x)
			}
		}
		return out
	}
	channels := top(`SELECT guild_id || ':' || channel_id, COUNT(*) FROM turn_log WHERE lab=0 AND day>=? GROUP BY guild_id, channel_id ORDER BY COUNT(*) DESC LIMIT 10`, from)
	if s.Store.Backend == storage.BackendTiDB {
		channels = top(`SELECT CONCAT(guild_id, ':', channel_id), COUNT(*) FROM turn_log WHERE lab=0 AND day>=? GROUP BY guild_id, channel_id ORDER BY COUNT(*) DESC LIMIT 10`, from)
	}
	tools := []kv{}
	if s.toolUsage() != nil {
		tools = top(`SELECT tool, SUM(n) FROM tool_usage WHERE scope='all' AND day>=? GROUP BY tool ORDER BY SUM(n) DESC LIMIT 10`, from)
	}
	spend := top(`SELECT kind, SUM(usd) FROM memory_spend WHERE day>=? GROUP BY kind ORDER BY SUM(usd) DESC`, now.UTC().AddDate(0, 0, -days+1).Format("2006-01-02"))
	var cost float64
	for _, x := range spend {
		cost += x.V
	}
	var changes, failedChanges int
	_ = db.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0) FROM audit_log WHERE at>=?`,
		now.AddDate(0, 0, -days+1).UTC().Format(time.RFC3339)).Scan(&changes, &failedChanges)
	var reviews = map[string]int{}
	if rows, err := db.DB.Query(`SELECT verdict, COUNT(*) FROM lab_reviews WHERE at>=? GROUP BY verdict`, now.AddDate(0, 0, -days+1).UTC().Format(time.RFC3339)); err == nil {
		for rows.Next() {
			var v string
			var c int
			if rows.Scan(&v, &c) == nil {
				reviews[v] = c
			}
		}
		rows.Close()
	}
	avgLatency := 0
	if latN > 0 {
		avgLatency = latSum / latN
	}
	var history map[string]any
	if h := s.runtimeHistory(action.Request{Action: "runtime.history", Input: []byte(fmt.Sprintf(`{"days":%d}`, min(days, 90)))}); h.OK {
		history = map[string]any{"uptime_pct": h.Output["uptime_pct"], "crashes": h.Output["crashes"]}
	}
	return action.OK(req, map[string]any{
		"period": map[string]any{"from": from, "to": to, "days": days, "timezone": loc.String()}, "instance": s.Doc().Instance.ID,
		"generated_at": s.nowString(), "turns": map[string]any{"total": total, "by_outcome": outcomes, "avg_latency_ms": avgLatency},
		"channels": channels, "tools": tools, "spend": map[string]any{"total_usd": cost, "by_kind": spend, "budget_usd": parseUSD(s.Doc().Limits.MonthlyBudgetUSD, 0)},
		"changes": map[string]any{"total": changes, "failed": failedChanges}, "reviews": reviews, "uptime": history, "version": Version,
	})
}

// ---------------------------------------------------------------------------
// import preview: what a KakaoTalk export holds before it is imported
// ---------------------------------------------------------------------------

var errPreviewSpeakers = errors.New("preview supports at most 4096 distinct speakers")

func (s *Service) importsPreview(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	path := strVal(in, "path", "")
	if path == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "path required", nil)
	}
	var err error
	path, err = s.importPath(path)
	if err != nil {
		return action.Fail(req, errcode.PermissionDenied, "generic.error", err.Error(), nil)
	}
	if strVal(in, "source", "kakao_csv") != "kakao_csv" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "preview supports kakao_csv", nil)
	}
	room := strVal(in, "room", "kakao:preview")
	if !strings.HasPrefix(room, "kakao:") {
		room = "kakao:" + room
	}
	locale := strVal(in, "export_locale", corpus.DefaultMarkerLocale)
	var first, last string
	kinds := map[string]int{}
	speakers := map[string]int{}
	speakerBytes := 0
	var ids []string
	existing := 0
	flush := func() error {
		if len(ids) == 0 {
			return ctx.Err()
		}
		args := make([]any, len(ids))
		for j, id := range ids {
			args[j] = id
		}
		var n int
		q := `SELECT COUNT(*) FROM memory_observations WHERE observation_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		err := s.Store.Reader().QueryRowContext(ctx, q, args...).Scan(&n)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "no such table") {
			return err
		}
		existing += n
		ids = ids[:0]
		return nil
	}
	eligible := 0
	sha, rows, err := corpus.ReadKakaoCSVContext(ctx, path, strings.TrimPrefix(room, "kakao:"), room, locale, func(o corpus.Observation) error {
		kinds[o.RecordKind]++
		if o.Eligible() {
			if _, known := speakers[o.ActorLabel]; !known {
				if len(speakers) >= 4096 || speakerBytes+len(o.ActorLabel) > 2<<20 {
					return errPreviewSpeakers
				}
				speakerBytes += len(o.ActorLabel)
			}
			eligible++
			speakers[o.ActorLabel]++
			if first == "" || o.ParsedTime < first {
				first = o.ParsedTime
			}
			if o.ParsedTime > last {
				last = o.ParsedTime
			}
		}
		ids = append(ids, o.ObservationID)
		if len(ids) >= 500 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return s.importFailure(req, err, nil)
	}
	if err := flush(); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	type sp struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	top := []sp{}
	for k, v := range speakers {
		top = append(top, sp{k, v})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].N > top[j].N || (top[i].N == top[j].N && top[i].Name < top[j].Name) })
	if len(top) > 12 {
		top = top[:12]
	}
	// the room already in memory, if any: its time span shows whether the file overlaps it
	var roomFirst, roomLast sql.NullString
	var roomN int
	_ = s.Store.Reader().QueryRowContext(ctx, `SELECT MIN(parsed_time), MAX(parsed_time), COUNT(*) FROM memory_observations WHERE room=? AND deleted_at=''`, room).Scan(&roomFirst, &roomLast, &roomN)
	return action.OK(req, map[string]any{
		"rows": rows, "file_sha256": sha, "kinds": kinds, "eligible": eligible,
		"first": first, "last": last, "speakers": len(speakers), "top_speakers": top,
		"already_imported": existing, "new": rows - existing, "duplicate_file": existing > 0 && existing == rows,
		"room": map[string]any{"key": room, "messages": roomN, "first": roomFirst.String, "last": roomLast.String},
	})
}

// opsAttention covers backups, keys, crashes and restores for the dashboard's attention list.
func (s *Service) opsAttention() []attention {
	var att []attention
	now := s.Clock.Now()
	if s.practice {
		return att
	}
	att = append(att, s.updateAttention()...)
	// backups: the newest one must be younger than the schedule allows (or 30 days when off)
	bl := s.backupList()
	maxAge := 30 * 24 * time.Hour
	switch s.Doc().Backup.Schedule {
	case "daily":
		maxAge = 50 * time.Hour
	case "weekly":
		maxAge = 8 * 24 * time.Hour
	}
	if last, ok := bl["last_backup_at"].(string); ok {
		if at, err := time.Parse(time.RFC3339, last); err == nil && now.Sub(at) > maxAge {
			att = append(att, attention{"warn", "backup_stale", map[string]any{"days": int(now.Sub(at).Hours() / 24)}, "/maintenance"})
		}
	} else if started, err := time.Parse(time.RFC3339, s.firstStart()); err == nil && now.Sub(started) > 24*time.Hour {
		att = append(att, attention{"warn", "backup_none", nil, "/maintenance"})
	}
	if st, ok := bl["schedule_state"].(scheduleState); ok && !st.OK && st.LastRun != "" {
		att = append(att, attention{"warn", "backup_failed", map[string]any{"error": st.Error}, "/maintenance"})
	}
	if last, ok := bl["last_restore"].(map[string]any); ok {
		if at, err := time.Parse(time.RFC3339, fmt.Sprint(last["at"])); err == nil && now.Sub(at) < 7*24*time.Hour {
			if last["ok"] == true {
				if now.Sub(at) < 24*time.Hour {
					att = append(att, attention{"info", "restore_done", map[string]any{"name": last["name"]}, "/maintenance"})
				}
			} else {
				att = append(att, attention{"danger", "restore_failed", map[string]any{"name": last["name"], "safety": last["safety_backup"]}, "/maintenance"})
			}
		}
	}
	if bl["restore_pending"] == true {
		att = append(att, attention{"warn", "restore_pending", nil, "/maintenance"})
	}
	// keys: rejected by the provider, or not replaced for half a year
	ids := make([]string, 0, len(s.Doc().Providers))
	for id := range s.Doc().Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	health := s.providerHealth()
	for _, id := range ids {
		h := health[id]
		if h.Rejected {
			att = append(att, attention{"danger", "key_rejected", map[string]any{"provider": id}, "/connections"})
			continue
		}
		if at, err := time.Parse(time.RFC3339, h.KeyUpdatedAt); err == nil && now.Sub(at) > 180*24*time.Hour {
			att = append(att, attention{"info", "key_old", map[string]any{"provider": id, "days": int(now.Sub(at).Hours() / 24)}, "/connections"})
		}
	}
	// crashes in the last week
	if h := s.runtimeHistory(action.Request{Action: "runtime.history", Input: []byte(`{"days":7}`)}); h.OK {
		if n, _ := h.Output["crashes"].(int); n > 0 {
			att = append(att, attention{"warn", "crashed", map[string]any{"n": n}, "/logs?tab=uptime"})
		}
	}
	return att
}

// firstStart is when the uptime history begins (the instance's first recorded start).
func (s *Service) firstStart() string {
	var ev []uptimeEvent
	_, _ = s.kvGet("uptime:events", &ev)
	if len(ev) > 0 {
		return ev[0].At
	}
	return ""
}

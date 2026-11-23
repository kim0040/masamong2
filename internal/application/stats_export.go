package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/hostprofile"
)

// stats.export: the file of weekly totals an operator may attach to an issue by hand. Nothing is sent
// anywhere by this program. The file is built from stats_daily only (the daily totals the rollup and the
// counters wrote), through the registry, under the rules of the design review (H4, H5):
//   - ISO weeks in UTC; no daily and no hourly rows; no alerts in v1;
//   - activity as buckets (messages to one significant digit, rooms and people as ranges);
//   - a member-facing row with fewer than statsExportMinEvents events in a week is folded into "other"
//     (the model's own tool calls, via=chat, are left out of the action counts: decision 2026-11-22);
//   - every dimension value goes through the registry's export vocabulary (statsExportDims), so it is a
//     member of a code-defined list; operator names, custom provider keys, hostnames and fine-tune ids
//     become "other", "custom" or "custom_http";
//   - settings are booleans, enumerations from a fixed list and bucketed numbers: no names, URLs, keys,
//     persona text, timezone or Discord IDs;
//   - the install id is random, made at the first export and replaced on request.
//
// It reads what exists even when stats.enabled is false (review L2). A finished week never changes
// afterwards: stats_daily rows of a finished day are not rewritten, and nothing here reads a table that
// holds a person's rows, so a withdrawal moves no exported number (review M10).

const (
	statsInstallKey = "stats:install"
	// statsExportDefaultWeeks is the window of an export without a period.
	statsExportDefaultWeeks = 12
	// statsExportMaxWeeks caps a requested period.
	statsExportMaxWeeks = 156
	// statsExportSchema is the file's schema number.
	statsExportSchema = 1
	// statsExportSettleDays is how long after a week's last day it counts as finished: a local day ends
	// up to 14 hours after the UTC day it is named after, and a day is final shortly after local midnight.
	statsExportSettleDays = 2
)

// statsInstallState is what stays on this install about the export id (admin_state).
type statsInstallState struct {
	ID        string `json:"id"`
	FirstSeen string `json:"first_seen,omitempty"` // an ISO week, never a date
}

// statsExport is the stats.export Action.
func (s *Service) statsExport(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	input := statsExportInput{From: strVal(in, "from", ""), To: strVal(in, "to", ""), ResetID: boolVal(in, "reset_id")}
	doc, err := s.buildStatsExport(ctx, input)
	if err != nil {
		if bad, ok := err.(statsExportBadInput); ok {
			return action.Fail(req, errcode.InvalidInput, "generic.error", string(bad), nil)
		}
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return s.failErr(req, errcode.IO, "error.file", err, nil)
	}
	var file map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&file); err != nil {
		return s.failErr(req, errcode.IO, "error.file", err, nil)
	}
	return action.OK(req, map[string]any{"file": file, "bytes": len(raw), "collecting": s.statsOn()})
}

type statsExportBadInput string

func (e statsExportBadInput) Error() string { return string(e) }

// buildStatsExport reads stats_daily and builds the file. It writes only the install id (created at the
// first export, replaced when input.ResetID).
func (s *Service) buildStatsExport(ctx context.Context, in statsExportInput) (statsExportDoc, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	first, last, err := s.statsExportWeeks(in)
	if err != nil {
		return statsExportDoc{}, err
	}
	doc := statsExportDoc{Schema: statsExportSchema, Settings: statsExportSettings(s.Doc()), Weekly: []statsExportRow{}, Totals: []statsExportRow{}}
	// generated_at is a week too: a date and time would date the file more exactly than its rows.
	doc.GeneratedAt = statsWeekOf(s.Clock.Now().UTC())
	db, err := s.adminDB()
	if err != nil {
		return statsExportDoc{}, err
	}
	if !first.IsZero() {
		doc.Period = statsExportPeriod{From: statsWeekOf(first), To: statsWeekOf(last)}
		weekly, totals, err := s.statsExportRows(ctx, db.Reader(), first, last.AddDate(0, 0, 6))
		if err != nil {
			return statsExportDoc{}, err
		}
		doc.Weekly, doc.Totals = weekly, totals
	}
	if doc.Install, err = s.statsExportInstall(ctx, in.ResetID); err != nil {
		return statsExportDoc{}, err
	}
	return doc, nil
}

// statsExportWeeks cuts the requested period to whole ISO weeks (UTC): the Monday of the first week and
// the Monday of the last. Weeks that are not finished are left out. Both zero when no week is left.
func (s *Service) statsExportWeeks(in statsExportInput) (first, last time.Time, err error) {
	now := s.Clock.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	settled := today.AddDate(0, 0, -statsExportSettleDays)
	lastSunday := settled.AddDate(0, 0, -int(settled.Weekday()))
	var from, to time.Time
	if in.To != "" {
		if to, err = time.Parse(time.DateOnly, in.To); err != nil {
			return first, last, statsExportBadInput("to must be a date, YYYY-MM-DD")
		}
	}
	if in.From != "" {
		if from, err = time.Parse(time.DateOnly, in.From); err != nil {
			return first, last, statsExportBadInput("from must be a date, YYYY-MM-DD")
		}
	}
	lastMonday := lastSunday.AddDate(0, 0, -6)
	if !to.IsZero() {
		if m := statsMonday(to); m.Before(lastMonday) {
			lastMonday = m
		}
	}
	firstMonday := lastMonday.AddDate(0, 0, -7*(statsExportDefaultWeeks-1))
	if !from.IsZero() {
		firstMonday = statsMonday(from)
	}
	if !to.IsZero() && !from.IsZero() && to.Before(from) {
		return first, last, statsExportBadInput("to is before from")
	}
	if firstMonday.After(lastMonday) {
		return time.Time{}, time.Time{}, nil
	}
	if weeks := int(lastMonday.Sub(firstMonday).Hours()/24/7) + 1; weeks > statsExportMaxWeeks {
		firstMonday = lastMonday.AddDate(0, 0, -7*(statsExportMaxWeeks-1))
	}
	return firstMonday, lastMonday, nil
}

// statsMonday is the Monday of t's ISO week (UTC date).
func statsMonday(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// statsWeekOf is the ISO week label ("2026-W47") of a date, in UTC.
func statsWeekOf(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// statsExportCell is the total of one (week, metric, exported dimensions) before bucketing.
type statsExportCell struct {
	week   string
	metric *statsMetric
	dims   map[string]string
	n      int64
	vals   map[string]float64
	hist   []int64
}

func statsDimKey(m *statsMetric, dims map[string]string) string {
	var b strings.Builder
	for _, d := range m.Dims {
		b.WriteString(d.Key + "=" + dims[d.Key] + "|")
	}
	return b.String()
}

func statsMetricOrder(m *statsMetric) int {
	for i := range statsRegistry {
		if statsRegistry[i].Name == m.Name {
			return i
		}
	}
	return len(statsRegistry)
}

// memberFacingRow says whether a row (by its exported dimension values) is something a member did, and so
// is subject to the small-count fold.
func (p statsExportPolicy) memberFacingRow(dims map[string]string) bool {
	if !p.MemberFacing {
		return false
	}
	if len(p.MemberWhen) == 0 {
		return true
	}
	for k, vs := range p.MemberWhen {
		if slices.Contains(vs, dims[k]) {
			return true
		}
	}
	return false
}

func (p statsExportPolicy) skipsRow(dims map[string]string) bool {
	for k, vs := range p.SkipWhen {
		if slices.Contains(vs, dims[k]) {
			return true
		}
	}
	return false
}

// statsExportRows reads stats_daily between two days (inclusive) and builds the weekly and the total rows.
func (s *Service) statsExportRows(ctx context.Context, rdb *sql.DB, firstMonday, lastSunday time.Time) (weekly, totals []statsExportRow, err error) {
	rows, err := rdb.QueryContext(ctx, `SELECT day, metric, dims, n, vals FROM stats_daily WHERE day>=? AND day<=? ORDER BY day`,
		firstMonday.Format(time.DateOnly), lastSunday.Format(time.DateOnly))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	// per day first: several stored values (operator names) fold into one exported value
	type dayKey struct{ day, metric, dims string }
	days := map[dayKey]*statsExportCell{}
	var dayOrder []dayKey
	for rows.Next() {
		var day, metric, dims, rawVals string
		var n int64
		if err := rows.Scan(&day, &metric, &dims, &n, &rawVals); err != nil {
			return nil, nil, err
		}
		m, ok := s.statsLookup(metric)
		if !ok || !m.Export.Exported {
			continue
		}
		t, perr := time.Parse(time.DateOnly, day)
		if perr != nil || n < 0 {
			continue
		}
		ed := statsExportDims(m, statsDecodeDims(dims))
		if m.Export.skipsRow(ed) {
			continue
		}
		vals, hist, verr := statsDecodeVals(rawVals)
		if verr != nil {
			continue // a damaged cell is left out rather than guessed
		}
		k := dayKey{day, m.Name, statsDimKey(m, ed)}
		c := days[k]
		if c == nil {
			c = &statsExportCell{week: statsWeekOf(t), metric: m, dims: ed}
			days[k] = c
			dayOrder = append(dayOrder, k)
		}
		c.n += n
		c.vals = statsMergeVals(c.vals, statsDeclaredVals(m, vals))
		if m.Hist {
			c.hist = statsHistMerge(c.hist, hist)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// days of a week into the week (sum, or the largest day for a distinct count)
	type weekKey struct{ week, metric, dims string }
	byWeek := map[weekKey]*statsExportCell{}
	var weekOrder []weekKey
	for _, k := range dayOrder {
		d := days[k]
		wk := weekKey{d.week, d.metric.Name, k.dims}
		w := byWeek[wk]
		if w == nil {
			w = &statsExportCell{week: d.week, metric: d.metric, dims: d.dims}
			byWeek[wk] = w
			weekOrder = append(weekOrder, wk)
		}
		combineCells(w, d, d.metric.Combine)
	}
	// the small-count fold (H5 rule 5): a member-facing row under the threshold joins the metric's "other" row
	folded := map[weekKey]*statsExportCell{}
	var foldedOrder []weekKey
	put := func(wk weekKey, c *statsExportCell) {
		if folded[wk] == nil {
			folded[wk] = &statsExportCell{week: c.week, metric: c.metric, dims: c.dims}
			foldedOrder = append(foldedOrder, wk)
		}
		combineCells(folded[wk], c, c.metric.Combine)
	}
	for _, wk := range weekOrder {
		c := byWeek[wk]
		if c.metric.Export.memberFacingRow(c.dims) && c.n < statsExportMinEvents {
			other := make(map[string]string, len(c.metric.Dims))
			for _, d := range c.metric.Dims {
				other[d.Key] = statsOther
			}
			c = &statsExportCell{week: c.week, metric: c.metric, dims: other, n: c.n, vals: c.vals, hist: c.hist}
			wk = weekKey{c.week, c.metric.Name, statsDimKey(c.metric, other)}
		}
		put(wk, c)
	}
	// totals come from the folded weekly rows, so they say nothing the weekly rows do not
	type totalKey struct{ metric, dims string }
	tot := map[totalKey]*statsExportCell{}
	var totOrder []totalKey
	for _, wk := range foldedOrder {
		c := folded[wk]
		tk := totalKey{c.metric.Name, wk.dims}
		t := tot[tk]
		if t == nil {
			t = &statsExportCell{metric: c.metric, dims: c.dims}
			tot[tk] = t
			totOrder = append(totOrder, tk)
		}
		combineCells(t, c, c.metric.Combine)
	}
	for _, wk := range foldedOrder {
		if r, ok := statsExportRowOf(folded[wk]); ok {
			weekly = append(weekly, r)
		}
	}
	for _, tk := range totOrder {
		if r, ok := statsExportRowOf(tot[tk]); ok {
			totals = append(totals, r)
		}
	}
	order := func(rs []statsExportRow) {
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].Week != rs[j].Week {
				return rs[i].Week < rs[j].Week
			}
			mi, _ := s.statsLookup(rs[i].Metric)
			mj, _ := s.statsLookup(rs[j].Metric)
			if oi, oj := statsMetricOrder(mi), statsMetricOrder(mj); oi != oj {
				return oi < oj
			}
			return statsDimKey(mi, rs[i].Dims) < statsDimKey(mi, rs[j].Dims)
		})
	}
	order(weekly)
	order(totals)
	if weekly == nil {
		weekly = []statsExportRow{}
	}
	if totals == nil {
		totals = []statsExportRow{}
	}
	return weekly, totals, nil
}

// combineCells adds b into a: sums, or for a distinct count the largest n.
func combineCells(a, b *statsExportCell, how statsCombine) {
	if how == statsMax {
		if b.n > a.n {
			a.n = b.n
		}
	} else {
		a.n += b.n
	}
	a.vals = statsMergeVals(a.vals, b.vals)
	a.hist = statsHistMerge(a.hist, b.hist)
}

// statsDeclaredVals keeps the numbers a metric declares and nothing else.
func statsDeclaredVals(m *statsMetric, vals map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, k := range m.Vals {
		if v, ok := vals[k]; ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
			out[k] = v
		}
	}
	return out
}

// statsExportRowOf buckets a cell into its file row (false for an empty one).
func statsExportRowOf(c *statsExportCell) (statsExportRow, bool) {
	m := c.metric
	r := statsExportRow{Week: c.week, Metric: m.Name, Dims: c.dims}
	if len(m.Dims) == 0 {
		r.Dims = nil
	}
	switch m.Export.Bucket {
	case statsBucketSig1:
		r.N = statsRoundSig1(c.n)
	case statsBucketRange:
		r.Range = statsRangeLabel(c.n)
	default:
		r.N = c.n
	}
	if m.Export.Bucket == statsBucketNone {
		vals := map[string]float64{}
		for k, v := range c.vals {
			if v = math.Round(v*1e6) / 1e6; v != 0 {
				vals[k] = v
			}
		}
		if len(vals) > 0 {
			r.Vals = vals
		}
		var total int64
		for _, h := range c.hist {
			total += h
		}
		if total > 0 && len(c.hist) == statsHistLen {
			r.Hist = slices.Clone(c.hist)
		}
	}
	if r.N == 0 && r.Range == "" && len(r.Vals) == 0 && len(r.Hist) == 0 {
		return statsExportRow{}, false
	}
	return r, true
}

// statsExportInstall is the install block. The id is made at the first export (or replaced on request).
func (s *Service) statsExportInstall(ctx context.Context, reset bool) (statsExportInstall, error) {
	s.stateLocks.statsInstall.Lock()
	defer s.stateLocks.statsInstall.Unlock()
	var st statsInstallState
	if _, err := s.kvGet(statsInstallKey, &st); err != nil {
		return statsExportInstall{}, err
	}
	changed := false
	if st.ID == "" || reset {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return statsExportInstall{}, err
		}
		st.ID, changed = hex.EncodeToString(b[:]), true
	}
	if seen, err := s.statsFirstSeen(ctx); err != nil {
		return statsExportInstall{}, err
	} else if seen != "" && (st.FirstSeen == "" || seen < st.FirstSeen) {
		st.FirstSeen, changed = seen, true
	}
	if changed {
		if err := s.kvSet(statsInstallKey, st); err != nil {
			return statsExportInstall{}, err
		}
	}
	return statsExportInstall{ID: st.ID, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Tier: hostprofile.Current().Tier, FirstSeen: st.FirstSeen}, nil
}

// statsFirstSeen is the week of the oldest statistics day or the oldest turn ("" when there is none).
func (s *Service) statsFirstSeen(ctx context.Context) (string, error) {
	db, err := s.adminDB()
	if err != nil {
		return "", err
	}
	oldest := ""
	for _, q := range []string{`SELECT day FROM stats_daily ORDER BY day LIMIT 1`, `SELECT day FROM turn_log ORDER BY day LIMIT 1`} {
		var day string
		switch err := db.Reader().QueryRowContext(ctx, q).Scan(&day); {
		case err == sql.ErrNoRows:
			continue
		case err != nil:
			return "", err
		}
		if oldest == "" || day < oldest {
			oldest = day
		}
	}
	t, err := time.Parse(time.DateOnly, oldest)
	if err != nil {
		return "", nil
	}
	return statsWeekOf(t), nil
}

// ---------------------------------------------------------------------------
// settings

var statsExportLocales = []string{"ko", "en", "ja", "zh-Hans", "es"}

// statsEnum is v when it is in the fixed list, else "other": a typo or a name never reaches the file.
func statsEnum(v string, allowed []string) string {
	v = strings.TrimSpace(v)
	if slices.Contains(allowed, v) {
		return v
	}
	if v == "" {
		return "unset"
	}
	return statsOther
}

// statsCountRange is a count as "0" or a range (review H5 rule 3).
func statsCountRange(n int) string {
	if n <= 0 {
		return "0"
	}
	return statsRangeLabel(int64(n))
}

// statsDaysBucket is a retention horizon in days: forever, or a coarse band.
func statsDaysBucket(d int) string {
	switch {
	case d < 0:
		return "forever"
	case d == 0:
		return "default"
	case d <= 30:
		return "30d"
	case d <= 90:
		return "90d"
	case d <= 400:
		return "400d"
	}
	return "over_400d"
}

// statsExportSettings describes how this install is set up: booleans, enumerations from fixed lists and
// bucketed counts. No name, hostname, URL, key, persona text, timezone, Discord ID or room count.
func statsExportSettings(doc config.Document) map[string]any {
	families := map[string]bool{}
	for key := range doc.Providers {
		families[statsProviderFamily(doc, key)] = true
	}
	famList := make([]string, 0, len(families))
	for f := range families {
		famList = append(famList, f)
	}
	sort.Strings(famList)
	customTools := 0
	for _, ts := range doc.Tools {
		if ts.HTTP != nil {
			customTools++
		}
	}
	return map[string]any{
		"stats_enabled":         doc.Stats.On(),
		"locale":                statsEnum(doc.I18n.DefaultLocale, statsExportLocales),
		"polite_default":        doc.I18n.PoliteDefault,
		"memory_capture":        doc.Memory.Capture,
		"memory_embed":          doc.Memory.Embed,
		"memory_summaries":      doc.Memory.Summaries,
		"memory_retrieve":       statsEnum(doc.Memory.Retrieve, config.RetrieveModes),
		"participation_enabled": doc.Participation.Enabled,
		"participation_mode":    statsEnum(doc.Participation.Mode, config.ParticipationModes),
		"chat_tool_mode":        statsEnum(doc.Chat.ToolMode, config.ToolModes),
		"chat_vision":           statsEnum(doc.Chat.Vision, config.VisionModes),
		"dm_allow":              statsEnum(doc.DM.AllowMode(), config.DMAllowModes),
		"backup_schedule":       statsEnum(doc.Backup.Schedule, config.BackupSchedules),
		"updates_check":         doc.Updates.CheckOn(),
		"alerts_discord":        doc.Alerts.DiscordOn(),
		"servers":               statsCountRange(len(doc.Guilds)),
		"personas":              statsCountRange(len(doc.Personas)),
		"providers":             statsCountRange(len(doc.Providers)),
		"provider_families":     famList,
		"custom_http_tools":     statsCountRange(customTools),
		"retention_turns":       statsDaysBucket(doc.Retention.TurnDays),
		"retention_turn_detail": statsDaysBucket(doc.Retention.TurnDetailDays),
		"retention_ledger":      statsDaysBucket(doc.Retention.LedgerDays),
	}
}

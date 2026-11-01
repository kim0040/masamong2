package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/features/notices"
	"masamong/internal/features/privacy"
	"masamong/internal/tools"
)

// School and transfer admission notices (19장 A23–A26, V1 school_notice and transfer_notice): the bot
// reads public university boards listed in a data file (internal/features/notices/catalog.json, or
// <data-root>/notices/catalog.json to change it), and sends each subscriber, once a day at their time,
// the new notices that concern them.
//
// What changed from V1: one engine reads both kinds of board; only boards someone subscribed to are
// read (and every board the operator asks for from the console); registering a school profile is one
// command with fields instead of a conversation with a model; delivery follows the person's time zone
// and quiet hours. As in V1: the boards are only asked for public list and notice pages (never a
// subscriber's data), robots.txt is honored, a person's profile is used only under the current consent,
// and transfer notices use no model at all.
//
// Knobs (tools.notices.status.defaults): collect_every_hours (6), max_requests (200), host_interval_ms
// (1000), max_details (40), user_agent; (tools.notices.subscribe.defaults): delivery_time ("08:00"),
// min_score (40), max_items (8), first_days (7).

func init() {
	featureDDL = append(featureDDL,
		`CREATE TABLE IF NOT EXISTS notice_state (
			source_id TEXT PRIMARY KEY,
			last_run TEXT NOT NULL,
			ok INTEGER NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			items INTEGER NOT NULL DEFAULT 0,
			new_items INTEGER NOT NULL DEFAULT 0,
			warnings TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS notice_items (
			item_key TEXT PRIMARY KEY,
			source_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			title TEXT NOT NULL,
			url TEXT NOT NULL,
			published TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL DEFAULT '',
			pinned INTEGER NOT NULL DEFAULT 0,
			fingerprint TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			signals TEXT NOT NULL DEFAULT '',
			first_seen TEXT NOT NULL,
			changed_at TEXT NOT NULL,
			detail_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notice_items_source ON notice_items(source_id, changed_at)`,
		`CREATE TABLE IF NOT EXISTS notice_subs (
			user_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			profile TEXT NOT NULL DEFAULT '',
			sources TEXT NOT NULL DEFAULT '',
			delivery_time TEXT NOT NULL DEFAULT '',
			active INTEGER NOT NULL DEFAULT 1,
			last_delivery TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(user_id, kind)
		)`,
		`CREATE TABLE IF NOT EXISTS notice_delivered (
			user_id TEXT NOT NULL,
			item_key TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			delivered_at TEXT NOT NULL,
			PRIMARY KEY(user_id, item_key)
		)`,
		`CREATE TABLE IF NOT EXISTS notice_delivery_outcomes (
			user_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			delivery_date TEXT NOT NULL,
			state TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(user_id, kind, delivery_date)
		)`)
}

var (
	catalogMu    sync.Mutex
	catalogCache struct {
		path   string
		mod    time.Time
		cat    *notices.Catalog
		origin string
	}
)

func (s *Service) noticeCatalogPath() string {
	return filepath.Join(s.DataRoot, "notices", "catalog.json")
}

// noticeCatalog is the board catalog: the instance's file when there is one, else the built-in one.
func (s *Service) noticeCatalog() (*notices.Catalog, string, error) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	path := s.noticeCatalogPath()
	var mod time.Time
	if fi, err := os.Stat(path); err == nil {
		mod = fi.ModTime()
	}
	if catalogCache.cat != nil && catalogCache.path == path && catalogCache.mod.Equal(mod) {
		return catalogCache.cat, catalogCache.origin, nil
	}
	c, origin, err := notices.Load(path)
	if err != nil {
		return nil, "", err
	}
	catalogCache.path, catalogCache.mod, catalogCache.cat, catalogCache.origin = path, mod, c, origin
	return c, origin, nil
}

// noticeSub is one person's subscription of one kind.
type noticeSub struct {
	UserID, Kind  string
	Profile       notices.Profile
	Sources       []string // transfer: the chosen boards
	DeliveryTime  string
	Active        bool
	LastDelivery  string
	CreatedAt     string
	consentScope  string
	profileExists bool
}

func noticeScope(kind string) string {
	if kind == notices.KindTransfer {
		return privacy.TransferNotice
	}
	return privacy.SchoolNotice
}

func (s *Service) noticeSub(userID, kind string) (noticeSub, bool) {
	sub := noticeSub{UserID: userID, Kind: kind, consentScope: noticeScope(kind)}
	db, err := s.adminDB()
	if err != nil {
		return sub, false
	}
	var profile, sources string
	var active int
	err = db.DB.QueryRow(`SELECT profile, sources, delivery_time, active, last_delivery, created_at FROM notice_subs WHERE user_id=? AND kind=?`, userID, kind).
		Scan(&profile, &sources, &sub.DeliveryTime, &active, &sub.LastDelivery, &sub.CreatedAt)
	if err != nil {
		return sub, false
	}
	_ = json.Unmarshal([]byte(profile), &sub.Profile)
	_ = json.Unmarshal([]byte(sources), &sub.Sources)
	sub.Active, sub.profileExists = active == 1, true
	return sub, true
}

func (s *Service) saveNoticeSub(sub noticeSub) error {
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	profile, _ := json.Marshal(sub.Profile)
	sources, _ := json.Marshal(sub.Sources)
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	_, err = db.DB.Exec(`INSERT INTO notice_subs(user_id, kind, profile, sources, delivery_time, active, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(user_id, kind) DO UPDATE SET profile=excluded.profile, sources=excluded.sources, delivery_time=excluded.delivery_time,
		active=excluded.active, updated_at=excluded.updated_at`, sub.UserID, sub.Kind, string(profile), string(sources), sub.DeliveryTime, b2i(sub.Active), now, now)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Collecting
// ---------------------------------------------------------------------------

var noticesRunning, noticesTicking atomic.Bool

// neededSources are the boards someone subscribed to (active, with consent).
func (s *Service) neededSources(c *notices.Catalog) map[string]bool {
	need := map[string]bool{}
	db, err := s.adminDB()
	if err != nil {
		return need
	}
	rows, err := db.DB.Query(`SELECT user_id, kind, profile, sources FROM notice_subs WHERE active=1`)
	if err != nil {
		return need
	}
	type row struct{ user, kind, profile, sources string }
	var subs []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.user, &r.kind, &r.profile, &r.sources) == nil {
			subs = append(subs, r)
		}
	}
	rows.Close()
	for _, r := range subs {
		if !s.ConsentAllows(r.user, noticeScope(r.kind)) {
			continue
		}
		if r.kind == notices.KindTransfer {
			var ids []string
			_ = json.Unmarshal([]byte(r.sources), &ids)
			for _, id := range ids {
				need[id] = true
			}
			continue
		}
		var p notices.Profile
		_ = json.Unmarshal([]byte(r.profile), &p)
		for _, src := range c.Sources {
			if src.Kind == notices.KindSchool && src.School == p.School {
				need[src.ID] = true
			}
		}
	}
	return need
}

// CollectReport is one collection run.
type CollectReport struct {
	At       string            `json:"at"`
	Sources  int               `json:"sources"`
	New      int               `json:"new"`
	Changed  int               `json:"changed"`
	Details  int               `json:"details"`
	Failures map[string]string `json:"failures,omitempty"`
}

// collectNotices reads the boards (the ones given, else the ones someone needs) and stores what is new.
func (s *Service) collectNotices(ctx context.Context, only []string) (CollectReport, error) {
	rep := CollectReport{At: s.Clock.Now().UTC().Format(time.RFC3339), Failures: map[string]string{}}
	if !noticesRunning.CompareAndSwap(false, true) {
		return rep, errors.New("a collection is already running")
	}
	defer noticesRunning.Store(false)
	c, _, err := s.noticeCatalog()
	if err != nil {
		return rep, err
	}
	want := s.neededSources(c)
	if len(only) > 0 {
		want = map[string]bool{}
		for _, id := range only {
			want[id] = true
		}
	}
	f := &notices.Fetcher{
		UserAgent:    s.toolString("notices.status", "user_agent", "Mozilla/5.0 (compatible; masamong/"+Version+"; +public-notices)"),
		MaxRequests:  s.toolDefault("notices.status", "max_requests", 200),
		HostInterval: time.Duration(s.toolDefault("notices.status", "host_interval_ms", 1000)) * time.Millisecond,
		AllowPrivate: s.noticeFetchPrivate,
	}
	details := s.toolDefault("notices.status", "max_details", 40)
	db, err := s.adminDB()
	if err != nil {
		return rep, err
	}
	for _, src := range c.Sources {
		if !want[src.ID] || src.Manual || ctx.Err() != nil {
			continue
		}
		rep.Sources++
		now := s.Clock.Now().UTC().Format(time.RFC3339)
		html, err := f.Get(ctx, src, src.ListURL)
		var items []notices.Item
		var warnings []string
		if err == nil {
			items, warnings = c.ParseList(html, src)
			if len(items) < src.MinItems && len(warnings) == 0 {
				warnings = append(warnings, fmt.Sprintf("only %d notices (expected at least %d)", len(items), src.MinItems))
			}
		}
		newN := 0
		for _, it := range items {
			key := it.SourceID + "|" + it.ExternalID
			var fp string
			found := db.DB.QueryRow(`SELECT fingerprint FROM notice_items WHERE item_key=?`, key).Scan(&fp) == nil
			switch {
			case !found:
				newN++
				rep.New++
				_, _ = db.DB.Exec(`INSERT INTO notice_items(item_key, source_id, kind, title, url, published, category, pinned, fingerprint, first_seen, changed_at)
					VALUES(?,?,?,?,?,?,?,?,?,?,?)`, key, src.ID, src.Kind, it.Title, it.URL, it.Published, it.Category, b2i(it.Pinned), it.Fingerprint, now, now)
			case fp != it.Fingerprint:
				rep.Changed++
				_, _ = db.DB.Exec(`UPDATE notice_items SET title=?, url=?, published=?, category=?, pinned=?, fingerprint=?, changed_at=?, detail_at='' WHERE item_key=?`,
					it.Title, it.URL, it.Published, it.Category, b2i(it.Pinned), it.Fingerprint, now, key)
			default:
				_, _ = db.DB.Exec(`UPDATE notice_items SET pinned=? WHERE item_key=?`, b2i(it.Pinned), key)
			}
		}
		// the notice pages of what is new or changed, within the run's budget
		if src.Kind == notices.KindSchool || len(src.Detail.BodySelectors) > 0 {
			rows, qerr := db.DB.Query(`SELECT item_key, url, title FROM notice_items WHERE source_id=? AND detail_at='' ORDER BY changed_at DESC LIMIT ?`, src.ID, max(0, details-rep.Details))
			type pending struct{ key, url, title string }
			var todo []pending
			if qerr == nil {
				for rows.Next() {
					var p pending
					if rows.Scan(&p.key, &p.url, &p.title) == nil {
						todo = append(todo, p)
					}
				}
				rows.Close()
			}
			for _, p := range todo {
				page, ferr := f.Get(ctx, src, p.url)
				if errors.Is(ferr, notices.ErrBudget) || ctx.Err() != nil {
					break
				}
				d := notices.Detail{}
				if ferr == nil {
					d = c.ParseDetail(page, src)
				}
				sig := c.Signals(p.title, d.Body)
				raw, _ := json.Marshal(sig)
				_, _ = db.DB.Exec(`UPDATE notice_items SET body=?, signals=?, detail_at=?, published=CASE WHEN published='' THEN ? ELSE published END WHERE item_key=?`,
					clip(d.Body, 6000), string(raw), s.Clock.Now().UTC().Format(time.RFC3339), d.Published, p.key)
				rep.Details++
			}
		}
		ok := err == nil
		msg := ""
		if err != nil {
			msg = clip(err.Error(), 300)
			rep.Failures[src.ID] = msg
		}
		_, _ = db.DB.Exec(`INSERT INTO notice_state(source_id, last_run, ok, error, items, new_items, warnings) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(source_id) DO UPDATE SET last_run=excluded.last_run, ok=excluded.ok, error=excluded.error, items=excluded.items,
			new_items=excluded.new_items, warnings=excluded.warnings`, src.ID, now, b2i(ok), msg, len(items), newN, strings.Join(warnings, "; "))
		if errors.Is(err, notices.ErrBudget) {
			break
		}
	}
	_ = s.kvSet("notices:last_collect", rep)
	if rep.New+rep.Changed > 0 {
		s.emit("notices", "collected", map[string]any{"new": rep.New})
	}
	return rep, nil
}

// toolString reads a text knob from tools.<tool>.defaults.
func (s *Service) toolString(tool, key, def string) string {
	if v, ok := s.Doc().Tools[tool].Defaults[key].(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// noticesTick runs the collection every collect_every_hours and the deliveries that are due.
func (s *Service) noticesTick(ctx context.Context) {
	if r, found := tools.Find(s.toolRegistry(), "notices.status"); found && !r.Enabled {
		return
	}
	var last CollectReport
	_, _ = s.kvGet("notices:last_collect", &last)
	every := time.Duration(max(1, s.toolDefault("notices.status", "collect_every_hours", 6))) * time.Hour
	if at, err := time.Parse(time.RFC3339, last.At); err != nil || s.Clock.Now().Sub(at) >= every {
		if rep, err := s.collectNotices(ctx, nil); err == nil && rep.Sources > 0 {
			s.Log.Info("notices: collected", "boards", rep.Sources, "new", rep.New, "changed", rep.Changed, "failures", len(rep.Failures))
		}
	}
	s.deliverNotices(ctx)
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

type rankedNotice struct {
	Key, Title, URL, Published, Fingerprint, University, Board string
	Score                                                      notices.Scored
}

// candidates are the notices of a subscription not sent yet, ranked, newest first among equals.
func (s *Service) candidates(c *notices.Catalog, sub noticeSub, includeSent bool) []rankedNotice {
	db, err := s.adminDB()
	if err != nil {
		return nil
	}
	var srcIDs []string
	if sub.Kind == notices.KindTransfer {
		srcIDs = sub.Sources
	} else {
		for _, src := range c.Sources {
			if src.Kind == notices.KindSchool && src.School == sub.Profile.School {
				srcIDs = append(srcIDs, src.ID)
			}
		}
	}
	if len(srcIDs) == 0 {
		return nil
	}
	since := s.Clock.Now().AddDate(0, 0, -s.toolDefault("notices.subscribe", "first_days", 7)).UTC().Format(time.RFC3339)
	if sub.LastDelivery != "" && !includeSent {
		// after the first delivery only what changed since subscribing is new to them
		if sub.CreatedAt > since {
			since = sub.CreatedAt
		}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(srcIDs)), ",")
	args := []any{}
	for _, id := range srcIDs {
		args = append(args, id)
	}
	args = append(args, since)
	rows, err := db.DB.Query(`SELECT item_key, source_id, title, url, published, pinned, fingerprint, body, signals FROM notice_items
		WHERE source_id IN (`+ph+`) AND changed_at >= ? ORDER BY changed_at DESC LIMIT 200`, args...)
	if err != nil {
		return nil
	}
	type row struct {
		key, src, title, url, pub, fp, body, sig string
		pinned                                   int
	}
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.key, &r.src, &r.title, &r.url, &r.pub, &r.pinned, &r.fp, &r.body, &r.sig) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	sent := map[string]string{}
	if !includeSent {
		if drows, err := db.DB.Query(`SELECT item_key, fingerprint FROM notice_delivered WHERE user_id=?`, sub.UserID); err == nil {
			for drows.Next() {
				var k, fp string
				if drows.Scan(&k, &fp) == nil {
					sent[k] = fp
				}
			}
			drows.Close()
		}
	}
	today := s.Clock.Now().In(s.userLocation(s.userPrefs(sub.UserID)))
	minScore := float64(s.toolDefault("notices.subscribe", "min_score", 40))
	var out []rankedNotice
	for _, r := range list {
		if fp, ok := sent[r.key]; ok && fp == r.fp {
			continue
		}
		src, _ := c.Source(r.src)
		rn := rankedNotice{Key: r.key, Title: r.title, URL: r.url, Published: r.pub, Fingerprint: r.fp, University: src.University, Board: src.Board}
		if sub.Kind == notices.KindSchool {
			var sig notices.Signals
			if err := json.Unmarshal([]byte(r.sig), &sig); err != nil {
				sig = c.Signals(r.title, r.body)
			}
			rn.Score = c.Score(sub.Profile, src, notices.Item{Title: r.title, Pinned: r.pinned == 1}, sig, r.body, today)
			if rn.Score.Band == "hidden" || rn.Score.Score < minScore {
				continue
			}
		}
		out = append(out, rn)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score.Score > out[j].Score.Score })
	return out[:min(len(out), s.toolDefault("notices.subscribe", "max_items", 8))]
}

var bandIcon = map[string]string{"action": "⏰", "opportunity": "✨", "reference": "📄"}

// noticeBands is the order school notices are grouped in: what to do first, then chances, then the rest.
var noticeBands = []string{"action", "opportunity", "reference"}

// noticeCard renders ranked notices (docs/plan-discord-experience.md §3.5): school notices grouped by how
// much they ask of the reader, transfer notices by university, each item a linked title and one line that
// starts with the deadline when there is one. today (the reader's day) counts the days left; loc words
// the headings and reasons. Groups are separated by a blank line, which is where a long list is split.
func (s *Service) noticeCard(loc, title, kind string, list []rankedNotice, c *notices.Catalog, today time.Time) action.Card {
	tone := "school"
	if kind == notices.KindTransfer {
		tone = "transfer"
	}
	card := action.Card{Title: title, Tone: tone}
	universities := map[string]bool{}
	for _, n := range list {
		universities[n.University] = true
	}
	type group struct {
		heading string
		items   []rankedNotice
	}
	var groups []group
	if kind == notices.KindTransfer {
		at := map[string]int{}
		for _, n := range list {
			i, ok := at[n.University]
			if !ok {
				i = len(groups)
				at[n.University] = i
				groups = append(groups, group{})
			}
			groups[i].items = append(groups[i].items, n)
		}
		for i := range groups {
			name := groups[i].items[0].University
			if name == "" {
				name = s.I18n.T(loc, "notices.group.other")
			}
			groups[i].heading = "🎓 " + s.I18n.T(loc, "notices.group.count", "name", name, "n", len(groups[i].items))
		}
	} else {
		for _, band := range append(noticeBands, "") {
			var g group
			for _, n := range list {
				if n.Score.Band == band || band == "" && bandIcon[n.Score.Band] == "" {
					g.items = append(g.items, n)
				}
			}
			if len(g.items) == 0 {
				continue
			}
			name := s.I18n.T(loc, "notices.group.other")
			icon := "•"
			if band != "" {
				name, icon = s.I18n.T(loc, "notices.band."+band), bandIcon[band]
			}
			g.heading = icon + " " + s.I18n.T(loc, "notices.group.count", "name", name, "n", len(g.items))
			groups = append(groups, g)
		}
	}
	var lines []string
	for _, g := range groups {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "**"+g.heading+"**")
		for _, n := range g.items {
			link := "[" + mdEscape(clip(n.Title, 120)) + "](" + n.URL + ")"
			if n.Score.Band == "action" {
				link = "**" + link + "**"
			}
			lines = append(lines, link)
			if meta := s.noticeMeta(loc, kind, n, c, today, len(universities) > 1); meta != "" {
				lines = append(lines, meta)
			}
		}
	}
	card.Text = strings.Join(lines, "\n")
	return card
}

// noticeMeta is the line under a notice: the deadline with the days left, or else the posting date; then
// the university when the list mixes them (a transfer list is grouped by it already) and the first reason
// the notice is for this reader.
func (s *Service) noticeMeta(loc, kind string, n rankedNotice, c *notices.Catalog, today time.Time, mixed bool) string {
	var meta []string
	if d, err := time.Parse("2006-01-02", n.Score.Deadline); err == nil {
		days := int(d.Sub(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
		left := s.I18n.T(loc, "notices.due_in", "n", days)
		if days == 0 {
			left = s.I18n.T(loc, "notices.due_today")
		}
		if days < 0 {
			meta = append(meta, s.I18n.T(loc, "notices.deadline", "date", s.shortDateOf(loc, n.Score.Deadline, today)))
		} else {
			meta = append(meta, s.I18n.T(loc, "notices.due", "date", s.shortDateOf(loc, n.Score.Deadline, today), "left", left))
		}
	} else if n.Score.Deadline != "" {
		meta = append(meta, s.I18n.T(loc, "notices.deadline", "date", n.Score.Deadline))
	} else if n.Published != "" {
		meta = append(meta, s.shortDateOf(loc, n.Published, today))
	}
	if mixed && kind != notices.KindTransfer && n.University != "" {
		meta = append(meta, n.University)
	}
	for _, r := range n.Score.Reasons {
		if r.Key == "interest" || r.Key == "my_department" || r.Key == "for_degree" {
			arg := r.Arg
			if r.Key == "interest" {
				arg = c.TopicLabel(r.Arg)
			} else if r.Key == "for_degree" {
				arg = s.I18n.T(loc, "notices.degree."+r.Arg)
			}
			meta = append(meta, s.I18n.T(loc, "notices.reason."+r.Key, "arg", arg))
			break
		}
	}
	return strings.Join(meta, " · ")
}

// shortDateOf is a YYYY-MM-DD date as "10/20", with the year when it is not today's year; any other
// text is kept as it is.
func (s *Service) shortDateOf(loc, raw string, today time.Time) string {
	d, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return raw
	}
	if d.Year() != today.Year() {
		return raw
	}
	return s.shortDate(loc, d)
}

var mdSpecial = strings.NewReplacer("[", "(", "]", ")", "*", "\\*", "_", "\\_", "`", "'")

func mdEscape(s string) string { return mdSpecial.Replace(s) }

// deliverNotices sends each subscriber whose time has come what is new for them (once a day).
func (s *Service) deliverNotices(ctx context.Context) {
	sender, ok := s.Discord.(DiscordSender)
	if s.Discord == nil || !ok || !s.Discord.Connected() {
		return
	}
	c, _, err := s.noticeCatalog()
	if err != nil {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	rows, err := db.DB.Query(`SELECT user_id, kind FROM notice_subs WHERE active=1`)
	if err != nil {
		return
	}
	type key struct{ user, kind string }
	var subs []key
	for rows.Next() {
		var k key
		if rows.Scan(&k.user, &k.kind) == nil {
			subs = append(subs, k)
		}
	}
	rows.Close()
	for _, k := range subs {
		if ctx.Err() != nil {
			return
		}
		sub, ok := s.noticeSub(k.user, k.kind)
		if !ok || !s.ConsentAllows(k.user, sub.consentScope) {
			continue
		}
		prefs := s.userPrefs(k.user)
		zone := s.userLocation(prefs)
		now := s.Clock.Now().In(zone)
		at := sub.DeliveryTime
		if at == "" {
			at = s.toolString("notices.subscribe", "delivery_time", "08:00")
		}
		t, err := time.Parse("15:04", at)
		if err != nil {
			continue
		}
		due, date := noticeSlot(now, t, sub.LastDelivery, sub.CreatedAt, func(slot time.Time) time.Time { return s.quietUntil(prefs, slot) })
		if sub.LastDelivery == date || now.Before(s.quietUntil(prefs, due)) {
			continue
		}
		list := s.candidates(c, sub, false)
		if len(list) > 0 {
			stamp := s.Clock.Now().UTC().Format(time.RFC3339)
			state, updated := "", ""
			if err := db.DB.QueryRow(`SELECT state, updated_at FROM notice_delivery_outcomes WHERE user_id=? AND kind=? AND delivery_date=?`, k.user, k.kind, date).Scan(&state, &updated); err == nil {
				if state == "sending" {
					at, err := time.Parse(time.RFC3339, updated)
					if err == nil && s.Clock.Now().UTC().Before(at.Add(time.Minute)) {
						continue
					}
					// A send interrupted beyond its lease window is ambiguous; settle it without resending.
					_ = db.WithWriter(func(tx *sql.Tx) error {
						if _, err := tx.Exec(`UPDATE notice_delivery_outcomes SET state='unknown', updated_at=? WHERE user_id=? AND kind=? AND delivery_date=? AND state='sending'`, stamp, k.user, k.kind, date); err != nil {
							return err
						}
						_, err := tx.Exec(`UPDATE notice_subs SET last_delivery=? WHERE user_id=? AND kind=?`, date, k.user, k.kind)
						return err
					})
					continue
				}
			} else if !isNoRows(err) {
				s.Log.Warn("notices: delivery outcome read failed", "kind", k.kind, "err", err)
				continue
			}
			if state == "sent" || state == "unknown" || state == "closed" || state == "sending" {
				continue
			}
			claimed, err := db.DB.Exec(`INSERT INTO notice_delivery_outcomes(user_id, kind, delivery_date, state, updated_at) VALUES(?,?,?,'sending',?)
				ON CONFLICT(user_id, kind, delivery_date) DO UPDATE SET state='sending', updated_at=excluded.updated_at WHERE state='failed'`, k.user, k.kind, date, stamp)
			if err != nil {
				s.Log.Warn("notices: delivery claim failed", "kind", k.kind, "err", err)
				continue
			}
			claimedRows, err := claimed.RowsAffected()
			if err != nil || claimedRows != 1 {
				continue
			}
			loc := prefs.Locale
			if loc == "" {
				loc = s.Doc().I18n.DefaultLocale
			}
			day := now
			if d, err := time.ParseInLocation("2006-01-02", date, zone); err == nil {
				day = d
			}
			title := s.I18n.T(loc, "notices.digest_title."+sub.Kind, "n", len(list), "date", s.dayLabel(loc, day))
			card := s.noticeCard(loc, title, sub.Kind, list, c, now)
			if sub.Kind == notices.KindTransfer {
				card.Text += "\n\n*" + s.I18n.T(loc, "notices.disclaimer.transfer") + "*"
			}
			card.Footer = s.I18n.T(loc, "notices.digest_footer."+sub.Kind)
			card.Timestamp = s.Clock.Now().UTC().Format(time.RFC3339)
			card.Buttons = []action.CardButton{{ID: "notice:pause:" + sub.Kind, Label: s.I18n.T(loc, "notices.button.pause"), Style: "secondary"}}
			sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err = sender.SendDM(sendCtx, k.user, card, "")
			cancel()
			if err != nil {
				s.Log.Warn("notices: digest not sent", "kind", sub.Kind, "err", err)
				state := "unknown"
				settleDate := true
				disable := false
				if errors.Is(err, ErrNotDelivered) {
					state, settleDate = "failed", false
				} else if errors.Is(err, ErrDMClosed) {
					state, disable = "closed", true
				}
				_ = db.WithWriter(func(tx *sql.Tx) error {
					if _, err := tx.Exec(`UPDATE notice_delivery_outcomes SET state=?, updated_at=? WHERE user_id=? AND kind=? AND delivery_date=?`, state, stamp, k.user, k.kind, date); err != nil {
						return err
					}
					if settleDate {
						if _, err := tx.Exec(`UPDATE notice_subs SET last_delivery=? WHERE user_id=? AND kind=?`, date, k.user, k.kind); err != nil {
							return err
						}
					}
					if disable {
						_, err := tx.Exec(`UPDATE notice_subs SET active=0 WHERE user_id=? AND kind=?`, k.user, k.kind)
						return err
					}
					return nil
				})
				continue
			}
			nowText := s.Clock.Now().UTC().Format(time.RFC3339)
			_ = db.WithWriter(func(tx *sql.Tx) error {
				for _, n := range list {
					if _, err := tx.Exec(`INSERT INTO notice_delivered(user_id, item_key, fingerprint, delivered_at) VALUES(?,?,?,?)
						ON CONFLICT(user_id, item_key) DO UPDATE SET fingerprint=excluded.fingerprint, delivered_at=excluded.delivered_at`, k.user, n.Key, n.Fingerprint, nowText); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(`UPDATE notice_subs SET last_delivery=? WHERE user_id=? AND kind=?`, date, k.user, k.kind); err != nil {
					return err
				}
				_, err := tx.Exec(`UPDATE notice_delivery_outcomes SET state='sent', updated_at=? WHERE user_id=? AND kind=? AND delivery_date=?`, nowText, k.user, k.kind, date)
				return err
			})
			continue
		}
		_, _ = db.DB.Exec(`UPDATE notice_subs SET last_delivery=? WHERE user_id=? AND kind=?`, date, k.user, k.kind)
	}
}

// noticeSlot keeps an unsettled logical delivery date while a quiet window crosses midnight. Once
// today's due time has passed, missed older dates are drained oldest first.
func noticeSlot(now time.Time, at time.Time, last, createdAt string, quietUntil func(time.Time) time.Time) (time.Time, string) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	todayDue := time.Date(today.Year(), today.Month(), today.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	candidate := time.Time{}
	if last != "" {
		if previous, err := time.ParseInLocation("2006-01-02", last, now.Location()); err == nil {
			candidate = previous.AddDate(0, 0, 1)
		}
	} else if now.Before(todayDue) {
		candidate = today.AddDate(0, 0, -1)
		previousDue := time.Date(candidate.Year(), candidate.Month(), candidate.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
		created, err := time.Parse(time.RFC3339, createdAt)
		if err != nil || created.After(previousDue) {
			candidate = time.Time{}
		}
	}
	if !candidate.IsZero() && candidate.Before(today) {
		due := time.Date(candidate.Year(), candidate.Month(), candidate.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
		if !now.Before(quietUntil(due)) {
			return due, due.Format("2006-01-02")
		}
	}
	return todayDue, today.Format("2006-01-02")
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

func (s *Service) noticesStatus(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	c, origin, err := s.noticeCatalog()
	if err != nil {
		return action.Fail(req, errcode.InvalidInput, "notices.catalog_invalid", s.msg(req, "notices.catalog_invalid", "error", err.Error()), nil)
	}
	userID, private, fromDiscord := fortuneInvoker(req)
	if !fromDiscord {
		return s.noticesConsole(req, c, origin)
	}
	kind := strVal(in, "kind", notices.KindSchool)
	what := strVal(in, "what", "latest")
	title := s.msg(req, "notices.title."+kind)
	if !private {
		return action.WithCard(action.OK(req, map[string]any{"status": "private_only"}),
			action.Card{Title: title, Text: s.msg(req, "notices.private_only"), Tone: "neutral"})
	}
	if !s.ConsentAllows(userID, noticeScope(kind)) {
		res := s.consentNotice(req, userID, noticeScope(kind))
		cd := res.Output["card"].(action.Card)
		cd.Footer = s.msg(req, "fortune.consent_then")
		res = action.WithCard(res, cd)
		res.Output["status"] = "consent_required"
		return res
	}
	sub, ok := s.noticeSub(userID, kind)
	if !ok {
		return action.WithCard(action.OK(req, map[string]any{"status": "no_profile"}),
			action.Card{Title: title, Text: s.msg(req, "notices.register_first."+kind), Tone: "neutral", Ephemeral: true})
	}
	switch what {
	case "profile":
		return action.WithCard(action.OK(req, map[string]any{"status": "ok"}), s.subCard(req, c, sub))
	case "status":
		return action.WithCard(action.OK(req, map[string]any{"status": "ok"}), s.boardStatusCard(req, c, sub))
	}
	list := s.candidates(c, sub, true)
	if len(list) == 0 {
		return action.WithCard(action.OK(req, map[string]any{"status": "empty"}),
			action.Card{Title: title, Text: s.msg(req, "notices.none_yet"), Tone: "neutral", Ephemeral: true})
	}
	card := s.noticeCard(req.Locale, title, kind, list, c, s.Clock.Now().In(s.userLocation(s.userPrefs(userID))))
	card.Ephemeral = true
	return action.WithCard(action.OK(req, map[string]any{"status": "ok", "count": len(list)}), card)
}

func (s *Service) subCard(req action.Request, c *notices.Catalog, sub noticeSub) action.Card {
	loc := req.Locale
	at := sub.DeliveryTime
	if at == "" {
		at = s.toolString("notices.subscribe", "delivery_time", "08:00")
	}
	state := s.msg(req, "notices.state.on", "time", at, "zone", s.userLocation(s.userPrefs(sub.UserID)).String())
	if !sub.Active {
		state = s.msg(req, "notices.state.paused")
	}
	// the commands go in the text, where Discord shows them as code (a footer shows the backticks as they are)
	card := action.Card{Title: s.msg(req, "notices.profile_title."+sub.Kind), Tone: "neutral", Ephemeral: true, Text: s.msg(req, "notices.profile_help."+sub.Kind)}
	if sub.Kind == notices.KindTransfer {
		var names []string
		for _, id := range sub.Sources {
			if src, ok := c.Source(id); ok {
				names = append(names, src.University)
			}
		}
		card.Fields = []action.CardField{{Name: s.msg(req, "notices.field.universities"), Value: orDash(strings.Join(names, ", "))}, {Name: s.msg(req, "notices.field.delivery"), Value: state}}
		return card
	}
	p := sub.Profile
	grade := "-"
	if p.Grade > 0 {
		grade = strconv.Itoa(p.Grade)
	}
	topics := func(ids []string) string {
		var out []string
		for _, id := range ids {
			out = append(out, c.TopicLabel(id))
		}
		return orDash(strings.Join(out, ", "))
	}
	card.Fields = []action.CardField{
		{Name: s.msg(req, "notices.field.school"), Value: orDash(c.SchoolName(p.School)), Inline: true},
		{Name: s.msg(req, "notices.field.degree"), Value: s.I18n.T(loc, "notices.degree."+p.Degree), Inline: true},
		{Name: s.msg(req, "notices.field.grade"), Value: grade, Inline: true},
		{Name: s.msg(req, "notices.field.department"), Value: orDash(p.Department), Inline: true},
		{Name: s.msg(req, "notices.field.campus"), Value: orDash(p.Campus), Inline: true},
		{Name: s.msg(req, "notices.field.interests"), Value: topics(p.Interests), Inline: true},
		{Name: s.msg(req, "notices.field.muted"), Value: topics(p.Muted), Inline: true},
		{Name: s.msg(req, "notices.field.delivery"), Value: state},
	}
	return card
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

// boardStatusCard is when the person's boards were last read and whether that worked.
func (s *Service) boardStatusCard(req action.Request, c *notices.Catalog, sub noticeSub) action.Card {
	card := action.Card{Title: s.msg(req, "notices.status_title"), Tone: "neutral", Ephemeral: true}
	db, err := s.adminDB()
	if err != nil {
		return card
	}
	for _, src := range c.Sources {
		mine := (sub.Kind == notices.KindSchool && src.Kind == notices.KindSchool && src.School == sub.Profile.School) ||
			(sub.Kind == notices.KindTransfer && contains(sub.Sources, src.ID))
		if !mine {
			continue
		}
		name := strings.TrimSpace(src.University + " " + src.Board)
		if src.Manual {
			card.Fields = append(card.Fields, action.CardField{Name: name, Value: s.msg(req, "notices.manual", "url", src.OfficialURL)})
			continue
		}
		var last, errText string
		var ok, items int
		if db.DB.QueryRow(`SELECT last_run, ok, error, items FROM notice_state WHERE source_id=?`, src.ID).Scan(&last, &ok, &errText, &items) != nil {
			card.Fields = append(card.Fields, action.CardField{Name: name, Value: s.msg(req, "notices.not_read_yet")})
			continue
		}
		v := s.msg(req, "notices.read_ok", "n", items, "when", last)
		if ok == 0 {
			v = s.msg(req, "notices.read_failed", "when", last)
		}
		card.Fields = append(card.Fields, action.CardField{Name: name, Value: v})
	}
	return card
}

// noticesConsole is the console's view: the catalog, every board's last read, subscriber counts.
func (s *Service) noticesConsole(req action.Request, c *notices.Catalog, origin string) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	state := map[string]map[string]any{}
	if rows, err := db.DB.Query(`SELECT source_id, last_run, ok, error, items, new_items, warnings FROM notice_state`); err == nil {
		for rows.Next() {
			var id, last, e, w string
			var ok, items, newN int
			if rows.Scan(&id, &last, &ok, &e, &items, &newN, &w) == nil {
				state[id] = map[string]any{"last_run": last, "ok": ok == 1, "error": e, "items": items, "new": newN, "warnings": w}
			}
		}
		rows.Close()
	}
	need := s.neededSources(c)
	boards := []map[string]any{}
	for _, src := range c.Sources {
		b := map[string]any{"id": src.ID, "kind": src.Kind, "university": src.University, "board": src.Board, "school": src.School,
			"manual": src.Manual, "url": firstNonEmpty(src.ListURL, src.OfficialURL), "needed": need[src.ID], "note": src.Note}
		if st, ok := state[src.ID]; ok {
			b["state"] = st
		}
		boards = append(boards, b)
	}
	counts := map[string]int{}
	if rows, err := db.DB.Query(`SELECT kind, COUNT(*) FROM notice_subs WHERE active=1 GROUP BY kind`); err == nil {
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				counts[k] = n
			}
		}
		rows.Close()
	}
	var last CollectReport
	_, _ = s.kvGet("notices:last_collect", &last)
	return action.OK(req, map[string]any{"catalog": origin, "catalog_path": s.noticeCatalogPath(), "boards": boards, "subscribers": counts,
		"last_collect": last, "schools": c.Schools, "topics": c.Vocabulary.Topics})
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

// noticesSubscribe manages a person's subscription, or (console) collects now / lists subscribers.
func (s *Service) noticesSubscribe(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	c, _, err := s.noticeCatalog()
	if err != nil {
		return action.Fail(req, errcode.InvalidInput, "notices.catalog_invalid", s.msg(req, "notices.catalog_invalid", "error", err.Error()), nil)
	}
	kind := strVal(in, "kind", notices.KindSchool)
	op := strVal(in, "op", "status")
	userID, private, fromDiscord := fortuneInvoker(req)
	if !fromDiscord {
		return s.noticesSubscribeConsole(ctx, req, c, in, op)
	}
	title := s.msg(req, "notices.title."+kind)
	if !private {
		return action.WithCard(action.OK(req, map[string]any{"status": "private_only"}),
			action.Card{Title: title, Text: s.msg(req, "notices.private_only"), Tone: "neutral"})
	}
	scope := noticeScope(kind)
	sub, exists := s.noticeSub(userID, kind)
	done := func(status, textKey string, args ...any) action.Result {
		return action.WithCard(action.OK(req, map[string]any{"status": status}),
			action.Card{Title: title, Text: s.msg(req, textKey, args...), Tone: "ok", Ephemeral: true})
	}
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	switch op {
	case "delete":
		_, _ = db.DB.Exec(`DELETE FROM notice_subs WHERE user_id=? AND kind=?`, userID, kind)
		_, _ = db.DB.Exec(`DELETE FROM notice_delivered WHERE user_id=? AND item_key IN (SELECT item_key FROM notice_items WHERE kind=?)`, userID, kind)
		_, _ = s.setConsent(userID, scope, privacy.Withdrawn, "discord")
		return done("deleted", "notices.deleted")
	case "pause", "resume":
		if !exists {
			return done("no_profile", "notices.register_first."+kind)
		}
		_, _ = db.DB.Exec(`UPDATE notice_subs SET active=?, updated_at=? WHERE user_id=? AND kind=?`, b2i(op == "resume"), s.Clock.Now().UTC().Format(time.RFC3339), userID, kind)
		return done(op+"d", "notices."+op+"d")
	}
	if !s.ConsentAllows(userID, scope) {
		res := s.consentNotice(req, userID, scope)
		cd := res.Output["card"].(action.Card)
		cd.Footer = s.msg(req, "fortune.consent_then")
		res = action.WithCard(res, cd)
		res.Output["status"] = "consent_required"
		return res
	}
	if !exists && op != "register" && op != "subscribe" {
		return done("no_profile", "notices.register_first."+kind)
	}
	sub.UserID, sub.Kind, sub.Active = userID, kind, true
	switch op {
	case "register", "subscribe":
		if kind == notices.KindTransfer {
			ids, unknown := s.pickUniversities(c, strVal(in, "universities", strVal(in, "text", "")))
			if len(unknown) > 0 || len(ids) == 0 {
				return action.Fail(req, errcode.InvalidInput, "notices.bad_university", s.msg(req, "notices.bad_university", "names", strings.Join(unknown, ", "), "list", s.universityList(c)), nil)
			}
			sub.Sources = ids
		} else {
			p, bad := s.parseSchoolProfile(req, c, in, sub.Profile)
			if bad != "" {
				return action.Fail(req, errcode.InvalidInput, "notices.bad_profile", bad, nil)
			}
			sub.Profile = p
		}
		if t := strings.TrimSpace(strVal(in, "time", "")); t != "" {
			if !hhmm.MatchString(t) {
				return action.Fail(req, errcode.InvalidInput, "fortune.bad_time", s.msg(req, "fortune.bad_time"), nil)
			}
			tt, _ := time.Parse("15:04", t)
			sub.DeliveryTime = tt.Format("15:04")
		}
		if err := s.saveNoticeSub(sub); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		sub, _ = s.noticeSub(userID, kind)
		// read the new boards soon instead of waiting for the next scheduled run
		s.ownedGo("notice collection", func(ctx context.Context) { _, _ = s.collectNotices(ctx, s.subSources(c, sub)) })
		card := s.subCard(req, c, sub)
		card.Title, card.Tone = s.msg(req, "notices.saved."+kind), "ok"
		return action.WithCard(action.OK(req, map[string]any{"status": "saved"}), card)
	case "time":
		t := strings.TrimSpace(strVal(in, "time", ""))
		if !hhmm.MatchString(t) {
			return action.Fail(req, errcode.InvalidInput, "fortune.bad_time", s.msg(req, "fortune.bad_time"), nil)
		}
		tt, _ := time.Parse("15:04", t)
		sub.DeliveryTime = tt.Format("15:04")
		if err := s.saveNoticeSub(sub); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return done("saved", "notices.time_set", "time", sub.DeliveryTime)
	case "mute", "unmute":
		if kind != notices.KindSchool {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "topics are for school notices", nil)
		}
		topic, ok := c.TopicID(strVal(in, "topic", strVal(in, "text", "")))
		if !ok {
			return action.Fail(req, errcode.InvalidInput, "notices.bad_topic", s.msg(req, "notices.bad_topic", "list", s.topicList(c)), nil)
		}
		muted := removeString(sub.Profile.Muted, topic)
		if op == "mute" {
			muted = append(muted, topic)
		}
		sub.Profile.Muted = muted
		if err := s.saveNoticeSub(sub); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return done("saved", "notices."+op+"d_topic", "topic", c.TopicLabel(topic))
	case "status":
		return action.WithCard(action.OK(req, map[string]any{"status": "ok"}), s.subCard(req, c, sub))
	}
	return action.Fail(req, errcode.InvalidInput, "generic.error", "unknown op", nil)
}

func removeString(list []string, v string) []string {
	out := []string{}
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func (s *Service) subSources(c *notices.Catalog, sub noticeSub) []string {
	if sub.Kind == notices.KindTransfer {
		return sub.Sources
	}
	var ids []string
	for _, src := range c.Sources {
		if src.Kind == notices.KindSchool && src.School == sub.Profile.School && !src.Manual {
			ids = append(ids, src.ID)
		}
	}
	return ids
}

func squash(v string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), " ", ""))
}

// pickUniversities matches names ("강원대", "부산대학교", a source id) to transfer boards.
func (s *Service) pickUniversities(c *notices.Catalog, text string) ([]string, []string) {
	var ids, unknown []string
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '，' || r == '/' || r == ' ' }) {
		w := squash(part)
		if w == "" {
			continue
		}
		found := ""
		for _, src := range c.Sources {
			if src.Kind != notices.KindTransfer {
				continue
			}
			name := squash(src.University)
			if w == squash(src.ID) || w == strings.TrimPrefix(src.ID, "transfer_") || name == w || strings.HasPrefix(name, w) || strings.Contains(name, w) && len([]rune(w)) >= 2 {
				found = src.ID
				break
			}
		}
		if found == "" {
			unknown = append(unknown, part)
		} else if !contains(ids, found) {
			ids = append(ids, found)
		}
	}
	return ids, unknown
}

func (s *Service) universityList(c *notices.Catalog) string {
	var names []string
	for _, src := range c.Sources {
		if src.Kind == notices.KindTransfer {
			names = append(names, src.University)
		}
	}
	return strings.Join(names, ", ")
}

func (s *Service) topicList(c *notices.Catalog) string {
	var names []string
	for _, t := range c.Vocabulary.Topics {
		names = append(names, t.Label)
	}
	return strings.Join(names, ", ")
}

// parseSchoolProfile reads the fields of a school profile (or "전북대 학부 2학년 소프트웨어공학과" as text).
func (s *Service) parseSchoolProfile(req action.Request, c *notices.Catalog, in map[string]any, base notices.Profile) (notices.Profile, string) {
	p := base
	if text := strings.TrimSpace(strVal(in, "text", "")); text != "" && strVal(in, "school", "") == "" {
		in = s.splitSchoolText(c, text)
	}
	if v := strings.TrimSpace(strVal(in, "school", "")); v != "" {
		id := ""
		for _, sc := range c.Schools {
			n := squash(sc.Name)
			if w := squash(v); w == sc.ID || w == n || strings.HasPrefix(n, w) && len([]rune(w)) >= 2 {
				id = sc.ID
				break
			}
		}
		if id == "" {
			var names []string
			for _, sc := range c.Schools {
				names = append(names, sc.Name)
			}
			return p, s.msg(req, "notices.bad_school", "name", v, "list", strings.Join(names, ", "))
		}
		p.School = id
	}
	if p.School == "" {
		return p, s.msg(req, "notices.need_school")
	}
	if v := strings.TrimSpace(strVal(in, "degree", "")); v != "" {
		words := map[string]string{}
		for d := range c.Vocabulary.DegreeAudiences {
			words[d] = d
			s.catalogWords(words, "notices.degree."+d+".aliases", d)
		}
		d, ok := words[squash(v)]
		if !ok {
			return p, s.msg(req, "notices.bad_degree")
		}
		p.Degree = d
	}
	if p.Degree == "" {
		p.Degree = "undergraduate"
	}
	if g := intVal(in, "grade", 0); g != 0 {
		if g < 1 || g > 6 {
			return p, s.msg(req, "notices.bad_grade")
		}
		p.Grade = g
	}
	if v, ok := in["department"]; ok {
		p.Department = clip(strings.TrimSpace(fmt.Sprint(v)), 40)
	}
	if v, ok := in["campus"]; ok {
		p.Campus = clip(strings.TrimSpace(fmt.Sprint(v)), 40)
	}
	if v := strings.TrimSpace(strVal(in, "interests", "")); v != "" {
		p.Interests = nil
		for _, w := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			if id, ok := c.TopicID(w); ok && !contains(p.Interests, id) {
				p.Interests = append(p.Interests, id)
			} else if !ok {
				return p, s.msg(req, "notices.bad_topic", "list", s.topicList(c))
			}
		}
	}
	return p, ""
}

// splitSchoolText reads a one-line profile: a school name, a degree word, "N학년"-style grade (the
// catalog's words and notices.grade_pattern), and the rest as the department.
func (s *Service) splitSchoolText(c *notices.Catalog, text string) map[string]any {
	out := map[string]any{}
	degrees := map[string]string{}
	for d := range c.Vocabulary.DegreeAudiences {
		s.catalogWords(degrees, "notices.degree."+d+".aliases", d)
	}
	var grades []*regexp.Regexp
	for _, loc := range s.I18n.Locales() {
		if pat, ok := s.I18n.Lookup(loc, "notices.grade_pattern"); ok {
			if re, err := regexp.Compile(pat); err == nil {
				grades = append(grades, re)
			}
		}
	}
	gradeOf := func(w string) (int, bool) {
		for _, re := range grades {
			if m := re.FindStringSubmatch(w); m != nil {
				for _, g := range m[1:] {
					if n, err := strconv.Atoi(g); err == nil {
						return n, true
					}
				}
			}
		}
		return 0, false
	}
	var rest []string
	for _, w := range strings.Fields(text) {
		switch {
		case out["school"] == nil && s.isSchool(c, w):
			out["school"] = w
		case degrees[squash(w)] != "" && out["degree"] == nil:
			out["degree"] = w
		case out["grade"] == nil && func() bool { _, ok := gradeOf(w); return ok }():
			n, _ := gradeOf(w)
			out["grade"] = float64(n)
		default:
			rest = append(rest, w)
		}
	}
	if len(rest) > 0 {
		out["department"] = strings.Join(rest, " ")
	}
	return out
}

func (s *Service) isSchool(c *notices.Catalog, w string) bool {
	w = squash(w)
	for _, sc := range c.Schools {
		n := squash(sc.Name)
		if w == sc.ID || w == n || strings.HasPrefix(n, w) && len([]rune(w)) >= 2 {
			return true
		}
	}
	return false
}

func (s *Service) noticesSubscribeConsole(ctx context.Context, req action.Request, c *notices.Catalog, in map[string]any, op string) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	switch op {
	case "collect":
		rep, err := s.collectNotices(ctx, strSlice(in, "sources"))
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		return action.OK(req, map[string]any{"report": rep})
	case "delete", "pause", "resume":
		userID, kind := strVal(in, "user_id", ""), strVal(in, "kind", notices.KindSchool)
		if userID == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "user_id required", nil)
		}
		if op == "delete" {
			_, _ = db.DB.Exec(`DELETE FROM notice_subs WHERE user_id=? AND kind=?`, userID, kind)
		} else {
			_, _ = db.DB.Exec(`UPDATE notice_subs SET active=? WHERE user_id=? AND kind=?`, b2i(op == "resume"), userID, kind)
		}
		return action.OK(req, map[string]any{"user_id": userID, "kind": kind, "op": op})
	}
	rows, err := db.DB.Query(`SELECT user_id, kind, delivery_time, active, last_delivery, updated_at FROM notice_subs ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	list := []map[string]any{}
	for rows.Next() {
		var user, kind, at, last, updated string
		var active int
		if rows.Scan(&user, &kind, &at, &active, &last, &updated) == nil {
			list = append(list, map[string]any{"user_id": user, "kind": kind, "time": at, "active": active == 1, "last_delivery": last, "updated_at": updated})
		}
	}
	rows.Close()
	for _, m := range list {
		m["consent"] = s.ConsentAllows(m["user_id"].(string), noticeScope(m["kind"].(string)))
	}
	return action.OK(req, map[string]any{"subscriptions": list})
}

// transferRecent lists recent transfer admission notices (public data: no consent needed).
func (s *Service) transferRecent(req action.Request) action.Result {
	in := inputMap(req)
	c, _, err := s.noticeCatalog()
	if err != nil {
		return action.Fail(req, errcode.InvalidInput, "notices.catalog_invalid", s.msg(req, "notices.catalog_invalid", "error", err.Error()), nil)
	}
	var ids []string
	if names := strings.TrimSpace(strVal(in, "universities", "")); names != "" {
		var unknown []string
		ids, unknown = s.pickUniversities(c, names)
		if len(unknown) > 0 {
			return action.Fail(req, errcode.InvalidInput, "notices.bad_university", s.msg(req, "notices.bad_university", "names", strings.Join(unknown, ", "), "list", s.universityList(c)), nil)
		}
	} else if userID, _, ok := fortuneInvoker(req); ok {
		if sub, has := s.noticeSub(userID, notices.KindTransfer); has && s.ConsentAllows(userID, privacy.TransferNotice) {
			ids = sub.Sources
		}
	}
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	q := `SELECT item_key, source_id, title, url, published, fingerprint FROM notice_items WHERE kind='transfer' AND changed_at >= ?`
	args := []any{s.Clock.Now().AddDate(0, 0, -max(1, intVal(in, "days", 30))).UTC().Format(time.RFC3339)}
	if len(ids) > 0 {
		q += ` AND source_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` ORDER BY CASE WHEN published='' THEN changed_at ELSE published END DESC LIMIT ?`
	args = append(args, min(25, max(1, intVal(in, "limit", 10))))
	rows, err := db.DB.Query(q, args...)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	var list []rankedNotice
	for rows.Next() {
		var n rankedNotice
		var src string
		if rows.Scan(&n.Key, &src, &n.Title, &n.URL, &n.Published, &n.Fingerprint) == nil {
			if so, ok := c.Source(src); ok {
				n.University = so.University
			}
			list = append(list, n)
		}
	}
	rows.Close()
	title := s.msg(req, "notices.recent_title")
	out := map[string]any{"count": len(list)}
	items := []map[string]any{}
	for _, n := range list {
		items = append(items, map[string]any{"title": n.Title, "url": n.URL, "published": n.Published, "university": n.University})
	}
	out["items"] = items
	if len(list) == 0 {
		return action.WithCard(action.OK(req, out), action.Card{Title: title, Text: s.msg(req, "notices.recent_none"), Tone: "neutral"})
	}
	zone := s.instanceZone()
	if userID, _, ok := fortuneInvoker(req); ok {
		zone = s.userLocation(s.userPrefs(userID))
	}
	card := s.noticeCard(req.Locale, title, notices.KindTransfer, list, c, s.Clock.Now().In(zone))
	var manual []string
	for _, src := range c.Sources {
		if src.Kind == notices.KindTransfer && src.Manual && (len(ids) == 0 || contains(ids, src.ID)) {
			manual = append(manual, "["+src.University+"]("+src.OfficialURL+")")
		}
	}
	if len(manual) > 0 {
		card.Footer = s.msg(req, "notices.manual_list", "list", strings.Join(manual, ", "))
		card.Fields = []action.CardField{{Name: s.msg(req, "notices.manual_title"), Value: strings.Join(manual, ", ")}}
		card.Footer = ""
	}
	return action.WithCard(action.OK(req, out), card)
}

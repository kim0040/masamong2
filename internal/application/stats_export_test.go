package application

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// exportCanaries are operator- and caller-chosen strings, Discord-shaped numbers and a secret-shaped value
// the fixtures seed through every route a value can take. None may appear anywhere in an exported file
// (review H4, H5).
var exportCanaries = []string{
	"acme_corp_lookup", "acme-proxy", "llm.acme-corp.example", "api.acme-corp.example", "ft:gpt-4o:acme-corp:abc123",
	"acme-corp", "123456789012345678", "987654321098765432", "sk-live-AbCdEf1234567890SECRET", "Asia/Seoul",
	"Persona Secret Name", "deploy.everything",
}

var exportWeekLabel = regexp.MustCompile(`^\d{4}-W\d{2}$`)

// exportService is a service whose clock is a Tuesday: the last finished week is 2026-W47 (16-22 November).
func exportService(t *testing.T) *Service {
	t.Helper()
	s, clk := deliveryService(t)
	clk.t = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	return s
}

func seedStatsDay(t *testing.T, s *Service, group, day string, rows ...statsRow) {
	t.Helper()
	if err := s.statsReplaceDay(context.Background(), statsDayWrite{Group: group, Day: day, Zone: "UTC", Rows: rows}); err != nil {
		t.Fatalf("seed %s %s: %v", group, day, err)
	}
}

// seedRawStats writes a row straight into stats_daily, bypassing the registry: what a damaged or
// hand-edited database could hold. The export must not trust it.
func seedRawStats(t *testing.T, s *Service, day, metric, dims string, n int64, vals string) {
	t.Helper()
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO stats_daily(day, metric, dims, n, vals) VALUES(?,?,?,?,?)`, day, metric, dims, n, vals); err != nil {
		t.Fatal(err)
	}
}

func exportDoc(t *testing.T, s *Service, in statsExportInput) statsExportDoc {
	t.Helper()
	doc, err := s.buildStatsExport(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func exportJSON(t *testing.T, doc statsExportDoc) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func exportRows(doc statsExportDoc, metric string) []statsExportRow {
	var out []statsExportRow
	for _, r := range doc.Weekly {
		if r.Metric == metric {
			out = append(out, r)
		}
	}
	return out
}

// seedHostile fills the groups the allowlist test reads: operator tool ids, a custom provider key, a
// fine-tune and an org-prefixed model id, ID-shaped numbers and a secret-shaped value, once through the
// registry (which already folds closed values) and once raw.
func seedHostile(t *testing.T, s *Service) {
	t.Helper()
	d := s.Doc()
	d.I18n.Timezone = "Asia/Seoul"
	d.Providers = map[string]config.Provider{
		"acme-proxy": {Kind: config.KindOpenAI, BaseURL: "https://llm.acme-corp.example/v1", CredentialRef: "sk-live-AbCdEf1234567890SECRET"},
	}
	d.Guilds = map[string]config.GuildSettings{"987654321098765432": {}}
	d.Personas = map[string]config.Persona{"Persona Secret Name": {}}
	d.Tools = map[string]tools.Settings{"acme_corp_lookup": {HTTP: &tools.HTTP{URL: "https://api.acme-corp.example/{q}"}}}
	s.setDoc(d, s.Revision())

	day := "2026-11-17"
	seedStatsDay(t, s, statsGroupLLM, day,
		statsRow{Metric: "llm.calls", Dims: map[string]string{"role": "123456789012345678", "model": "ft:gpt-4o:acme-corp:abc123", "provider": "acme-proxy"}, N: 40,
			Vals: map[string]float64{"prompt_tokens": 1000, "cost_usd": 0.5}, Hist: statsHistAdd(nil, 700)},
		statsRow{Metric: "llm.calls", Dims: map[string]string{"role": "sk-live-AbCdEf1234567890SECRET", "model": "acme-corp/my-model", "provider": "https://llm.acme-corp.example/v1"}, N: 9},
		statsRow{Metric: "llm.errors", Dims: map[string]string{"role": "chat", "class": "timeout"}, N: 1},
		statsRow{Metric: "llm.finish", Dims: map[string]string{"role": "chat", "finish": "acme-corp finish"}, N: 3},
	)
	seedStatsDay(t, s, statsGroupTools, day,
		statsRow{Metric: "tools.calls", Dims: map[string]string{"tool": "acme_corp_lookup", "status": "ok", "class": "", "provider": "acme-proxy"}, N: 12},
		statsRow{Metric: "tools.calls", Dims: map[string]string{"tool": "123456789012345678", "status": "acme-corp status", "class": "", "provider": ""}, N: 9},
		statsRow{Metric: "tools.guard", Dims: map[string]string{"tool": "acme_corp_lookup", "guard": "allowed"}, N: 8},
	)
	seedStatsDay(t, s, statsGroupJobs, day,
		statsRow{Metric: "jobs.runs", Dims: map[string]string{"type": "acme_corp_lookup", "status": "succeeded", "code": ""}, N: 6},
	)
	if err := s.statsAdd(context.Background(), []statsRow{
		{Day: day, Metric: "actions.calls", Dims: map[string]string{"action": statsActionID("deploy.everything", false), "via": "gui", "code": "ok"}, N: 7},
		{Day: day, Metric: "actions.calls", Dims: map[string]string{"action": statsActionID("acme_corp_lookup", true), "via": "gui", "code": "ok"}, N: 6},
	}); err != nil {
		t.Fatal(err)
	}
	// raw rows the registry would never write
	seedRawStats(t, s, day, "llm.calls", "role=123456789012345678|model=sk-live-AbCdEf1234567890SECRET|provider=acme-proxy|extra=987654321098765432", 30,
		`{"cost_usd":2,"sk-live-AbCdEf1234567890SECRET":9,"prompt_tokens":10}`)
	seedRawStats(t, s, day, "tools.calls", "tool=acme_corp_lookup|status=deploy.everything|class=Asia/Seoul|provider=llm.acme-corp.example", 20, `{}`)
	seedRawStats(t, s, day, "secrets.dump", "key=sk-live-AbCdEf1234567890SECRET", 99, `{}`)
	seedRawStats(t, s, day, "actions.calls", "action=deploy.everything|via=acme-corp|code=123456789012345678", 25, `{}`)
}

// Every exported dimension value is a member of its dimension's list, and none of the strings an operator,
// a caller or a database could hold appears anywhere in the file, settings included (H4).
func TestStatsExportDimensionsAreAllowlisted(t *testing.T) {
	s := exportService(t)
	seedHostile(t, s)
	doc := exportDoc(t, s, statsExportInput{})
	raw := exportJSON(t, doc)
	for _, c := range exportCanaries {
		if strings.Contains(raw, c) {
			t.Errorf("the export holds %q:\n%s", c, raw)
		}
	}
	if len(doc.Weekly) == 0 {
		t.Fatal("the fixture reached no exported row; the allowlist check would pass for nothing")
	}
	for _, rows := range [][]statsExportRow{doc.Weekly, doc.Totals} {
		for _, r := range rows {
			m, ok := statsLookup(r.Metric)
			if !ok || !m.Export.Exported {
				t.Errorf("exported metric %q is not an exported registry metric", r.Metric)
				continue
			}
			if len(r.Dims) != len(m.Dims) && !(len(m.Dims) == 0 && r.Dims == nil) {
				t.Errorf("%s dims %v are not the declared %d dimensions", r.Metric, r.Dims, len(m.Dims))
			}
			for k, v := range r.Dims {
				d, ok := m.dim(k)
				if !ok {
					t.Errorf("%s exports dimension %q it does not declare", r.Metric, k)
					continue
				}
				if !slices.Contains(d.Values, v) {
					t.Errorf("%s.%s exports %q, which is not in its list", r.Metric, k, v)
				}
			}
			for k := range r.Vals {
				if !slices.Contains(m.Vals, k) {
					t.Errorf("%s exports value %q it does not declare", r.Metric, k)
				}
			}
		}
	}
	// the fixture's names came out as the closed stand-ins
	seen := map[string]bool{}
	for _, r := range doc.Weekly {
		for _, v := range r.Dims {
			seen[v] = true
		}
	}
	for _, want := range []string{"custom", "custom_http", "other"} {
		if !seen[want] {
			t.Errorf("no exported row carries %q: the fixture's names were not folded into it", want)
		}
	}
	if _, ok := doc.Settings["servers"]; !ok || doc.Settings["servers"] != "1" {
		t.Errorf("settings should give the server count as a range, got %v", doc.Settings["servers"])
	}
	for k, v := range doc.Settings {
		switch x := v.(type) {
		case bool, []string:
		case string:
			if len(x) > 16 {
				t.Errorf("setting %s = %q is not an enumeration or a bucket", k, x)
			}
		default:
			t.Errorf("setting %s has type %T: booleans, enumerations and bucketed numbers only", k, v)
		}
	}
}

// Member-facing rows under the weekly threshold fold into "other"; operational rows and the model's own
// tool calls are treated as the policy says; totals are built from the folded rows.
func TestStatsExportFoldsSmallMemberRows(t *testing.T) {
	s := exportService(t)
	for _, day := range []string{"2026-11-10", "2026-11-17"} { // W46, W47
		n := int64(8)
		if day == "2026-11-10" {
			n = 3
		}
		seedStatsDay(t, s, statsGroupTools, day,
			statsRow{Metric: "tools.calls", Dims: map[string]string{"tool": "search.web", "status": "ok", "class": "", "provider": ""}, N: n},
			statsRow{Metric: "tools.calls", Dims: map[string]string{"tool": "weather.query", "status": "ok", "class": "", "provider": ""}, N: 2},
		)
		seedStatsDay(t, s, statsGroupLLM, day,
			statsRow{Metric: "llm.errors", Dims: map[string]string{"role": "chat", "class": "timeout"}, N: 1},
		)
	}
	if err := s.statsAdd(context.Background(), []statsRow{
		{Day: "2026-11-17", Metric: "actions.calls", Dims: map[string]string{"action": "stats.series", "via": "discord", "code": "ok"}, N: 2},
		{Day: "2026-11-17", Metric: "actions.calls", Dims: map[string]string{"action": "stats.series", "via": "gui", "code": "ok"}, N: 2},
		{Day: "2026-11-17", Metric: "actions.calls", Dims: map[string]string{"action": "stats.series", "via": "chat", "code": "ok"}, N: 50},
		{Day: "2026-11-17", Metric: "actions.calls", Dims: map[string]string{"action": "stats.series", "via": "discord", "code": "ok"}, N: 4}, // same row: now 6
	}); err != nil {
		t.Fatal(err)
	}
	doc := exportDoc(t, s, statsExportInput{})

	find := func(rows []statsExportRow, week, metric string, dims map[string]string) *statsExportRow {
		for i := range rows {
			r := &rows[i]
			if r.Week == week && r.Metric == metric && reflect.DeepEqual(r.Dims, dims) {
				return r
			}
		}
		return nil
	}
	allOther := map[string]string{"tool": "other", "status": "other", "class": "other", "provider": "other"}
	search := map[string]string{"tool": "search.web", "status": "ok", "class": "none", "provider": "none"}
	// W46: 3 and 2 are both under 5 → one "other" row of 5; the named rows are gone
	if r := find(doc.Weekly, "2026-W46", "tools.calls", search); r != nil {
		t.Errorf("a member-facing row of 3 events stayed named: %+v", r)
	}
	if r := find(doc.Weekly, "2026-W46", "tools.calls", allOther); r == nil || r.N != 5 {
		t.Errorf("W46 folded row = %+v, want n 5", r)
	}
	// W47: 8 stays named, the 2 folds
	if r := find(doc.Weekly, "2026-W47", "tools.calls", search); r == nil || r.N != 8 {
		t.Errorf("W47 search.web = %+v, want n 8", r)
	}
	if r := find(doc.Weekly, "2026-W47", "tools.calls", allOther); r == nil || r.N != 2 {
		t.Errorf("W47 folded row = %+v, want n 2", r)
	}
	// operational rows are not folded, however small
	if r := find(doc.Weekly, "2026-W47", "llm.errors", map[string]string{"role": "chat", "class": "timeout"}); r == nil || r.N != 1 {
		t.Errorf("an operational row of 1 was folded or lost: %+v", r)
	}
	// actions: discord 6 stays, gui is operational, chat is never exported
	if r := find(doc.Weekly, "2026-W47", "actions.calls", map[string]string{"action": "stats.series", "via": "discord", "code": "ok"}); r == nil || r.N != 6 {
		t.Errorf("discord action row = %+v, want n 6", r)
	}
	if r := find(doc.Weekly, "2026-W47", "actions.calls", map[string]string{"action": "stats.series", "via": "gui", "code": "ok"}); r == nil || r.N != 2 {
		t.Errorf("a gui action row of 2 must stay (not member-facing): %+v", r)
	}
	for _, r := range exportRows(doc, "actions.calls") {
		if r.Dims["via"] == "chat" {
			t.Errorf("the model's own tool calls are in the action counts: %+v", r)
		}
	}
	// totals come from the folded weekly rows: 5 + 2 other, 8 search.web
	if r := find(doc.Totals, "", "tools.calls", allOther); r == nil || r.N != 7 {
		t.Errorf("total of the folded rows = %+v, want 7", r)
	}
	if r := find(doc.Totals, "", "tools.calls", search); r == nil || r.N != 8 {
		t.Errorf("total of search.web = %+v, want 8 (the W46 rows folded)", r)
	}
}

// No daily or hourly row; no date anywhere; activity only as buckets.
func TestStatsExportHasNoDailyOrHourlyRowsAndBucketsActivity(t *testing.T) {
	s := exportService(t)
	seedStatsDay(t, s, statsGroupTurns, "2026-11-17",
		statsRow{Metric: "turns.count", Dims: map[string]string{"outcome": "answered"}, N: 31, Vals: map[string]float64{"cost_usd": 1.25, "latency_ms": 9000}, Hist: statsHistAdd(nil, 900)},
		statsRow{Metric: "turns.hour", Dims: map[string]string{"hour": "3"}, N: 31},
	)
	seedStatsDay(t, s, statsGroupActivity, "2026-11-16", statsRow{Metric: "activity.messages", N: 1234}, statsRow{Metric: "activity.rooms", N: 3}, statsRow{Metric: "activity.people", N: 7})
	seedStatsDay(t, s, statsGroupActivity, "2026-11-17", statsRow{Metric: "activity.messages", N: 222}, statsRow{Metric: "activity.rooms", N: 4}, statsRow{Metric: "activity.people", N: 25})
	doc := exportDoc(t, s, statsExportInput{})
	raw := exportJSON(t, doc)
	if regexp.MustCompile(`\d{4}-\d{2}-\d{2}`).MatchString(raw) {
		t.Errorf("the file holds a calendar date:\n%s", raw)
	}
	for _, r := range append(slices.Clone(doc.Weekly), doc.Totals...) {
		if r.Metric == "turns.hour" || strings.Contains(r.Metric, "hour") {
			t.Errorf("per-hour row exported: %+v", r)
		}
		if r.Week != "" && !exportWeekLabel.MatchString(r.Week) {
			t.Errorf("row week %q is not an ISO week", r.Week)
		}
	}
	if !exportWeekLabel.MatchString(doc.Period.From) || !exportWeekLabel.MatchString(doc.Period.To) || !exportWeekLabel.MatchString(doc.GeneratedAt) {
		t.Errorf("period and generated_at must be weeks: %+v %q", doc.Period, doc.GeneratedAt)
	}
	// the two days fall in W47; 1234 + 222 = 1456 → 1000; rooms max 4 → 2-5; people max 25 → 21-100
	one := func(metric string) statsExportRow {
		t.Helper()
		rows := exportRows(doc, metric)
		if len(rows) != 1 {
			t.Fatalf("%s rows = %+v", metric, rows)
		}
		return rows[0]
	}
	// 16 November is a Monday: W47 holds both days
	if r := one("activity.messages"); r.N != 1000 || r.Range != "" {
		t.Errorf("messages = %+v, want n 1000", r)
	}
	if r := one("activity.rooms"); r.Range != "2-5" || r.N != 0 {
		t.Errorf("rooms = %+v, want range 2-5", r)
	}
	if r := one("activity.people"); r.Range != "21-100" || r.N != 0 {
		t.Errorf("people = %+v, want range 21-100", r)
	}
	if r := one("turns.count"); r.N != 31 || r.Vals["cost_usd"] != 1.25 || len(r.Hist) != statsHistLen {
		t.Errorf("an operational row is exported as counted: %+v", r)
	}
}

// A person's withdrawal removes their rows from the source tables; a finished week's exported numbers do
// not move (review M10).
func TestStatsExportWithdrawnPersonChangesNothing(t *testing.T) {
	s := exportService(t)
	ctx := context.Background()
	seedStatsDay(t, s, statsGroupFeedback, "2026-11-17",
		statsRow{Metric: "feedback.ratings", Dims: map[string]string{"source": "menu", "rating": "good", "reason": ""}, N: 12},
		statsRow{Metric: "feedback.ratings", Dims: map[string]string{"source": "menu", "rating": "bad", "reason": "wrong"}, N: 6},
	)
	seedStatsDay(t, s, statsGroupTurns, "2026-11-17",
		statsRow{Metric: "turns.count", Dims: map[string]string{"outcome": "answered"}, N: 20})
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	ref := personRef("bob")
	if _, err := db.DB.Exec(`INSERT INTO turn_feedback(turn_id, user_hash, source, rating, reason, at) VALUES('t-bob', ?, 'menu', 1, '', ?)`, ref, "2026-11-17T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM turn_feedback WHERE user_hash=?`, ref).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatal("the withdrawn person's row must exist before the withdrawal, or the test proves nothing")
	}
	before := exportDoc(t, s, statsExportInput{})
	if err := s.forgetPerson(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatal("the withdrawal did not remove the person's feedback row")
	}
	after := exportDoc(t, s, statsExportInput{})
	if !reflect.DeepEqual(before, after) {
		t.Errorf("a withdrawal changed the export:\nbefore %s\nafter  %s", exportJSON(t, before), exportJSON(t, after))
	}
	if len(before.Weekly) == 0 {
		t.Fatal("empty export")
	}
}

// stats.enabled: false stops collection, not the export of what exists (review L2).
func TestStatsExportWorksWithStatsOff(t *testing.T) {
	s := exportService(t)
	seedStatsDay(t, s, statsGroupTurns, "2026-11-17",
		statsRow{Metric: "turns.count", Dims: map[string]string{"outcome": "answered"}, N: 20})
	off := false
	d := s.Doc()
	d.Stats.Enabled = &off
	s.setDoc(d, s.Revision())
	if s.statsOn() {
		t.Fatal("stats should be off")
	}
	res := s.Execute(context.Background(), action.Request{Action: "stats.export"})
	if !res.OK {
		t.Fatalf("export with stats off: %+v", res.Error)
	}
	if res.Output["collecting"] != false {
		t.Errorf("collecting = %v, want false", res.Output["collecting"])
	}
	file, ok := res.Output["file"].(map[string]any)
	if !ok {
		t.Fatalf("output file: %T", res.Output["file"])
	}
	weekly, _ := file["weekly"].([]any)
	if len(weekly) != 1 {
		t.Fatalf("weekly rows %v, want the one collected before the switch", weekly)
	}
	if settings, _ := file["settings"].(map[string]any); settings["stats_enabled"] != false {
		t.Errorf("settings.stats_enabled = %v", settings["stats_enabled"])
	}
}

// The install id is made at the first export, stays, and is replaced on request; an empty install exports.
func TestStatsExportInstallID(t *testing.T) {
	s := exportService(t)
	var st statsInstallState
	if found, err := s.kvGet(statsInstallKey, &st); err != nil || found {
		t.Fatalf("an id exists before the first export: %v %v", found, err)
	}
	empty := exportDoc(t, s, statsExportInput{})
	if empty.Weekly == nil || empty.Totals == nil || len(empty.Weekly) != 0 {
		t.Fatalf("an install with no statistics exports empty lists: %+v", empty)
	}
	id := empty.Install.ID
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("install id %q", id)
	}
	if again := exportDoc(t, s, statsExportInput{}); again.Install.ID != id {
		t.Errorf("the id changed without a reset: %q → %q", id, again.Install.ID)
	}
	reset := exportDoc(t, s, statsExportInput{ResetID: true})
	if reset.Install.ID == id || reset.Install.ID == "" {
		t.Errorf("reset kept the id: %q", reset.Install.ID)
	}
	if after := exportDoc(t, s, statsExportInput{}); after.Install.ID != reset.Install.ID {
		t.Errorf("the new id was not kept")
	}
	if empty.Install.Version == "" || empty.Install.OS == "" || empty.Install.Arch == "" || empty.Install.Tier == "" {
		t.Errorf("install facts missing: %+v", empty.Install)
	}
	// first seen is a week, from the oldest statistics day
	seedStatsDay(t, s, statsGroupTurns, "2026-11-10", statsRow{Metric: "turns.count", Dims: map[string]string{"outcome": "answered"}, N: 1})
	if got := exportDoc(t, s, statsExportInput{}).Install.FirstSeen; got != "2026-W46" {
		t.Errorf("first_seen = %q, want 2026-W46", got)
	}
}

// The period is whole ISO weeks (UTC) and only finished weeks.
func TestStatsExportPeriodIsWholeFinishedWeeks(t *testing.T) {
	s, clk := deliveryService(t)
	clk.t = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC) // Tuesday of W48: W47 is finished
	for _, day := range []string{"2026-11-04", "2026-11-11", "2026-11-18", "2026-11-23"} {
		seedStatsDay(t, s, statsGroupTurns, day, statsRow{Metric: "turns.count", Dims: map[string]string{"outcome": "answered"}, N: 10})
	}
	doc := exportDoc(t, s, statsExportInput{From: "2026-11-05", To: "2026-11-19"}) // Thursday .. Thursday
	if doc.Period.From != "2026-W45" || doc.Period.To != "2026-W47" {
		t.Errorf("period = %+v, want W45..W47 (whole weeks)", doc.Period)
	}
	weeks := map[string]int64{}
	for _, r := range exportRows(doc, "turns.count") {
		weeks[r.Week] = r.N
	}
	if !reflect.DeepEqual(weeks, map[string]int64{"2026-W45": 10, "2026-W46": 10, "2026-W47": 10}) {
		t.Errorf("weeks = %v", weeks)
	}
	// the running week (W48, with 2026-11-23) is never in the file, however it is asked for
	doc = exportDoc(t, s, statsExportInput{To: "2026-12-31"})
	if doc.Period.To != "2026-W47" {
		t.Errorf("an unfinished week is exported: %+v", doc.Period)
	}
	// a Monday that has just begun: the week before is not yet settled
	clk.t = time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	if doc = exportDoc(t, s, statsExportInput{}); doc.Period.To != "2026-W46" {
		t.Errorf("W47 is exported the morning after it ended: %+v", doc.Period)
	}
	for _, in := range []statsExportInput{{From: "21.11.2026"}, {To: "soon"}, {From: "2026-11-10", To: "2026-11-01"}} {
		res := s.Execute(context.Background(), action.Request{Action: "stats.export", Input: mustJSON(in)})
		if res.OK || res.Error == nil || res.Error.Code != "INVALID_INPUT" {
			t.Errorf("%+v: %+v", in, res)
		}
	}
	// too long a period is cut to the cap, newest weeks kept
	doc = exportDoc(t, s, statsExportInput{From: "2000-01-03"})
	if doc.Period.To != "2026-W46" || doc.Period.From == "2000-W01" {
		t.Errorf("period not capped: %+v", doc.Period)
	}
}

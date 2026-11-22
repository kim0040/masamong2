package application

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/catalog"
	"masamong/internal/config"
	"masamong/internal/errcode"
)

// Values that must never reach an export as themselves: an operator's tool and provider names, a
// fine-tune id with an organization in it, an unknown Action, free text and separators.
var statsNasty = []string{"", " ", "none", "other", "acme_corp_lookup", "acme-proxy", "ft:gpt-4o:acme-corp:abc123",
	"acme-corp/my-model", "deploy.everything", "12345678901234567890", "a|b=c", "line\nbreak", "  OK  ", "-1", "99",
	"skip_cap", "would_offer", "reaction:👍", "stop", "end_turn", strings.Repeat("x", 300)}

func TestStatsRegistryInvariants(t *testing.T) {
	seen := map[string]bool{}
	groupKind := map[string]statsKind{}
	for _, m := range statsRegistry {
		if seen[m.Name] {
			t.Fatalf("duplicate metric %s", m.Name)
		}
		seen[m.Name] = true
		if !strings.HasPrefix(m.Name, m.Group+".") {
			t.Errorf("%s: name must start with its group %q", m.Name, m.Group)
		}
		if m.Kind != statsCounter && m.Kind != statsDerived {
			t.Errorf("%s: kind %q", m.Name, m.Kind)
		}
		if k, ok := groupKind[m.Group]; ok && k != m.Kind {
			t.Errorf("group %s mixes counters and derived metrics (a replace would delete counters, review H2)", m.Group)
		}
		groupKind[m.Group] = m.Kind
		if m.Doc == "" {
			t.Errorf("%s: no doc", m.Name)
		}
		if m.Combine != statsSum && m.Combine != statsMax {
			t.Errorf("%s: combine %q", m.Name, m.Combine)
		}
		if m.Combine == statsMax && (m.Kind == statsCounter || len(m.Vals) > 0 || m.Hist) {
			t.Errorf("%s: a distinct-per-day count has no values to sum and is never a counter", m.Name)
		}
		for i, v := range m.Vals {
			if v == "" || v == "h" || slices.Contains(m.Vals[i+1:], v) {
				t.Errorf("%s: value key %q", m.Name, v)
			}
		}
		switch m.Export.Bucket {
		case statsBucketNone, statsBucketSig1, statsBucketRange:
		default:
			t.Errorf("%s: bucket %q", m.Name, m.Export.Bucket)
		}
		if !m.Export.Exported && (m.Export.MemberFacing || m.Export.Bucket != "") {
			t.Errorf("%s: local metric with export rules", m.Name)
		}
		for _, when := range []map[string][]string{m.Export.MemberWhen, m.Export.SkipWhen} {
			for k, vs := range when {
				d, ok := m.dim(k)
				if !ok {
					t.Errorf("%s: export rule on undeclared dimension %s", m.Name, k)
				}
				for _, v := range vs {
					if !slices.Contains(d.Values, v) {
						t.Errorf("%s: export rule value %s.%s is not in the vocabulary", m.Name, k, v)
					}
				}
			}
		}
		keys := map[string]bool{}
		for _, d := range m.Dims {
			checkStatsDim(t, m.Name, d)
			if keys[d.Key] {
				t.Errorf("%s: dimension %s twice", m.Name, d.Key)
			}
			keys[d.Key] = true
		}
	}
	if groupKind[statsGroupActions] != statsCounter {
		t.Fatal("the action counter must be a counter group")
	}
	for _, g := range statsDerivedGroups() {
		if groupKind[g] != statsDerived {
			t.Fatalf("derived group %s", g)
		}
	}
	// the v1 groups of review section 5 are all declared
	for _, g := range []string{"turns", "llm", "tools", "deliveries", "feedback", "participation", "reminders", "jobs", "activity", "actions"} {
		if len(statsGroupMetrics(g)) == 0 {
			t.Errorf("group %s has no metric", g)
		}
	}
	if m, _ := statsLookup("turns.hour"); m.Export.Exported {
		t.Fatal("the per-hour profile never leaves the install (review H5 rule 2)")
	}
}

// Every dimension has a closed vocabulary with "other", and whatever goes in, what comes out of the
// export is a member of it (review H4).
func checkStatsDim(t *testing.T, metric string, d statsDim) {
	t.Helper()
	name := metric + "." + d.Key
	if d.Key == "" || strings.ContainsAny(d.Key, "|=") {
		t.Errorf("%s: key", name)
	}
	if d.Fold == nil {
		t.Errorf("%s: no fold (a dimension needs a closed set or a mapping)", name)
		return
	}
	if len(d.Values) < 2 || !slices.Contains(d.Values, statsOther) {
		t.Errorf("%s: vocabulary %v must be closed and hold %q", name, d.Values, statsOther)
	}
	for i, v := range d.Values {
		if v == "" || strings.ContainsAny(v, "|= \n") || slices.Contains(d.Values[i+1:], v) {
			t.Errorf("%s: vocabulary value %q", name, v)
		}
		if got := d.export(v); got != v {
			t.Errorf("%s: vocabulary value %q exports as %q", name, v, got)
		}
	}
	for _, raw := range append(slices.Clone(statsNasty), d.Values...) {
		stored := d.store(raw)
		if stored == "" || strings.ContainsAny(stored, "|=\n") || len([]rune(stored)) > 64 {
			t.Errorf("%s: %q stored as %q", name, raw, stored)
		}
		if !d.Open && !slices.Contains(d.Values, stored) {
			t.Errorf("%s: closed dimension stored %q (from %q) outside its vocabulary", name, stored, raw)
		}
		out := d.export(stored)
		if !slices.Contains(d.Values, out) {
			t.Errorf("%s: %q exports as %q, outside its vocabulary", name, raw, out)
		}
		if d.export(out) != out {
			t.Errorf("%s: export not stable for %q", name, raw)
		}
	}
}

// The fixture of review H4: none of these leave the install as themselves.
func TestStatsExportFoldsOperatorNames(t *testing.T) {
	llm, _ := statsLookup("llm.calls")
	tools, _ := statsLookup("tools.calls")
	actions, _ := statsLookup("actions.calls")
	model, _ := llm.dim("model")
	tool, _ := tools.dim("tool")
	action, _ := actions.dim("action")
	for raw, want := range map[string]string{"ft:gpt-4o:acme-corp:abc123": "custom", "acme-corp/my-model": "custom",
		"deepseek/deepseek-v4.1-flash": "deepseek/deepseek-v4.1-flash", "anthropic/claude-x-9": "anthropic/other", "claude-x-9": "anthropic/other", "": "none"} {
		if got := model.export(model.store(raw)); got != want {
			t.Errorf("model %q exports as %q, want %q", raw, got, want)
		}
	}
	if model.store("Acme/Model") != "Acme/Model" {
		t.Error("an open dimension keeps the operator's name on this install")
	}
	if got := tool.export(tool.store("acme_corp_lookup")); got != "custom_http" {
		t.Errorf("operator tool exports as %q", got)
	}
	if got := tool.export(tool.store("weather.query")); got != "weather.query" {
		t.Errorf("built-in tool exports as %q", got)
	}
	if got := action.store(statsActionID("deploy.everything", false)); got != "unknown" {
		t.Errorf("unknown action stored as %q", got)
	}
	if got := action.store(statsActionID("acme_corp_lookup", true)); got != "custom_http" {
		t.Errorf("operator tool action stored as %q", got)
	}
	doc := config.Document{Providers: map[string]config.Provider{
		"acme-proxy": {Kind: config.KindOpenAI, BaseURL: "https://llm.acme-corp.example/v1"},
		"or":         {Kind: config.KindOpenAI, BaseURL: "https://openrouter.ai/api/v1"},
		"search":     {Kind: config.KindTavily},
	}}
	for key, want := range map[string]string{"acme-proxy": "compatible", "or": "openrouter", "search": "tavily", "gone": "custom", "": "none"} {
		if got := statsProviderFamily(doc, key); got != want {
			t.Errorf("provider %q family %q, want %q", key, got, want)
		}
	}
	finish, _ := statsLookup("llm.finish")
	fd, _ := finish.dim("finish")
	for raw, want := range map[string]string{"end_turn": "stop", "MAX_TOKENS": "length", "tool_use": "tool_calls", "SAFETY": "content_filter", "weird": "other", "": "none"} {
		if got := fd.store(raw); got != want {
			t.Errorf("finish %q → %q, want %q", raw, got, want)
		}
	}
}

func TestStatsDimsCanonical(t *testing.T) {
	m, _ := statsLookup("tools.calls")
	a, err := statsEncodeDims(m, map[string]string{"provider": "tavily", "class": "", "status": "ok", "tool": "search.web"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := statsEncodeDims(m, map[string]string{"tool": "search.web", "status": "ok", "class": "", "provider": "tavily"})
	if err != nil || a != b {
		t.Fatalf("encoding depends on map order: %q vs %q (%v)", a, b, err)
	}
	if a != "tool=search.web|status=ok|class=none|provider=tavily" {
		t.Fatalf("dims %q must follow the registry order", a)
	}
	if got := statsDecodeDims(a); got["class"] != "none" || len(got) != 4 {
		t.Fatalf("decode %v", got)
	}
	if _, err := statsEncodeDims(m, map[string]string{"tool": "x", "status": "ok", "class": ""}); !errors.Is(err, errStatsUndeclared) {
		t.Fatalf("a missing dimension must be refused, got %v", err)
	}
	if _, err := statsEncodeDims(m, map[string]string{"tool": "x", "status": "ok", "class": "", "provider": "", "guild": "1"}); !errors.Is(err, errStatsUndeclared) {
		t.Fatalf("an undeclared dimension must be refused, got %v", err)
	}
	if d, err := statsEncodeDims(statsByName["activity.messages"], nil); err != nil || d != "" {
		t.Fatalf("no dimensions: %q %v", d, err)
	}
}

// withStatsTestCounter declares a counter with values and a histogram for the length of a test (the
// shipped counter has neither).
func withStatsTestCounter(t *testing.T) {
	t.Helper()
	saved := statsRegistry
	statsRegistry = append(slices.Clone(statsRegistry), statsMetric{Name: "actions.test", Group: statsGroupActions, Kind: statsCounter,
		Doc: "test", Dims: []statsDim{closedDim("via", statsVias, nil)}, Vals: []string{"cost_usd"}, Hist: true})
	statsByName["actions.test"] = &statsRegistry[len(statsRegistry)-1]
	t.Cleanup(func() {
		statsRegistry = saved
		delete(statsByName, "actions.test")
	})
}

func statsRead(t *testing.T, s *Service, day, metric, dims string) (int64, map[string]float64, []int64) {
	t.Helper()
	db, _ := s.adminDB()
	var n int64
	var raw string
	if err := db.DB.QueryRow(`SELECT n, vals FROM stats_daily WHERE day=? AND metric=? AND dims=?`, day, metric, dims).Scan(&n, &raw); err != nil {
		t.Fatalf("%s %s %s: %v", day, metric, dims, err)
	}
	vals, hist, err := statsDecodeVals(raw)
	if err != nil {
		t.Fatal(err)
	}
	return n, vals, hist
}

func TestStatsAddIsAdditive(t *testing.T) {
	s, _ := deliveryService(t)
	withStatsTestCounter(t)
	ctx := context.Background()
	h := statsHistAdd(nil, 120)
	add := func() {
		t.Helper()
		err := s.statsAdd(ctx, []statsRow{
			{Day: "2026-11-21", Metric: "actions.calls", Dims: map[string]string{"action": "stats.series", "via": "gui", "code": "ok"}, N: 2},
			{Day: "2026-11-21", Metric: "actions.calls", Dims: map[string]string{"code": "ok", "via": "gui", "action": "stats.series"}, N: 1}, // same row, other map order
			{Day: "2026-11-21", Metric: "actions.test", Dims: map[string]string{"via": "cli"}, N: 1, Vals: map[string]float64{"cost_usd": 0.25}, Hist: h},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	add()
	add()
	if n, _, _ := statsRead(t, s, "2026-11-21", "actions.calls", "action=stats.series|via=gui|code=ok"); n != 6 {
		t.Fatalf("n = %d, want 6 (two adds of 2+1)", n)
	}
	n, vals, hist := statsRead(t, s, "2026-11-21", "actions.test", "via=cli")
	if n != 2 || math.Abs(vals["cost_usd"]-0.5) > 1e-9 || len(hist) != statsHistLen || hist[statsHistIndex(120)] != 2 {
		t.Fatalf("n=%d vals=%v hist=%v", n, vals, hist)
	}
	// derived metrics, undeclared metrics and values are refused; nothing is written
	for _, r := range []statsRow{
		{Day: "2026-11-21", Metric: "turns.count", Dims: map[string]string{"outcome": "answered"}, N: 1},
		{Day: "2026-11-21", Metric: "actions.nope", N: 1},
		{Day: "2026-11-21", Metric: "actions.test", Dims: map[string]string{"via": "cli"}, Vals: map[string]float64{"text": 1}},
		{Day: "21.11.2026", Metric: "actions.test", Dims: map[string]string{"via": "cli"}},
	} {
		if err := s.statsAdd(ctx, []statsRow{r}); err == nil {
			t.Errorf("statsAdd accepted %+v", r)
		}
	}
	if err := s.statsAdd(ctx, []statsRow{{Day: "2026-11-21", Metric: "turns.count", Dims: map[string]string{"outcome": "answered"}, N: 1}}); !errors.Is(err, errStatsKind) {
		t.Fatalf("a derived metric through statsAdd: %v", err)
	}
	off := false
	d := s.Doc()
	d.Stats.Enabled = &off
	s.setDoc(d, s.Revision())
	if err := s.statsAdd(ctx, []statsRow{{Day: "2026-11-21", Metric: "actions.test", Dims: map[string]string{"via": "cli"}, N: 1}}); !errors.Is(err, errStatsOff) {
		t.Fatalf("stats.enabled: false must stop collection, got %v", err)
	}
}

func TestStatsReplaceDay(t *testing.T) {
	s, _ := deliveryService(t)
	ctx := context.Background()
	answered := func(n int64) statsRow {
		return statsRow{Metric: "turns.count", Dims: map[string]string{"outcome": "answered"}, N: n,
			Vals: map[string]float64{"cost_usd": 0.1}, Hist: statsHistAdd(nil, 900)}
	}
	w := statsDayWrite{Group: statsGroupTurns, Day: "2026-11-20", Zone: "Asia/Seoul", Rows: []statsRow{answered(3),
		{Metric: "turns.count", Dims: map[string]string{"outcome": "error"}, N: 1}, {Metric: "turns.hour", Dims: map[string]string{"hour": "9"}, N: 4}}}
	if err := s.statsReplaceDay(ctx, w); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := statsRead(t, s, "2026-11-20", "turns.hour", "hour=09"); n != 4 {
		t.Fatalf("hour row %d", n)
	}
	// a finished day is not rewritten (review H1)
	w.Rows = []statsRow{answered(7)}
	if err := s.statsReplaceDay(ctx, w); !errors.Is(err, errStatsFinalized) {
		t.Fatalf("finished day rewritten without force: %v", err)
	}
	if n, _, _ := statsRead(t, s, "2026-11-20", "turns.count", "outcome=answered"); n != 3 {
		t.Fatalf("a refused replace changed the day: %d", n)
	}
	// forced: the group's rows of that day are replaced as a whole
	w.Force = true
	if err := s.statsReplaceDay(ctx, w); err != nil {
		t.Fatal(err)
	}
	db, _ := s.adminDB()
	var rows int
	_ = db.DB.QueryRow(`SELECT COUNT(*) FROM stats_daily WHERE day='2026-11-20'`).Scan(&rows)
	if n, _, _ := statsRead(t, s, "2026-11-20", "turns.count", "outcome=answered"); n != 7 || rows != 1 {
		t.Fatalf("forced replace: n=%d rows=%d", n, rows)
	}
	// counters are never replaced, another group's metric is refused, an empty day still finishes
	if err := s.statsReplaceDay(ctx, statsDayWrite{Group: statsGroupActions, Day: "2026-11-20"}); !errors.Is(err, errStatsKind) {
		t.Fatalf("counter group replaced: %v", err)
	}
	err := s.statsReplaceDay(ctx, statsDayWrite{Group: statsGroupTurns, Day: "2026-11-19",
		Rows: []statsRow{{Metric: "actions.calls", Dims: map[string]string{"action": "stats.series", "via": "gui", "code": "ok"}, N: 1}}})
	if !errors.Is(err, errStatsKind) {
		t.Fatalf("counter row in a derived replace: %v", err)
	}
	if err := s.statsReplaceDay(ctx, statsDayWrite{Group: statsGroupTurns, Day: "2026-11-19",
		Rows: []statsRow{{Metric: "llm.errors", Dims: map[string]string{"role": "planner", "class": "timeout"}, N: 1}}}); !errors.Is(err, errStatsKind) {
		t.Fatalf("another group's metric: %v", err)
	}
	if err := s.statsReplaceDay(ctx, statsDayWrite{Group: statsGroupTurns, Day: "2026-11-19"}); err != nil {
		t.Fatal(err)
	}
	days, err := s.statsFinishedDays(ctx, statsGroupTurns, "", "")
	if err != nil || !slices.Equal(days, []string{"2026-11-19", "2026-11-20"}) {
		t.Fatalf("finished days %v %v", days, err)
	}
	if days, _ := s.statsFinishedDays(ctx, statsGroupLLM, "", ""); len(days) != 0 {
		t.Fatalf("other group finished %v", days)
	}
	// a counter written for the same day survives a forced replace of a derived group
	if err := s.statsAdd(ctx, []statsRow{{Day: "2026-11-20", Metric: "actions.calls", Dims: map[string]string{"action": "stats.series", "via": "gui", "code": "ok"}, N: 5}}); err != nil {
		t.Fatal(err)
	}
	if err := s.statsReplaceDay(ctx, w); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := statsRead(t, s, "2026-11-20", "actions.calls", "action=stats.series|via=gui|code=ok"); n != 5 {
		t.Fatalf("a derived replace touched a counter: %d", n)
	}
}

func TestStatsHistogramAndBuckets(t *testing.T) {
	if len(statsHistBounds) != 11 || statsHistBounds[0] != 100 || statsHistBounds[10] != 120000 {
		t.Fatalf("bounds %v (review M1)", statsHistBounds)
	}
	var h []int64
	for _, ms := range []int64{50, 100, 101, 900, 900, 900, 200000} {
		h = statsHistAdd(h, ms)
	}
	if h[0] != 2 || h[1] != 1 || h[3] != 3 || h[len(h)-1] != 1 {
		t.Fatalf("histogram %v", h)
	}
	if q := statsHistQuantile(h, 0.5); q != 1000 {
		t.Fatalf("p50 %d", q)
	}
	if q := statsHistQuantile(h, 1); q != math.MaxInt64 {
		t.Fatalf("p100 %d", q)
	}
	if statsHistQuantile(nil, 0.5) != -1 {
		t.Fatal("empty histogram")
	}
	if m := statsHistMerge(slices.Clone(h), h); m[3] != 6 {
		t.Fatalf("merge %v", m)
	}
	for n, want := range map[int64]int64{0: 0, 7: 7, 56: 60, 1234: 1000, 95: 100, 4500: 5000} {
		if got := statsRoundSig1(n); got != want {
			t.Errorf("round %d = %d, want %d", n, got, want)
		}
	}
	for n, want := range map[int64]string{0: "", 1: "1", 5: "2-5", 6: "6-20", 100: "21-100", 101: "100+"} {
		if got := statsRangeLabel(n); got != want {
			t.Errorf("range %d = %q, want %q", n, got, want)
		}
	}
}

// The catalog entries exist ahead of their handlers: until a part builds them, both Actions fail with a
// code and reason a caller (and the parts' own tests) can see.
func TestStatsActionsNotYetImplemented(t *testing.T) {
	s, _ := deliveryService(t)
	for _, id := range []string{"stats.rollup"} {
		res := s.Execute(context.Background(), action.Request{Action: id})
		if res.OK || res.Error == nil || res.Error.Code != errcode.ActionUnavailable || res.Error.Args["reason"] != catalog.ImplementationDeferred {
			t.Fatalf("%s: %+v", id, res.Error)
		}
	}
}

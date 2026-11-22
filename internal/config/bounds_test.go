package config

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The console schema and Parse must enforce the same numeric limits: bounds (tree.go) is the one
// declaration, this table is the proof. A candidate outside the range is rejected naming the key; a
// committed document is clamped to the nearer limit and reported as a Problem, so an upgrade never
// stops a daemon over a number it stored.
func TestSchemaBoundsEnforced(t *testing.T) {
	ptr := func(v int) *int { return &v }
	cases := map[string]struct {
		set  func(*Document, float64)
		get  func(Document) float64
		low  float64 // value below the limit; skipped when it cannot be written
		high float64 // value above the limit
	}{
		"backup.hour":    {func(d *Document, v float64) { d.Backup.Hour = int(v) }, func(d Document) float64 { return float64(d.Backup.Hour) }, -1, 24},
		"backup.weekday": {func(d *Document, v float64) { d.Backup.Weekday = int(v) }, func(d Document) float64 { return float64(d.Backup.Weekday) }, -1, 7},
		"backup.keep":    {func(d *Document, v float64) { d.Backup.Keep = int(v) }, func(d Document) float64 { return float64(d.Backup.Keep) }, 0, 366},
		"chat.transcript_chars": {func(d *Document, v float64) { d.Chat.TranscriptChars = TranscriptChars(v) },
			func(d Document) float64 { return float64(d.Chat.TranscriptChars) }, 999, 20001},
		"dm.per_user_per_day": {func(d *Document, v float64) { d.DM.PerUserPerDay = int(v) },
			func(d Document) float64 { return float64(d.DM.PerUserPerDay) }, -2, 10001},
		"dm.per_day": {func(d *Document, v float64) { d.DM.PerDay = int(v) },
			func(d Document) float64 { return float64(d.DM.PerDay) }, -2, 100001},
		"limits.budget_alert_percent": {func(d *Document, v float64) { d.Limits.BudgetAlertPercent = int(v) },
			func(d Document) float64 { return float64(d.Limits.BudgetAlertPercent) }, -1, 101},
		"limits.max_concurrent_turns": {func(d *Document, v float64) { d.Limits.MaxConcurrentTurns = int(v) },
			func(d Document) float64 { return float64(d.Limits.MaxConcurrentTurns) }, -1, 65},
		"limits.max_external_attempts": {func(d *Document, v float64) { d.Limits.MaxExternalAttempts = int(v) },
			func(d Document) float64 { return float64(d.Limits.MaxExternalAttempts) }, -1, 21},
		"memory.backfill_days": {func(d *Document, v float64) { d.Memory.BackfillDays = int(v) },
			func(d Document) float64 { return float64(d.Memory.BackfillDays) }, -2, 31},
		"memory.index_interval_min": {func(d *Document, v float64) { d.Memory.IndexIntervalMin = int(v) },
			func(d Document) float64 { return float64(d.Memory.IndexIntervalMin) }, -1, 1441},
		"memory.index_workers": {func(d *Document, v float64) { d.Memory.IndexWorkers = int(v) },
			func(d Document) float64 { return float64(d.Memory.IndexWorkers) }, -1, MaxIndexWorkers + 1},
		"participation.min_score": {func(d *Document, v float64) { d.Participation.MinScore = v },
			func(d Document) float64 { return d.Participation.MinScore }, -0.5, 1.5},
		"web_search.read_pages": {func(d *Document, v float64) { d.WebSearch.ReadPages = ptr(int(v)) },
			func(d Document) float64 { return float64(*d.WebSearch.ReadPages) }, -1, 11},
		"web_search.deep_pages": {func(d *Document, v float64) { d.WebSearch.DeepPages = ptr(int(v)) },
			func(d Document) float64 { return float64(*d.WebSearch.DeepPages) }, -1, 11},
		"web_search.fetch_daily_limit": {func(d *Document, v float64) { d.WebSearch.FetchDailyLimit = int(v) },
			func(d Document) float64 { return float64(d.WebSearch.FetchDailyLimit) }, -1, 5001},
	}
	for path := range bounds {
		if _, ok := cases[path]; !ok {
			t.Fatalf("bounds declares %s but no Parse check is tested for it", path)
		}
	}
	for path, c := range cases {
		b := bounds[path]
		lo, hi := *b[0], *b[1]
		for _, v := range []float64{c.low, c.high} {
			d := Default("x", "/tmp/x")
			c.set(&d, v)
			raw, err := Marshal(d)
			if err != nil {
				t.Fatalf("%s=%v: marshal: %v", path, v, err)
			}
			if _, err := Parse(raw); err == nil {
				t.Fatalf("%s=%v: a candidate outside %v–%v was accepted", path, v, lo, hi)
			} else if !strings.Contains(err.Error(), path) {
				t.Fatalf("%s=%v: the error must name the key: %v", path, v, err)
			}
			got, problems, err := ParseCommitted(raw)
			want := lo
			if v > hi {
				want = hi
			}
			if err != nil {
				t.Fatalf("%s=%v: a committed document must load, got %v", path, v, err)
			}
			if c.get(got) != want {
				t.Fatalf("%s=%v: committed value %v, want it clamped to %v", path, v, c.get(got), want)
			}
			if !slices.ContainsFunc(problems, func(p Problem) bool { return p.Path == path }) {
				t.Fatalf("%s=%v: committed clamp not reported: %v", path, v, problems)
			}
		}
	}
}

// A committed document that passes Parse stays a candidate's equal at every bound edge.
func TestSchemaBoundsEdgesAccepted(t *testing.T) {
	cases := map[string]func(*Document, float64){
		"backup.hour":                 func(d *Document, v float64) { d.Backup.Hour = int(v) },
		"backup.keep":                 func(d *Document, v float64) { d.Backup.Keep = int(v) },
		"chat.transcript_chars":       func(d *Document, v float64) { d.Chat.TranscriptChars = TranscriptChars(v) },
		"limits.budget_alert_percent": func(d *Document, v float64) { d.Limits.BudgetAlertPercent = int(v) },
		"memory.index_workers":        func(d *Document, v float64) { d.Memory.IndexWorkers = int(v) },
		"participation.min_score":     func(d *Document, v float64) { d.Participation.MinScore = v },
	}
	for path, set := range cases {
		b := bounds[path]
		for _, v := range []float64{*b[0], *b[1]} {
			d := Default("x", "/tmp/x")
			set(&d, v)
			raw, err := Marshal(d)
			if err != nil {
				t.Fatalf("%s=%v: marshal: %v", path, v, err)
			}
			if _, err := Parse(raw); err != nil {
				t.Fatalf("%s=%v: a value at the declared limit was rejected: %v", path, v, err)
			}
		}
	}
}

// The console offers each closed value set from enums (tree.go); Parse must accept every member and
// refuse anything else, naming the key. Both read the same lists, and this keeps them tied for the day
// one of them gets a literal of its own.
func TestSchemaEnumsMatchParse(t *testing.T) {
	for path, values := range enums {
		keys := strings.Split(path, ".")
		for _, v := range append(slices.Clone(values), "not-a-choice") {
			tr := baseTree(t)
			set(tr, v, keys...)
			raw, err := yaml.Marshal(tr)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(raw)
			if v == "not-a-choice" {
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Errorf("%s=%q: want an error naming the key, got %v", path, v, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s=%q is offered by the console but Parse refuses it: %v", path, v, err)
			}
		}
	}
}

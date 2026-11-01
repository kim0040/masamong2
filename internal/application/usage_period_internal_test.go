package application

import (
	"context"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
)

func TestUsagePeriodTotalsTheWindow(t *testing.T) {
	s := privacyService(t)
	_ = s.toolUsage() // makes tool_usage
	if _, err := s.MemoryEngine(); err != nil {
		t.Fatal(err)
	}
	loc := s.instanceZone()
	s.Clock = clock.Fixed{T: time.Date(2026, 10, 15, 12, 0, 0, 0, loc)}
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		day, tool, scope string
		n                int
	}{{"2026-10-15", "weather.now", "all", 3}, {"2026-10-14", "weather.now", "all", 5}, {"2026-10-10", "weather.now", "all", 7},
		{"2026-09-20", "weather.now", "all", 100}, {"2026-10-14", "kma", "provider", 4}, {"2026-10-15", "kma", "provider", 1}} {
		if _, err := db.DB.Exec(`INSERT INTO tool_usage(day, tool, scope, subject, n, last_at) VALUES(?,?,?,'',?,'')`, r.day, r.tool, r.scope, r.n); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct {
		day string
		usd float64
	}{{"2026-10-15", 0.01}, {"2026-10-14", 0.02}, {"2026-10-01", 0.04}} {
		if _, err := db.DB.Exec(`INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?, 'chat', ?, 1)`, r.day, r.usd); err != nil {
			t.Fatal(err)
		}
	}
	get := func(period string) map[string]any {
		r := s.Execute(context.Background(), action.Request{Action: "usage.period", Input: []byte(`{"period":"` + period + `"}`)})
		if !r.OK {
			t.Fatalf("%s: %+v", period, r.Error)
		}
		return r.Output
	}
	for period, want := range map[string]struct {
		tool, prov int
		usd        float64
	}{"today": {3, 1, 0.01}, "yesterday": {5, 4, 0.02}, "7d": {15, 5, 0.03}, "30d": {115, 5, 0.07}} {
		o := get(period)
		if o["tools"].(map[string]int)["weather.now"] != want.tool || o["providers"].(map[string]int)["kma"] != want.prov {
			t.Errorf("%s: tools %v providers %v", period, o["tools"], o["providers"])
		}
		if got := o["spent_usd"].(float64); got < want.usd-1e-9 || got > want.usd+1e-9 {
			t.Errorf("%s: spent %v, want %v", period, got, want.usd)
		}
	}
	if o := get("yesterday"); o["from"] != "2026-10-14" || o["to"] != "2026-10-14" {
		t.Fatalf("yesterday window %v..%v", o["from"], o["to"])
	}
	if r := s.Execute(context.Background(), action.Request{Action: "usage.period", Input: []byte(`{"period":"year"}`)}); r.OK {
		t.Fatal("unknown period accepted")
	}
}

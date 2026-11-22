package config

import (
	"maps"
	"strings"
	"testing"
)

// The keys that existed before statistics keep their defaults exactly, and the new ones default to
// what their records followed before: ledgers 90 days (the old fixed constant), pending = usage_days,
// context = turn_detail_days.
func TestRetentionDefaultsUnchanged(t *testing.T) {
	got := Retention{}.Days()
	want := map[string]int{"turns": 400, "turn_detail": 30, "traces": 30, "audit": 400, "usage": 14, "query_cache": 7, "images": 14,
		"ledger": 90, "pending": 14, "context": 30}
	if !maps.Equal(got, want) {
		t.Fatalf("defaults %v, want %v", got, want)
	}
	// an install that changed the old keys keeps the same horizons for the records that followed them
	r := Retention{UsageDays: 5, TurnDetailDays: 60}.Days()
	if r["pending"] != 5 || r["context"] != 60 || r["usage"] != 5 || r["turn_detail"] != 60 {
		t.Fatalf("pending/context must follow usage/turn_detail while unset: %v", r)
	}
	// forever on a record key keeps member content only when its own key is not set
	r = Retention{TurnDetailDays: -1, UsageDays: -1, ContextDays: 30, PendingDays: 14, LedgerDays: -1}.Days()
	if r["context"] != 30 || r["pending"] != 14 || r["ledger"] != -1 || r["turn_detail"] != -1 {
		t.Fatalf("own keys win: %v", r)
	}
}

func TestStatsDefaultOnAndParses(t *testing.T) {
	if !(Stats{}).On() {
		t.Fatal("stats must be on by default")
	}
	off := false
	if (Stats{Enabled: &off}).On() {
		t.Fatal("stats.enabled: false must turn collection off")
	}
	base, err := Marshal(Default("t", "/tmp/masamong-test"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(base), "stats:") || strings.Contains(string(base), "ledger_days") {
		t.Fatalf("the default document must not spell the new keys (an older binary refuses unknown keys):\n%s", base)
	}
	raw := string(base) + "stats:\n  enabled: false\nretention:\n  ledger_days: -1\n  pending_days: 14\n  context_days: 30\n"
	doc, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Stats.On() || doc.Retention.Days()["ledger"] != -1 || doc.Retention.Days()["context"] != 30 {
		t.Fatalf("parsed %+v %+v", doc.Stats, doc.Retention)
	}
}

func TestSchemaImpliesStatsKeys(t *testing.T) {
	imp := implied()
	for k, want := range map[string]any{"stats.enabled": true, "retention.ledger_days": 90, "retention.pending_days": 14, "retention.context_days": 30} {
		if imp[k] != want {
			t.Errorf("%s implied %v, want %v", k, imp[k], want)
		}
	}
}

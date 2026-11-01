package application

import (
	"testing"
	"time"

	"masamong/internal/providers/openai"
)

// A key with less left than the month's budget still allows is a warning; a key about to run out is
// danger whatever the budget; a key with room says nothing.
func TestCreditAttention(t *testing.T) {
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := func(v float64) *float64 { return &v }
	s.credit.at = s.Clock.Now() // fresh: no background refresh
	s.credit.keys = map[string]openai.KeyCredit{"openrouter": {Limit: f(20), Remaining: f(8.5)}}
	if att := s.creditAttention(0.15, 15); len(att) != 1 || att[0].Key != "key_credit_low" || att[0].Severity != "warn" {
		t.Fatalf("8.50 left, 14.85 of budget: %+v", att)
	}
	if att := s.creditAttention(10, 15); len(att) != 0 {
		t.Fatalf("8.50 left, 5 of budget: %+v", att)
	}
	s.credit.keys["openrouter"] = openai.KeyCredit{Limit: f(20), Remaining: f(0.3)}
	if att := s.creditAttention(0, 0); len(att) != 1 || att[0].Key != "key_credit_out" || att[0].Severity != "danger" {
		t.Fatalf("0.30 left: %+v", att)
	}
	if att := s.creditAttention(0, 15); len(att) != 1 || att[0].Key != "key_credit_out" {
		t.Fatalf("0.30 left beats the budget rule: %+v", att)
	}
	s.credit.at = s.Clock.Now().Add(-time.Hour)
	_ = s.providerCredit() // stale: a refresh starts in the background and must not block or panic
}

// A failed read keeps the key's last figure for creditKeep; a key answering without a limit, or no
// longer configured, drops out.
func TestCreditStoreKeepsLastFigureOnError(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	var c creditCache
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c.store(map[string]creditRead{"or": {openai.KeyCredit{Limit: f(20), Remaining: f(8)}, true}, "gone": {openai.KeyCredit{Limit: f(5), Remaining: f(1)}, true}}, t0)
	c.store(map[string]creditRead{"or": {ok: false}}, t0.Add(30*time.Minute))
	if k, ok := c.keys["or"]; !ok || *k.Remaining != 8 || len(c.keys) != 1 {
		t.Fatalf("kept on error, removed provider dropped: %+v", c.keys)
	}
	c.store(map[string]creditRead{"or": {ok: false}}, t0.Add(61*time.Minute))
	if len(c.keys) != 0 {
		t.Fatalf("expires after creditKeep: %+v", c.keys)
	}
	c.store(map[string]creditRead{"or": {openai.KeyCredit{Limit: f(20), Remaining: f(8)}, true}}, t0.Add(2*time.Hour))
	c.store(map[string]creditRead{"or": {openai.KeyCredit{}, true}}, t0.Add(3*time.Hour))
	if len(c.keys) != 0 || c.running {
		t.Fatalf("limit removed: %+v", c.keys)
	}
}

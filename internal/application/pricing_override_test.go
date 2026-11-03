package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
	"masamong/internal/providers/openai"
)

func TestConditionalPricingOverridesPersistAndApplyPerRequest(t *testing.T) {
	root := t.TempDir()
	openReadyService := func() *Service {
		t.Helper()
		svc, err := Open(root, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		input, _ := json.Marshal(map[string]any{"instance": "pricing-override-test"})
		if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: input, Locale: "en"}); !res.OK {
			t.Fatalf("setup: %+v", res)
		}
		return svc
	}
	first := openReadyService()
	raw := []byte(`{"data":[{"id":"tier-model","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000001","overrides":[` +
		`{"min_prompt_tokens":200000,"prompt":"0.000002","completion":"0.000004","input_cache_read":"0.0000002"},` +
		`{"min_prompt_tokens":200000,"completion":"0.000005"}]}}]}`)
	models, err := openai.ParseModels("application/json", raw)
	if err != nil || len(models) != 1 {
		t.Fatalf("parse model prices: %+v %v", models, err)
	}
	first.storePrices("test-provider", models)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	svc := openReadyService() // proves the conditional entries survive the persisted price list.
	t.Cleanup(func() { _ = svc.Close() })
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	price := svc.contextPricer("test-provider")
	in, out, cached, ok := price("tier-model", 200000, at)
	if !ok || in != 1e-6 || out != 2e-6 || cached != 0.1e-6 {
		t.Fatalf("at threshold should use base rates: %g %g %g ok=%v", in, out, cached, ok)
	}
	in, out, cached, ok = price("tier-model", 200001, at)
	if !ok || in != 2e-6 || out != 5e-6 || cached != 0.2e-6 {
		t.Fatalf("above threshold should apply ordered per-key overrides: %g %g %g ok=%v", in, out, cached, ok)
	}
	conservativeIn, conservativeOut, ok := svc.pricer("test-provider")("tier-model")
	if !ok || conservativeIn != 2e-6 || conservativeOut != 5e-6 {
		t.Fatalf("context-free estimate should use the conservative listed bound: %g %g %v", conservativeIn, conservativeOut, ok)
	}
}

// OpenGateway's prices are scheduled promotions that end at pricing.until: the list is read again then,
// not up to a day later; a list without an end keeps the daily refresh.
func TestPriceListStaleAtListedPriceEnd(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fetched := now.Add(-time.Hour).Format(time.RFC3339)
	ends := &priceList{FetchedAt: fetched, Until: now.Add(time.Hour).Format(time.RFC3339)}
	if ends.stale(now) || !ends.stale(now.Add(time.Hour)) {
		t.Fatal("stale from the listed end on")
	}
	daily := &priceList{FetchedAt: fetched}
	if daily.stale(now) || !daily.stale(now.Add(24*time.Hour)) {
		t.Fatal("a list without an end lasts a day")
	}
	// storePrices keeps the first end still ahead
	s := &Service{Clock: clock.Fixed{T: now}}
	s.storePrices("og", []openai.Model{
		{ID: "a", PromptPrice: "0.000001", PriceUntil: now.Add(48 * time.Hour)},
		{ID: "b", PromptPrice: "0.000001", PriceUntil: now.Add(6 * time.Hour)},
		{ID: "c", PromptPrice: "0.000001", PriceUntil: now.Add(-time.Hour)},
	})
	if l := s.prices().lists["og"]; l == nil || l.Until != now.Add(6*time.Hour).Format(time.RFC3339) {
		t.Fatalf("stored list %+v", l)
	}
}

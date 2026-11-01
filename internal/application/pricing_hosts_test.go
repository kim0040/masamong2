package application_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
)

// Hosts that sell models without listing prices (CometAPI) get a reference price from the same model
// on the public list; a host whose detailed list has prices per million tokens (NanoGPT) is read in
// that unit; a model priced nowhere is an alert, because the budget cannot stop it. Shapes recorded
// 2026-10-01.
func TestPricesForHostsWithoutPriceLists(t *testing.T) {
	comet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-v4-flash","object":"model","owned_by":"deepseek","supported_endpoint_types":["openai"]},{"id":"mystery-image-1","object":"model","owned_by":"custom","supported_endpoint_types":["openai"]}],"object":"list"}`))
	}))
	defer comet.Close()
	var nanoQuery string
	nano := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nanoQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"openai/gpt-5.4-nano","object":"model","pricing":{"prompt":0.2,"completion":1.25,"currency":"USD","unit":"per_million_tokens"}}]}`))
	}))
	defer nano.Close()
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the reference list is public: no key is sent")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek/deepseek-v4-flash","pricing":{"prompt":"0.00000014","completion":"0.00000028","input_cache_read":"0.000000014"}},{"id":"deepseek/deepseek-v4-flash:free","pricing":{"prompt":"0","completion":"0"}}]}`))
	}))
	defer ref.Close()
	svc := setupSvc(t)
	application.SetReferencePricesForTest(svc, ref.URL)
	t.Setenv("COMET_KEY", "c")
	t.Setenv("NANO_KEY", "n")
	for id, url := range map[string]string{"cometapi": comet.URL + "/v1", "nanogpt": nano.URL + "/api/v1"} {
		out(t, svc, "providers.add", map[string]any{"api": "compatible", "id": id, "base_url": url})
	}
	out(t, svc, "providers.credentials", map[string]any{"id": "cometapi", "credential_ref": "env:COMET_KEY"})
	out(t, svc, "providers.credentials", map[string]any{"id": "nanogpt", "credential_ref": "env:NANO_KEY"})
	out(t, svc, "models.select", map[string]any{"role": "response", "provider": "cometapi", "model": "deepseek-v4-flash"})
	out(t, svc, "models.select", map[string]any{"role": "summary", "provider": "nanogpt", "model": "openai/gpt-5.4-nano"})
	out(t, svc, "models.select", map[string]any{"role": "image", "provider": "cometapi", "model": "mystery-image-1"})
	var prices map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		prices = out(t, svc, "providers.list", nil)["model_prices"].(map[string]any)
		r, _ := prices["response"].(map[string]any)
		s, _ := prices["summary"].(map[string]any)
		if r["source"] == "reference" && s["source"] == "list" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := prices["response"].(map[string]any)
	if r["source"] != "reference" || r["in_per_mtok"].(float64) < 0.139 || r["in_per_mtok"].(float64) > 0.141 {
		t.Fatalf("CometAPI's deepseek priced from the reference list (the paid listing, not :free): %v", r)
	}
	// the list's prompt-cache price comes with it (API first; the operator can still set one)
	if c, _ := r["cached_per_mtok"].(float64); c < 0.0139 || c > 0.0141 {
		t.Fatalf("cache price from the reference list: %v", r)
	}
	if _, ok := prices["summary"].(map[string]any)["cached_per_mtok"]; ok {
		t.Fatal("a list without a cache price shows none")
	}
	s := prices["summary"].(map[string]any)
	if s["source"] != "list" || s["out_per_mtok"].(float64) < 1.249 || s["out_per_mtok"].(float64) > 1.251 || !strings.Contains(nanoQuery, "detailed=true") {
		t.Fatalf("NanoGPT's detailed list, per million tokens: %v (query %q)", s, nanoQuery)
	}
	var warned string
	for _, raw := range out(t, svc, "dashboard.overview", nil)["attention"].([]any) {
		if a := raw.(map[string]any); a["key"] == "price_unknown" {
			warned = a["args"].(map[string]any)["roles"].(string)
		}
	}
	if warned != "image" {
		t.Fatalf("only the image model is priced nowhere: %q", warned)
	}
}

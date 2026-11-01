package application_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/errcode"
)

const kmaFlat = `{"item":[{"baseDate":"20260915","baseTime":"1500","category":"T1H","obsrValue":"21.5"},{"category":"REH","obsrValue":"40"},{"category":"WSD","obsrValue":"2.1"},{"category":"PTY","obsrValue":"0"}]}`

func TestLiveWeatherThroughConfiguredProvider(t *testing.T) {
	svc := setupSvc(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Query().Get("authKey") != "kma-secret" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"result":401,"message":"invalid key"}`))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(kmaFlat))
	}))
	defer srv.Close()
	t.Setenv("KMA_TEST", "kma-secret")

	show := exec(svc, "config.show", nil)
	yaml := show.Output["yaml"].(string)
	yaml = strings.Replace(yaml, "base_url: https://apihub.kma.go.kr", "base_url: "+srv.URL, 1)
	yaml = strings.Replace(yaml, "credential_ref: env:MASAMONG_KMA_KEY", "credential_ref: env:KMA_TEST", 1)
	yaml = strings.Replace(yaml, "daily_limit: 10000", "daily_limit: 2", 1)
	ap := exec(svc, "config.apply", map[string]any{"yaml": yaml, "base_revision": 1})
	if !ap.OK {
		t.Fatalf("apply %+v", ap)
	}
	doc := exec(svc, "runtime.doctor", nil)
	if doc.Output["features"].(map[string]any)["weather"].(map[string]any)["status"] != "ready" {
		t.Fatalf("doctor after credential: %+v", doc.Output["features"])
	}
	w := exec(svc, "weather.query", map[string]any{"region": "부산"})
	if !w.OK || w.Output["live"] != true || w.Output["source"] != "kma" || w.Output["temp_c"] != 21.5 || w.Output["envelope"] != "flat" {
		t.Fatalf("live weather %+v", w)
	}
	if hits != 1 {
		t.Fatalf("hits %d", hits)
	}
	// Redaction: the JSON result must never contain the secret.
	raw := exec(svc, "providers.list", nil)
	for _, p := range raw.Output["providers"].([]any) {
		if strings.Contains(strings.ToLower(strings.Join([]string{fmtAny(p)}, "")), "kma-secret") {
			t.Fatal("secret leaked into providers.list")
		}
	}
	// Quota: limit 2 → third call is refused before the network.
	exec(svc, "weather.query", map[string]any{"region": "부산"})
	q := exec(svc, "weather.query", map[string]any{"region": "부산"})
	if q.OK || q.Error.Code != errcode.BudgetExceeded || hits != 2 {
		t.Fatalf("quota %+v hits=%d", q, hits)
	}
}

func TestLiveProviderRejectedMapsToStableCode(t *testing.T) {
	svc := setupSvc(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "typ01") {
			_, _ = w.Write([]byte("Error: authKey is not registered\n"))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"30","resultMsg":"SERVICE_KEY_IS_NOT_REGISTERED_ERROR"}}}`))
	}))
	defer srv.Close()
	t.Setenv("KMA_TEST", "wrong")
	yaml := exec(svc, "config.show", nil).Output["yaml"].(string)
	yaml = strings.Replace(yaml, "base_url: https://apihub.kma.go.kr", "base_url: "+srv.URL, 1)
	yaml = strings.Replace(yaml, "credential_ref: env:MASAMONG_KMA_KEY", "credential_ref: env:KMA_TEST", 1)
	if ap := exec(svc, "config.apply", map[string]any{"yaml": yaml, "base_revision": 1}); !ap.OK {
		t.Fatalf("apply %+v", ap)
	}
	w := exec(svc, "weather.query", map[string]any{"region": "서울"})
	if w.OK || w.Error.Code != errcode.ProviderRejected || w.Error.Retryable {
		t.Fatalf("rejected key must be PROVIDER_REJECTED and not retryable: %+v", w)
	}
	pt := exec(svc, "providers.test", map[string]any{"id": "kma"})
	if pt.OK || pt.Error.Code != errcode.ProviderRejected {
		t.Fatalf("providers.test %+v", pt)
	}
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>maintenance</body></html>"))
	}))
	defer html.Close()
	yaml = strings.Replace(exec(svc, "config.show", nil).Output["yaml"].(string), srv.URL, html.URL, 1)
	if ap := exec(svc, "config.apply", map[string]any{"yaml": yaml, "base_revision": 2}); !ap.OK {
		t.Fatalf("apply2 %+v", ap)
	}
	w = exec(svc, "weather.query", map[string]any{"region": "서울"})
	if w.OK || w.Error.Code != errcode.ProviderFormat {
		t.Fatalf("html must be PROVIDER_FORMAT: %+v", w)
	}
}

func TestConfigRejectsInlineSecretsAndUnknownKinds(t *testing.T) {
	svc := setupSvc(t)
	yaml := exec(svc, "config.show", nil).Output["yaml"].(string)
	bad := strings.Replace(yaml, "credential_ref: env:MASAMONG_KMA_KEY", "credential_ref: abcdef123", 1)
	v := exec(svc, "config.validate", map[string]any{"yaml": bad})
	if v.OK || v.Error.Code != errcode.ConfigInvalid || !strings.Contains(v.Error.Message, "never the secret") {
		t.Fatalf("inline secret accepted: %+v", v)
	}
	bad = strings.Replace(yaml, "kind: kma", "kind: weatherapi", 1)
	if v := exec(svc, "config.validate", map[string]any{"yaml": bad}); v.OK {
		t.Fatal("unknown kind accepted")
	}
	c := exec(svc, "providers.credentials", map[string]any{"id": "kma", "value": "abc"})
	if c.OK {
		t.Fatal("secret value must be refused")
	}
	c = exec(svc, "providers.credentials", map[string]any{"id": "kma", "credential_ref": "file:/tmp/kma.key"})
	if !c.OK || c.Revision != 2 {
		t.Fatalf("ref update %+v", c)
	}
	rb := exec(svc, "config.rollback", map[string]any{"revision": 1})
	if !rb.OK || rb.Revision != 3 {
		t.Fatalf("rollback %+v", rb)
	}
	if !strings.Contains(exec(svc, "config.show", nil).Output["yaml"].(string), "env:MASAMONG_KMA_KEY") {
		t.Fatal("rollback content")
	}
	// The config file on disk must never contain a legacy .env secret.
	b, _ := os.ReadFile(filepath.Join(svc.DataRoot, "config.yaml"))
	if strings.Contains(string(b), "abc") {
		t.Fatal("secret on disk")
	}
}

func TestModelsListRealEndpoint(t *testing.T) {
	svc := setupSvc(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer or-key" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"no auth"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-5","name":"GPT-5","context_length":400000,"pricing":{"prompt":"0.000001","completion":"0.000004"}},{"id":"x/embed-1","name":"E"}]}`))
	}))
	defer srv.Close()
	t.Setenv("OR_TEST", "or-key")
	yaml := exec(svc, "config.show", nil).Output["yaml"].(string)
	yaml = strings.Replace(yaml, "base_url: https://openrouter.ai/api/v1", "base_url: "+srv.URL, 1)
	yaml = strings.Replace(yaml, "credential_ref: env:MASAMONG_OPENROUTER_KEY", "credential_ref: env:OR_TEST", 1)
	if ap := exec(svc, "config.apply", map[string]any{"yaml": yaml, "base_revision": 1}); !ap.OK {
		t.Fatalf("apply %+v", ap)
	}
	ml := exec(svc, "models.list", map[string]any{"provider": "openrouter", "filter": "gpt"})
	if !ml.OK || len(ml.Output["models"].([]any)) != 1 || ml.Output["list_is_not_inference"] != true {
		t.Fatalf("models %+v", ml)
	}
	sel := exec(svc, "models.select", map[string]any{"role": "response", "provider": "openrouter", "model": "openai/gpt-5"})
	if !sel.OK || sel.Revision != 3 {
		t.Fatalf("select %+v", sel)
	}
	if bad := exec(svc, "models.select", map[string]any{"role": "response", "provider": "nope", "model": "m"}); bad.OK {
		t.Fatal("unknown provider")
	}
}

func fmtAny(v any) string { return strings.TrimSpace(strings.ReplaceAll(fmtSprint(v), "\n", " ")) }

func fmtSprint(v any) string { return fmt.Sprint(v) }

package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/providers/openmeteo"
	"masamong/internal/tools"
)

// fakeWeather serves Open-Meteo (geocoding + forecast) and KMA on loopback, counting calls.
type fakeWeather struct {
	srv                *httptest.Server
	geo, forecast, kma atomic.Int32
	kmaDown            atomic.Bool
}

var weatherRegionTestID atomic.Uint64

func newFakeWeather(t *testing.T) *fakeWeather {
	t.Helper()
	f := &fakeWeather{}
	read := func(dir, name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "providers", dir, "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/search":
			f.geo.Add(1)
			name := r.URL.Query().Get("name")
			if strings.Contains(name, "아틀란티스") || strings.Contains(name, "アトランティス") {
				_, _ = w.Write([]byte(`{"generationtime_ms":0.1}`))
				return
			}
			_, _ = w.Write(read("openmeteo", "geo_tokyo.json"))
		case r.URL.Path == "/v1/forecast":
			f.forecast.Add(1)
			_, _ = w.Write(read("openmeteo", "forecast_tokyo.json"))
		default:
			f.kma.Add(1)
			if f.kmaDown.Load() {
				w.WriteHeader(503)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(read("kma", "ncst_flat.json"))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func weatherSvc(t *testing.T, f *fakeWeather, kmaKey bool, openMeteoLimit int) *application.Service {
	t.Helper()
	svc := setupSvc(t)
	ops := []any{
		map[string]any{"path": []any{"providers", "openmeteo", "base_url"}, "value": f.srv.URL},
		map[string]any{"path": []any{"providers", "openmeteo", "daily_limit"}, "value": openMeteoLimit},
		map[string]any{"path": []any{"providers", "kma", "base_url"}, "value": f.srv.URL},
	}
	if kmaKey {
		t.Setenv("WEATHER_TEST_KMA", "kma-key")
		ops = append(ops, map[string]any{"path": []any{"providers", "kma", "credential_ref"}, "value": "env:WEATHER_TEST_KMA"})
	}
	if r := exec(svc, "config.patch", map[string]any{"ops": ops}); !r.OK {
		t.Fatalf("config %+v", r.Error)
	}
	return svc
}

func weather(svc *application.Service, locale string, in map[string]any) map[string]any {
	r := execLocale(svc, "weather.query", in, locale)
	out := jsonShape(r.Output)
	if !r.OK {
		out["error_code"] = r.Error.Code
		out["error"] = r.Error.Message
	}
	return out
}

func TestWeatherWorldwideAndKorea(t *testing.T) {
	f := newFakeWeather(t)
	svc := weatherSvc(t, f, false, 100)
	city := fmt.Sprintf("도쿄-%d", weatherRegionTestID.Add(1))

	// a foreign city: geocoded in the request language, then forecast
	out := weather(svc, "ko", map[string]any{"region": city})
	if out["source"] != "open_meteo" || out["region"] != "도쿄, 일본" || out["condition"] != "partly_cloudy" || out["temp_c"] != 23.8 {
		t.Fatalf("tokyo %+v", out)
	}
	if f.geo.Load() != 5 || f.forecast.Load() != 1 {
		t.Fatalf("calls geo=%d forecast=%d", f.geo.Load(), f.forecast.Load())
	}
	// the same place again comes from the geocode cache
	weather(svc, "ko", map[string]any{"region": city})
	if f.geo.Load() != 5 {
		t.Fatalf("second lookup must use the cache, geo=%d", f.geo.Load())
	}
	// a Korean region without a KMA key: placed by the KMA grid, never geocoded
	out = weather(svc, "ko", map[string]any{"region": "부산"})
	if out["source"] != "open_meteo" || out["region"] != "부산" || f.geo.Load() != 5 {
		t.Fatalf("busan %+v geo=%d", out, f.geo.Load())
	}
	if lat := out["latitude"].(float64); lat < 34.9 || lat > 35.4 {
		t.Fatalf("busan latitude %v", lat)
	}
	// tomorrow's forecast
	out = weather(svc, "ko", map[string]any{"region": city, "day_offset": 1})
	fc, _ := out["forecast"].(map[string]any)
	if fc == nil || fc["date"] != "2026-09-25" || fc["condition"] != "rain" {
		t.Fatalf("forecast %+v", out)
	}
	for _, bad := range []any{-1, openmeteo.MaxForecastDays, 30} {
		if out := weather(svc, "ko", map[string]any{"region": city, "day_offset": bad}); out["error_code"] != "INVALID_INPUT" {
			t.Fatalf("day_offset %v must be refused: %+v", bad, out)
		}
	}
	// an unknown place is a clear message, not a provider failure
	out = weather(svc, "ko", map[string]any{"region": "아틀란티스"})
	if out["error_code"] != "INVALID_INPUT" || !strings.Contains(fmt.Sprint(out["error"]), "아틀란티스") {
		t.Fatalf("unknown place %+v", out)
	}
	jaFailure := weather(svc, "ja", map[string]any{"region": "アトランティス"})
	if !strings.Contains(fmt.Sprint(jaFailure["error"]), "見つかりません") {
		t.Fatalf("unknown place failure should follow the request locale: %+v", jaFailure)
	}
}

func TestWeatherPrefersKMAInKoreaAndFallsBack(t *testing.T) {
	f := newFakeWeather(t)
	svc := weatherSvc(t, f, true, 100)
	out := weather(svc, "ko", map[string]any{"region": "서울"})
	if out["source"] != "kma" || f.kma.Load() == 0 || f.forecast.Load() != 0 {
		t.Fatalf("seoul today must use KMA: %+v", out)
	}
	// forecasts come from the worldwide source even in Korea
	out = weather(svc, "ko", map[string]any{"region": "서울", "day_offset": 2})
	if out["source"] != "open_meteo" {
		t.Fatalf("forecast %+v", out)
	}
	f.kmaDown.Store(true)
	out = weather(svc, "ko", map[string]any{"region": "서울"})
	if out["source"] != "open_meteo" || out["fallback_from"] != "kma" {
		t.Fatalf("KMA outage must fall back: %+v", out)
	}
	// asking for KMA explicitly reports its failure instead
	if out := weather(svc, "ko", map[string]any{"region": "서울", "source": "kma"}); out["error_code"] == nil {
		t.Fatalf("explicit KMA must surface the outage: %+v", out)
	}
}

func TestWeatherQuotaAndDisabledProvider(t *testing.T) {
	f := newFakeWeather(t)
	svc := weatherSvc(t, f, false, 2) // geocode + forecast = the whole allowance
	city := fmt.Sprintf("Tokyo-%d", weatherRegionTestID.Add(1))
	if out := weather(svc, "en", map[string]any{"region": city}); out["source"] != "open_meteo" {
		t.Fatalf("first %+v", out)
	}
	if out := weather(svc, "en", map[string]any{"region": city}); out["error_code"] != "BUDGET_EXCEEDED" {
		t.Fatalf("over the daily limit must be refused: %+v", out)
	}
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"providers", "openmeteo", "disabled"}, "value": true}}}); !r.OK {
		t.Fatal(r.Error)
	}
	if out := weather(svc, "en", map[string]any{"region": "Seoul"}); out["status"] != "unconfigured" {
		t.Fatalf("a switched-off provider and no KMA key means no source: %+v", out)
	}
	if out := weather(svc, "en", map[string]any{"region": city}); out["error_code"] != "INVALID_INPUT" {
		t.Fatalf("with only KMA left, a place outside Korea is unknown: %+v", out)
	}
	if st, _ := svc.Registry().Status("openmeteo"); st.Status != "disabled" {
		t.Fatalf("status %+v", st)
	}
}

// A place nobody named is an assumption: the result says it came from the default and which place
// was assumed, so the reply can say so; a named place is the message's and assumes nothing.
func TestWeatherSaysWhenItAssumedThePlace(t *testing.T) {
	f := newFakeWeather(t)
	svc := weatherSvc(t, f, false, 0)
	out := weather(svc, "ko", map[string]any{})
	if out["status"] != "ok" || out["region_source"] != "default" || out["assumed_region"] != "서울" {
		t.Fatalf("defaulted place: %+v", out)
	}
	out = weather(svc, "ko", map[string]any{"region": "부산", "region_source": "nonsense"})
	if out["region_source"] != "message" || out["assumed_region"] != nil {
		t.Fatalf("named place: %+v", out)
	}
	// an operator's default place is still an assumption
	if r := exec(svc, "tools.configure", map[string]any{"id": "weather.query", "defaults": `{"region":"부산"}`}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	out = weather(svc, "ko", map[string]any{})
	if out["region_source"] != "default" || out["assumed_region"] != "부산" {
		t.Fatalf("configured default place: %+v", out)
	}
}

func execLocale(svc *application.Service, act string, in map[string]any, locale string) action.Result {
	raw, _ := json.Marshal(in)
	return svc.Execute(context.Background(), action.Request{Action: act, Input: raw, Locale: locale, RequestID: "t"})
}

// The planner is shown exactly the day offsets the weather tool accepts, and the tool's choices match.
func TestForecastRangeIsOneNumber(t *testing.T) {
	if chat.ForecastDays != openmeteo.MaxForecastDays {
		t.Fatalf("planner shows %d days, the tool answers %d", chat.ForecastDays, openmeteo.MaxForecastDays)
	}
	for _, b := range tools.Builtins() {
		for _, in := range b.Inputs {
			if b.ID == "weather.query" && in.Name == "day_offset" && len(in.Options) != openmeteo.MaxForecastDays {
				t.Fatalf("day_offset choices %v", in.Options)
			}
		}
	}
}

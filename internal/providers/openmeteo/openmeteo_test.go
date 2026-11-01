package openmeteo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseGeocode(t *testing.T) {
	p, err := ParseGeocode("application/json", fixture(t, "geo_tokyo.json"))
	if err != nil || p.Name != "도쿄" || p.Country != "일본" || p.Timezone != "Asia/Tokyo" || p.Latitude != 35.6895 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := ParseGeocode("application/json", fixture(t, "geo_none.json")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no results must be ErrNotFound: %v", err)
	}
	if _, err := ParseGeocode("text/html", []byte("<html>busy</html>")); !errors.Is(err, decode.ErrHTMLResponse) {
		t.Fatalf("html: %v", err)
	}
	if _, err := ParseGeocode("application/json", []byte(`{"results":[{"name":"x","latitude":999,"longitude":0}]}`)); err == nil {
		t.Fatal("out-of-range coordinates must be refused")
	}
}

func TestGeocodePrefersPopulatedPlaces(t *testing.T) {
	body := `{"results":[{"name":"샌프란시스코 국제공항","feature_code":"AIRP","latitude":37.6,"longitude":-122.4},
		{"name":"스프링필드","feature_code":"PPLA2","population":59680,"latitude":1,"longitude":1},
		{"name":"스프링필드","feature_code":"PPLA2","population":170188,"latitude":2,"longitude":2}]}`
	p, err := ParseGeocode("application/json", []byte(body))
	if err != nil || p.Population != 170188 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestGeocodeSearchesEveryLocaleAndChoosesLargestPopulatedMatch(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		language := r.URL.Query().Get("language")
		got = append(got, language)
		w.Header().Set("Content-Type", "application/json")
		if language == "es" {
			_, _ = w.Write([]byte(`{"results":[{"name":"Londres","feature_code":"PPLA","population":8982000,"country":"Reino Unido","country_code":"GB","latitude":51.5,"longitude":-0.1}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"name":"Londres","feature_code":"PPLA2","population":12000,"country":"Argentina","country_code":"AR","latitude":-34.6,"longitude":-58.4}]}`))
	}))
	defer srv.Close()
	a := New(providers.NewClient(time.Second, nil), srv.URL, providers.Credential{})
	p, err := a.Geocode(context.Background(), "Londres", "en")
	if err != nil || p.CountryCode != "GB" || p.Population != 8982000 {
		t.Fatalf("selected place %+v, err %v", p, err)
	}
	want := map[string]bool{"en": true, "ko": true, "ja": true, "zh": true, "es": true}
	for _, language := range got {
		delete(want, language)
	}
	if len(want) != 0 || len(got) != 5 {
		t.Fatalf("geocoder languages %v, missing %v", got, want)
	}
}

func TestParseForecast(t *testing.T) {
	w, err := ParseForecast("application/json", fixture(t, "forecast_tokyo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if *w.TempC != 23.8 || *w.Humidity != 81 || *w.WindMS != 0.6 || w.Code != 2 || w.Condition != "partly_cloudy" {
		t.Fatalf("current %+v", w)
	}
	if w.ObservedAt.Format(time.RFC3339) != "2026-09-24T20:45:00+09:00" {
		t.Fatalf("local observation time %v", w.ObservedAt)
	}
	unknownZone := strings.Replace(string(fixture(t, "forecast_tokyo.json")), `"Asia/Tokyo"`, `"Mars/Olympus"`, 1)
	// a zone name this build does not know: the forecast is kept and read at the offset the API returns
	odd, err := ParseForecast("application/json", []byte(unknownZone))
	if err != nil {
		t.Fatalf("an unknown zone name must not lose the forecast: %v", err)
	}
	if odd.ObservedAt.Format(time.RFC3339) != "2026-09-24T20:45:00+09:00" || odd.Timezone != "Mars/Olympus" {
		t.Fatalf("observation must use utc_offset_seconds: %v %q", odd.ObservedAt, odd.Timezone)
	}
	noOffset := strings.Replace(unknownZone, `"utc_offset_seconds":32400,`, "", 1)
	if w, err := ParseForecast("application/json", []byte(noOffset)); err != nil || w.ObservedAt.Format(time.RFC3339) != "2026-09-24T20:45:00Z" {
		t.Fatalf("without an offset the reading falls back to UTC: %v %v", w.ObservedAt, err)
	}
	if len(w.Days) != 2 || *w.Days[0].MaxC != 28.6 || w.Days[0].Condition != "drizzle" || w.Days[1].MinC != nil || w.Days[1].Condition != "rain" {
		t.Fatalf("days %+v", w.Days)
	}
	if _, err := ParseForecast("application/json", fixture(t, "error.json")); !errors.Is(err, ErrProvider) {
		t.Fatalf("error body: %v", err)
	}
	if _, err := ParseForecast("application/json", []byte(`{"current":{"time":"2026-01-01T00:00"}}`)); !errors.Is(err, decode.ErrUnexpected) {
		t.Fatalf("no temperature: %v", err)
	}
	if _, err := ParseForecast("application/json", []byte(`{"current":{"temperature_2m":1},"daily":{"time":["a","b"],"temperature_2m_max":[1],"temperature_2m_min":[1,2],"precipitation_probability_max":[1,2],"weather_code":[1,2]}}`)); err == nil {
		t.Fatal("ragged daily arrays must be refused")
	}
}

func TestLiveRoundTripWithKey(t *testing.T) {
	var gotKey string
	var gotLangs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("apikey")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/search":
			gotLangs = append(gotLangs, r.URL.Query().Get("language"))
			_, _ = w.Write(fixture(t, "geo_tokyo.json"))
		case "/v1/forecast":
			if r.URL.Query().Get("latitude") == "999.0000" {
				w.WriteHeader(400)
				_, _ = w.Write(fixture(t, "error.json"))
				return
			}
			_, _ = w.Write(fixture(t, "forecast_tokyo.json"))
		}
	}))
	defer srv.Close()
	t.Setenv("OM_TEST_KEY", "om-secret")
	cred, _ := providers.Resolve("env:OM_TEST_KEY")
	c := providers.NewClient(2*time.Second, nil)
	c.MaxRetries = 0
	a := New(c, srv.URL, cred)
	p, err := a.Geocode(context.Background(), "도쿄", "ko")
	if err != nil || p.Name != "도쿄" || gotKey != "om-secret" {
		t.Fatalf("geocode %+v %v languages=%v", p, err, gotLangs)
	}
	if len(gotLangs) != 5 || gotLangs[0] != "ko" {
		t.Fatalf("the member language must be first, then locale fallbacks: %v", gotLangs)
	}
	w, err := a.Forecast(context.Background(), p.Latitude, p.Longitude, 30)
	if err != nil || len(w.Days) != 2 {
		t.Fatalf("forecast %v", err)
	}
	if _, err := a.Forecast(context.Background(), 999, 0, 1); !errors.Is(err, ErrProvider) {
		t.Fatalf("400 with a reason must surface it: %v", err)
	}
	if _, err := a.Geocode(context.Background(), strings.Repeat("x", 81), "ko"); !errors.Is(err, ErrNotFound) {
		t.Fatal("overlong names are refused before a request")
	}
	if strings.Contains(c.Redact(srv.URL+"/v1/search?apikey=om-secret"), "om-secret") {
		t.Fatal("the key must be redacted from logged URLs")
	}
}

func TestConditionAndPrecip(t *testing.T) {
	for code, want := range map[int]string{0: "clear", 3: "overcast", 45: "fog", 53: "drizzle", 63: "rain", 67: "freezing_rain", 75: "snow", 81: "showers", 86: "snow_showers", 99: "thunderstorm", 42: "unknown"} {
		if got := Condition(code); got != want {
			t.Errorf("Condition(%d) = %s", code, got)
		}
	}
	if Precip("showers") != "rain" || Precip("snow_showers") != "snow" || Precip("clear") != "none" {
		t.Fatal("precip mapping")
	}
}

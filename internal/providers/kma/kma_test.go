package kma

import (
	"context"
	"errors"
	"math"
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

func f(v float64) *float64 { return &v }

func TestParseCurrentFormats(t *testing.T) {
	cases := []struct {
		file, ct, variant string
		temp              *float64
		precip            string
	}{
		{"ncst_openapi.json", "application/json;charset=UTF-8", "openapi", f(24.3), "none"},
		{"ncst_flat.json", "text/plain", "flat", f(24.3), "rain"}, // API Hub V2 serves JSON as text/plain
		{"ncst_single_item.json", "application/json", "openapi", f(24.3), "unknown"},
	}
	for _, c := range cases {
		obs, err := ParseCurrent(c.ct, fixture(t, c.file))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if obs.Variant != c.variant {
			t.Errorf("%s: variant %s", c.file, obs.Variant)
		}
		if (obs.TempC == nil) != (c.temp == nil) || (obs.TempC != nil && *obs.TempC != *c.temp) {
			t.Errorf("%s: temp %v", c.file, obs.TempC)
		}
		if obs.Precip != c.precip {
			t.Errorf("%s: precip %s", c.file, obs.Precip)
		}
	}
}

func TestParseCurrentSingleItemReportsMissingCategories(t *testing.T) {
	obs, err := ParseCurrent("application/json", fixture(t, "ncst_single_item.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.MissingCodes) != 3 { // REH WSD PTY
		t.Fatalf("missing %v", obs.MissingCodes)
	}
}

func TestParseCurrentNoDataAndErrors(t *testing.T) {
	if _, err := ParseCurrent("application/json", fixture(t, "ncst_nodata.json")); !errors.Is(err, decode.ErrKMANoData) {
		t.Fatalf("nodata: %v", err)
	}
	if _, err := ParseCurrent("application/json", fixture(t, "ncst_empty_items.json")); !errors.Is(err, decode.ErrKMANoData) {
		t.Fatalf("empty items must be NO_DATA, got %v", err)
	}
	if _, err := ParseCurrent("application/json", fixture(t, "error_auth.json")); !errors.Is(err, decode.ErrKMAService) {
		t.Fatalf("auth error: %v", err)
	}
	if _, err := ParseCurrent("application/json", fixture(t, "error_openapi.json")); !errors.Is(err, decode.ErrKMAService) {
		t.Fatalf("openapi error: %v", err)
	}
	if _, err := ParseCurrent("text/html", fixture(t, "error_html.html")); !errors.Is(err, decode.ErrHTMLResponse) {
		t.Fatalf("html: %v", err)
	}
	// Sentinel values must never become numbers.
	if _, err := ParseCurrent("application/json", fixture(t, "ncst_sentinel.json")); !errors.Is(err, decode.ErrUnexpected) {
		t.Fatalf("sentinel-only payload must be rejected, got %v", err)
	}
}

func TestParseEarthquakes(t *testing.T) {
	list, err := ParseEarthquakes("application/json", fixture(t, "eqk_openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d", len(list))
	}
	if !list[0].Domestic || list[0].Magnitude == nil || *list[0].Magnitude != 3.4 {
		t.Fatalf("domestic first: %+v", list[0])
	}
	if list[0].EventTime.Format("2006-01-02 15:04:05") != "2026-09-15 09:10:30" {
		t.Fatalf("event time %v", list[0].EventTime)
	}
	if list[1].Domestic || !list[1].NoDomesticImpact {
		t.Fatalf("overseas: %+v", list[1])
	}
	if list[1].EventTime.Format("1504") != "1210" { // 12-digit tmEqk
		t.Fatalf("12-digit time %v", list[1].EventTime)
	}
	one, err := ParseEarthquakes("text/plain", fixture(t, "eqk_flat_single.json"))
	if err != nil || len(one) != 1 || !one[0].Domestic {
		t.Fatalf("flat single: %v %+v", err, one)
	}
	none, err := ParseEarthquakes("application/json", fixture(t, "ncst_nodata.json"))
	if err != nil || none != nil {
		t.Fatalf("NO_DATA must be empty success: %v %v", err, none)
	}
}

func TestParseWarnings(t *testing.T) {
	rows, err := ParseWarnings(fixture(t, "wrn_now.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].KindName != "폭염" || rows[0].LevelName != "주의보" || rows[1].KindName != "풍랑" || rows[1].LevelName != "경보" {
		t.Fatalf("csv rows: %+v", rows)
	}
	sp, err := ParseWarnings(fixture(t, "wrn_now_space.txt"))
	if err != nil || len(sp) != 1 || sp[0].Region != "경북북동산지" {
		t.Fatalf("space rows: %v %+v", err, sp)
	}
	empty, err := ParseWarnings(fixture(t, "wrn_now_empty.txt"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty must be success with zero rows: %v %v", err, empty)
	}
	if _, err := ParseWarnings(fixture(t, "wrn_error.txt")); !errors.Is(err, decode.ErrKMATextError) {
		t.Fatalf("error body: %v", err)
	}
	if _, err := ParseWarnings(fixture(t, "error_html.html")); !errors.Is(err, decode.ErrHTMLResponse) {
		t.Fatalf("html: %v", err)
	}
}

// disp=1 answers JSON with Korean, space-padded names; an empty array is "no active warnings".
func TestParseWarningsJSON(t *testing.T) {
	rows, err := ParseWarnings(fixture(t, "wrn_now.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	if r := rows[0]; r.Kind != "V" || r.KindName != "풍랑" || r.Level != "2" || r.LevelName != "주의보" || r.Region != "서해중부바깥먼바다" || r.Command != "발표" || r.Effective != "202608100900" {
		t.Fatalf("row 0: %+v", r)
	}
	if r := rows[1]; r.Kind != "H" || r.Level != "3" || r.LevelName != "경보" {
		t.Fatalf("row 1: %+v", r)
	}
	if r := rows[2]; r.Kind != "K" || r.KindName != "열대야" {
		t.Fatalf("row 2: %+v", r)
	}
	empty, err := ParseWarnings(fixture(t, "wrn_now_empty.json"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("[] must be success with zero rows: %v %v", err, empty)
	}
	if _, err := ParseWarnings(fixture(t, "error_auth.json")); !errors.Is(err, decode.ErrUnexpected) {
		t.Fatalf("json error object: %v", err)
	}
}

func TestLookupGrid(t *testing.T) {
	for _, name := range []string{"서울", "서울특별시", "Seoul", " 서울 "} {
		g, err := LookupGrid(name)
		if err != nil || g.NX != 60 || g.NY != 127 {
			t.Fatalf("%q: %v %+v", name, err, g)
		}
	}
	if _, err := LookupGrid("아틀란티스"); !errors.Is(err, ErrUnknownRegion) {
		t.Fatal("unknown region must not silently default")
	}
}

func TestBaseTimeMargin(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 30, 0, 0, KST)
	d, h, _ := BaseTime(now)
	if d != "20260915" || h != "1400" {
		t.Fatalf("before :45 must use previous hour: %s %s", d, h)
	}
	_, h, _ = BaseTime(time.Date(2026, 9, 15, 15, 50, 0, 0, KST))
	if h != "1500" {
		t.Fatalf("after :45 uses current hour: %s", h)
	}
}

func TestCurrentLiveRoundTripRedactsKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.URL.Query().Get("authKey")
		w.Header().Set("Content-Type", "text/plain") // API Hub quirk
		_, _ = w.Write(fixture(t, "ncst_flat.json"))
	}))
	defer srv.Close()
	t.Setenv("KMA_TEST_KEY", "secret-123")
	cred, err := providers.Resolve("env:KMA_TEST_KEY")
	if err != nil {
		t.Fatal(err)
	}
	a := New(providers.NewClient(2*time.Second, nil), srv.URL, cred)
	a.Now = func() time.Time { return time.Date(2026, 9, 15, 15, 50, 0, 0, KST) }
	obs, err := a.Current(context.Background(), Grid{"서울", 60, 127})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "secret-123" || obs.TempC == nil {
		t.Fatalf("auth %q obs %+v", gotAuth, obs)
	}
	if strings.Contains(cred.String(), "secret") {
		t.Fatal("credential String must be redacted")
	}
	if !strings.Contains(a.Client.Redact(srv.URL+"/x?authKey=secret-123&nx=1"), "authKey=REDACTED") {
		t.Fatal("redact")
	}
}

func TestCurrentWithoutCredential(t *testing.T) {
	a := New(providers.NewClient(time.Second, nil), "http://127.0.0.1:9", providers.Credential{})
	if _, err := a.Current(context.Background(), Grid{"서울", 60, 127}); !errors.Is(err, providers.ErrNoCredential) {
		t.Fatalf("must not hit the network without a credential: %v", err)
	}
}

// The grid inverse lands within a grid cell (5 km) of the cities' real coordinates.
func TestGridLatLon(t *testing.T) {
	for name, want := range map[string][2]float64{"서울": {37.5665, 126.9780}, "부산": {35.1796, 129.0756}, "제주": {33.4996, 126.5312}} {
		g, err := LookupGrid(name)
		if err != nil {
			t.Fatal(err)
		}
		lat, lon := g.LatLon()
		if d := math.Hypot((lat-want[0])*111, (lon-want[1])*89); d > 7 {
			t.Errorf("%s: %.4f,%.4f is %.1f km from %.4f,%.4f", name, lat, lon, d, want[0], want[1])
		}
	}
}

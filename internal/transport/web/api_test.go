//go:build !noweb

package web

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"masamong/internal/action"
)

// A state the setup is expected to be in (no key yet, no bot connected) is a 4xx the console can show,
// not a 502: the browser logs every 5xx as an error and the operator reads it as a crash.
func TestExpectedStatesAreNot5xx(t *testing.T) {
	for code, want := range map[string]int{
		"PROVIDER_UNCONFIGURED": http.StatusFailedDependency,
		"PROVIDER_FAKE_ONLY":    http.StatusFailedDependency,
		"PROVIDER_REJECTED":     http.StatusFailedDependency,
		"DISCORD_NOT_CONNECTED": http.StatusFailedDependency,
		"PROVIDER_RATE_LIMITED": http.StatusTooManyRequests,
		"JOB_NOT_FOUND":         http.StatusNotFound,
		"LEASE_MISMATCH":        http.StatusConflict,
		"BACKUP_VERIFY_FAILED":  http.StatusUnprocessableEntity,
		"IO":                    http.StatusInternalServerError,
		"PROVIDER_UNAVAILABLE":  http.StatusBadGateway, // the service the action asked really failed
		"PROVIDER_FORMAT":       http.StatusBadGateway,
		"INVALID_INPUT":         http.StatusUnprocessableEntity,
	} {
		if got := statusFor(action.Result{Error: &action.Error{Code: code}}); got != want {
			t.Errorf("%s: HTTP %d, want %d", code, got, want)
		}
	}
	if got := statusFor(action.Result{OK: true}); got != http.StatusOK {
		t.Errorf("ok: %d", got)
	}
}

// A value that cannot be encoded answers 500 with an error body, not a 200 with a truncated one.
func TestWriteJSONAnswers500WhenTheValueCannotBeEncoded(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"ch": make(chan int)})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["ok"] != false {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// /api/i18n is the biggest request of every load: it is compressed and revalidated with a tag, so a
// repeat load costs a 304 instead of 150-190 KB.
func TestI18nIsCompressedAndRevalidated(t *testing.T) {
	_, ts := testServer(t)
	get := func(headers map[string]string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+"/api/i18n?lang=ko", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		// asking for gzip by hand turns off the client's transparent decoding
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get(map[string]string{"Accept-Encoding": "gzip"})
	defer resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" || etag == "" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers: %d %v", resp.StatusCode, resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	var body struct {
		Lang    string            `json:"lang"`
		Strings map[string]string `json:"strings"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Lang != "ko" || body.Strings["app.name"] == "" {
		t.Fatalf("body: %v %.80s", err, raw)
	}
	if resp.ContentLength > int64(len(raw))/2 {
		t.Fatalf("gzip saved too little: %d of %d", resp.ContentLength, len(raw))
	}
	again := get(map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag})
	defer again.Body.Close()
	if again.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation: %d", again.StatusCode)
	}
	plain := get(nil)
	defer plain.Body.Close()
	if plain.Header.Get("Content-Encoding") != "" || plain.Header.Get("ETag") != etag {
		t.Fatalf("a client without gzip gets the plain body under the same tag: %v", plain.Header)
	}
}

// A timer-driven load is a poll; everything else keeps its source.
func TestRequestSourcePoll(t *testing.T) {
	for name, tc := range map[string]struct {
		via, poll, want string
	}{
		"console":      {"", "", "web"},
		"cli":          {"cli", "", "cli"},
		"tray":         {"tray", "", "tray"},
		"console poll": {"", "1", "gui_poll"},
		"tray poll":    {"tray", "1", "gui_poll"},
		"unknown via":  {"curl", "", "web"},
	} {
		r, _ := http.NewRequest(http.MethodPost, "/api/action", nil)
		if tc.via != "" {
			r.Header.Set("X-Masamong-Via", tc.via)
		}
		if tc.poll != "" {
			r.Header.Set("X-Masamong-Poll", tc.poll)
		}
		if got := requestSource(r); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

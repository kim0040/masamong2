package application_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
)

func TestVersionsCompare(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		newer           bool
	}{
		{"0.3.0", "0.2.0", true}, {"0.2.1", "0.2.0", true}, {"1.0.0", "0.9.9", true}, {"0.2.0", "0.2.0", false},
		{"0.2.0", "0.3.0", false}, {"0.2.0", "0.2.0-rc1", true}, {"0.10.0", "0.9.0", true}, {"banana", "0.2.0", false},
		// a source build stamped by git describe holds its tag and more
		{"v1.0.0", "v1.0.0-3-gabc1234", false}, {"v1.0.0", "v1.0.0-dirty", false}, {"v1.0.0", "v1.0.0-3-gabc1234-dirty", false},
		{"v1.0.1", "v1.0.0-3-gabc1234", true}, {"v0.9.9", "v1.0.0-3-gabc1234", false}, {"v1.1.0", "v1.1.0-rc1-2-gabc1234", true},
	} {
		if got := application.NewerVersionForTest(c.latest, c.current); got != c.newer {
			t.Errorf("newer(%s, %s) = %v", c.latest, c.current, got)
		}
	}
}

func TestUpdateCheckSaysWhenANewerReleaseIsOut(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var ua atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.UserAgent())
		if status.Load() != 200 {
			w.WriteHeader(int(status.Load()))
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.0","html_url":"https://github.com/kim0040/masamong2/releases/tag/v0.3.0","published_at":"2026-10-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	defer application.UseUpdateAPIForTest(srv.URL+"/%s", "0.2.0")()
	svc := setupSvc(t)
	o := out(t, svc, "updates.check", nil)
	if o["newer"] != true || o["state"].(map[string]any)["latest"] != "0.3.0" || ua.Load() != "masamong/0.2.0" {
		t.Fatalf("check: %v (user agent %v)", o, ua.Load())
	}
	found := false
	for _, a := range out(t, svc, "dashboard.overview", nil)["attention"].([]any) {
		if a.(map[string]any)["key"] == "update_available" {
			found = true
		}
	}
	if !found {
		t.Fatal("no notification for the newer release")
	}
	// a private repository answers 404: nothing to show, no error
	status.Store(404)
	if o := out(t, svc, "updates.check", nil); o["newer"] != false || o["state"].(map[string]any)["status"] != "unavailable" {
		t.Fatalf("404: %v", o)
	}
	// off: no notification even with a newer release recorded
	status.Store(200)
	out(t, svc, "updates.check", nil)
	applyDoc(t, svc, func(d *config.Document) { off := false; d.Updates.Check = &off })
	for _, a := range out(t, svc, "dashboard.overview", nil)["attention"].([]any) {
		if a.(map[string]any)["key"] == "update_available" {
			t.Fatal("update notice while the check is off")
		}
	}
}

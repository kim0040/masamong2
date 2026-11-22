package application_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"masamong/internal/application"
	"masamong/internal/relnotes"
)

func notesFile(t *testing.T, version string) string {
	t.Helper()
	b, err := relnotes.Marshal(relnotes.Note{Version: version, Date: "2026-12-01",
		User:     map[string][]string{"en": {"You can now do a new thing."}, "ko": {"새로운 걸 할 수 있어요."}},
		Operator: map[string][]string{"en": {"A new setting was added."}, "ko": {"설정이 하나 늘었어요."}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The update-available card can show what the newer release changes: the check also reads the
// release's release-notes.json, within the same limits as the other release files, and anything wrong
// with that file leaves the check as it was, with no notes.
func TestUpdateCheckReadsTheNewerReleasesNotes(t *testing.T) {
	var notesHits atomic.Int32
	body, assetSize, assetPath := notesFile(t, "v0.3.0"), 0, "/dl/kim0040/masamong2/v0.3.0/release-notes.json"
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/kim0040/masamong2":
			size := assetSize
			if size == 0 {
				size = len(body)
			}
			_, _ = fmt.Fprintf(w, `{"tag_name":"v0.3.0","html_url":"https://github.com/kim0040/masamong2/releases/tag/v0.3.0","published_at":"2026-10-01T00:00:00Z",
				"assets":[{"name":"release-notes.json","size":%d,"browser_download_url":"http://%s%s"}]}`, size, r.Host, assetPath)
		case "/dl/kim0040/masamong2/v0.3.0/release-notes.json":
			notesHits.Add(1)
			if status != 200 {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer application.SetVersionForTest("0.2.0")()
	defer application.UseSelfUpdateForTest(srv.URL+"/dl/%s/", t.TempDir())()
	svc := setupSvc(t)
	application.UseUpdateAPIForTest(svc, srv.URL+"/api/%s")

	o := out(t, svc, "updates.check", nil)
	n, ok := o["latest_notes"].(map[string]any)
	if !ok || n["version"] != "v0.3.0" || n["language"] != "en" || strList(t, n["user"])[0] != "You can now do a new thing." || strList(t, n["operator"])[0] != "A new setting was added." {
		t.Fatalf("notes: %v", o)
	}
	if _, leaked := o["state"].(map[string]any)["notes"]; leaked {
		t.Fatal("the stored file is not part of the answer")
	}
	// status reads what the check stored, in the caller's language
	if o := out(t, svc, "updates.status", nil); o["latest_notes"] == nil {
		t.Fatalf("status keeps them: %v", o)
	}

	// a file that does not read as valid notes of that very version, an error, a size over the limit,
	// and an address that is not the repository's releases: no notes, and the check itself still works
	for name, set := range map[string]func(){
		"another version": func() { body = notesFile(t, "v0.9.0") },
		"not json":        func() { body = "<html>not found</html>" },
		"an error":        func() { body = notesFile(t, "v0.3.0"); status = 500 },
		"too large":       func() { status = 200; assetSize = 1 << 20 },
		"another address": func() { assetSize = 0; assetPath = "/elsewhere/release-notes.json" },
	} {
		body, status, assetSize, assetPath = notesFile(t, "v0.3.0"), 200, 0, "/dl/kim0040/masamong2/v0.3.0/release-notes.json"
		set()
		before := notesHits.Load()
		o := out(t, svc, "updates.check", nil)
		if o["newer"] != true || o["latest_notes"] != nil {
			t.Errorf("%s: %v", name, o)
		}
		if (name == "too large" || name == "another address") && notesHits.Load() != before {
			t.Errorf("%s: the file was fetched anyway", name)
		}
	}
	// the same release is not newer: nothing to show
	defer application.UseVersionForTest("0.3.0")()
	body, status, assetSize, assetPath = notesFile(t, "v0.3.0"), 200, 0, "/dl/kim0040/masamong2/v0.3.0/release-notes.json"
	if o := out(t, svc, "updates.check", nil); o["newer"] != false || o["latest_notes"] != nil {
		t.Fatalf("not newer: %v", o)
	}
}

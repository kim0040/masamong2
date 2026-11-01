package application_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"masamong/internal/application"
)

// fakeRelease is a release archive whose masamong prints the given version, like the real one.
func fakeRelease(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	dir := strings.TrimSuffix(name, ".tar.gz") + "/"
	files := map[string]string{
		"masamong":   fmt.Sprintf("#!/bin/sh\necho '{\"ok\":true,\"action\":\"help.version\",\"output\":{\"name\":\"masamong\",\"version\":\"%s\"}}'\n", version),
		"masamongd":  fmt.Sprintf("#!/bin/sh\necho '{\"output\":{\"version\":\"%s\"}}'\n", version),
		"install.sh": "#!/bin/sh\n",
		"LICENSE":    "MIT\n",
	}
	for n, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: dir + n, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestUpdateApplyInstallsTheReleaseAndRestarts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake release programs are shell scripts")
	}
	name := application.UpdateAssetNameForTest()
	archive := fakeRelease(t, name, "0.3.0")
	sum := sha256.Sum256(archive)
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	badSum := false
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/kim0040/masamong2":
			fmt.Fprintf(w, `{"tag_name":"v0.3.0","html_url":"https://github.com/kim0040/masamong2/releases/tag/v0.3.0","assets":[
				{"name":%q,"browser_download_url":"%s/dl/kim0040/masamong2/v0.3.0/%s","size":%d},
				{"name":"SHA256SUMS","browser_download_url":"%s/dl/kim0040/masamong2/v0.3.0/SHA256SUMS"}]}`, name, srv.URL, name, len(archive), srv.URL)
		case "/dl/kim0040/masamong2/v0.3.0/" + name:
			_, _ = w.Write(archive)
		case "/dl/kim0040/masamong2/v0.3.0/SHA256SUMS":
			if badSum {
				_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  " + name + "\n"))
				return
			}
			_, _ = w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	bin := t.TempDir()
	for _, b := range []string{"masamong", "masamongd"} {
		if err := os.WriteFile(filepath.Join(bin, b), []byte("old "+b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	defer application.UseUpdateAPIForTest(srv.URL+"/api/%s", "0.2.0")()
	defer application.UseSelfUpdateForTest(srv.URL+"/dl/%s/", bin)()
	svc := setupSvc(t)

	st := out(t, svc, "updates.check", nil)
	if st["newer"] != true || st["can_apply"] != true {
		t.Fatalf("a release install with a newer version can update itself: %v", st)
	}
	// outside the daemon there is nothing to restart
	if r := exec(svc, "updates.apply", map[string]any{"confirm": "0.3.0"}); r.OK {
		t.Fatal("updates.apply outside the daemon")
	}
	svc.Daemon = true
	if r := exec(svc, "updates.apply", map[string]any{"confirm": "0.2.9"}); r.OK {
		t.Fatal("the confirmation must name the version")
	}
	// a sum that does not match changes nothing
	badSum = true
	if r := exec(svc, "updates.apply", map[string]any{"confirm": "0.3.0"}); r.OK || !strings.Contains(r.Error.Message, "SHA256SUMS") {
		t.Fatalf("a damaged download must be refused: %+v", r.Error)
	}
	if b, _ := os.ReadFile(filepath.Join(bin, "masamongd")); string(b) != "old masamongd" {
		t.Fatal("a refused update changed the programs")
	}
	badSum = false
	o := out(t, svc, "updates.apply", map[string]any{"confirm": "0.3.0"})
	if o["installed"] != "0.3.0" || o["restarting"] != true || !strings.Contains(fmt.Sprint(o["backup"]), "-preupdate") {
		t.Fatalf("apply: %v", o)
	}
	if l := <-svc.LifecycleRequests(); l != application.Lifecycle("restart") {
		t.Fatalf("lifecycle %q", l)
	}
	for _, b := range []string{"masamong", "masamongd"} {
		got, _ := os.ReadFile(filepath.Join(bin, b))
		if strings.HasPrefix(string(got), "old") {
			t.Fatalf("%s was not replaced", b)
		}
		if st, _ := os.Stat(filepath.Join(bin, b)); st.Mode()&0o111 == 0 {
			t.Fatalf("%s is not executable", b)
		}
	}
	entries, _ := os.ReadDir(bin)
	programs := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			programs++
		}
	}
	if programs != 2 {
		t.Fatalf("leftover files beside the programs: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(svc.DataRoot, "backups", fmt.Sprint(o["backup"]), "manifest.json")); err != nil {
		t.Fatal("no backup before the update")
	}
}

func TestUpdateApplyRefusesASourceBuild(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.0","assets":[]}`))
	}))
	defer srv.Close()
	// git describe of a clone: not a release version, so the installer updates it
	defer application.UseUpdateAPIForTest(srv.URL+"/%s", "d1e4e02-dirty")()
	svc := setupSvc(t)
	svc.Daemon = true
	o := out(t, svc, "updates.status", nil)
	if o["can_apply"] == true {
		t.Fatalf("a source build cannot update itself: %v", o)
	}
	if r := exec(svc, "updates.apply", map[string]any{"confirm": "0.3.0"}); r.OK {
		t.Fatal("updates.apply on a source build")
	}
}

func TestResetStartsOverButKeepsBackups(t *testing.T) {
	svc := setupSvc(t)
	svc.Daemon = true
	root := svc.DataRoot
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "secrets", "openrouter.key"), []byte("k"), 0o600)
	if r := exec(svc, "instance.reset", map[string]any{"confirm": "yes"}); r.OK {
		t.Fatal("reset without the confirmation word")
	}
	o := out(t, svc, "instance.reset", map[string]any{"confirm": "reset"})
	if o["pending"] != true || !strings.Contains(fmt.Sprint(o["backup"]), "-prereset") {
		t.Fatalf("reset: %v", o)
	}
	if l := <-svc.LifecycleRequests(); l != application.Lifecycle("restart") {
		t.Fatalf("lifecycle %q", l)
	}
	svc.Close()
	if !application.ApplyPendingReset(root, svc.Log) {
		t.Fatal("the staged reset did not run")
	}
	for _, f := range []string{"masamong.db", "config.yaml", "reset-pending.json"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			t.Fatalf("%s survived the reset", f)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "backups", fmt.Sprint(o["backup"]), "manifest.json")); err != nil {
		t.Fatal("the backup taken before the reset is gone")
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "openrouter.key")); err != nil {
		t.Fatal("keys go only when asked")
	}
	// the next start is a fresh install
	again, err := application.Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Ready() {
		t.Fatal("after a reset the instance opens the first-run setup")
	}
	if application.ApplyPendingReset(root, svc.Log) {
		t.Fatal("a reset runs once")
	}
}

func TestFirstStartOfANewVersionTakesABackup(t *testing.T) {
	restore := application.SetVersionForTest("0.2.0")
	defer restore()
	svc := setupSvc(t)
	count := func() int {
		n := 0
		entries, _ := os.ReadDir(filepath.Join(svc.DataRoot, "backups"))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), "-upgrade") {
				n++
			}
		}
		return n
	}
	svc.BackupOnUpgrade() // the version setup ran with: nothing to back up
	if count() != 0 {
		t.Fatal("a new instance's first start is not an upgrade")
	}
	application.SetVersionForTest("0.3.0")
	svc.BackupOnUpgrade()
	svc.BackupOnUpgrade() // the second start of the same version
	if count() != 1 {
		t.Fatalf("upgrade backups: %d, want 1", count())
	}
}

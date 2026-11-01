package application

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func redirectTestArchive(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&buf)
		for _, b := range []string{"masamong", "masamongd"} {
			w, _ := zw.Create(exeName(b))
			_, _ = w.Write([]byte("program " + b))
		}
		_ = zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, b := range []string{"masamong", "masamongd"} {
		_ = tw.WriteHeader(&tar.Header{Name: "r/" + b, Mode: 0o755, Size: int64(len("program " + b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("program " + b))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// GitHub answers a release asset with a redirect to its file host. The provider client refuses every
// cross-origin redirect, so the in-app update could never download anything in production.
func TestUpdateDownloadFollowsTheRedirectToTheAssetHost(t *testing.T) {
	name := updateAssetName()
	archive := redirectTestArchive(t, name)
	sum := sha256.Sum256(archive)
	assetHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			http.Error(w, "credentials reached the asset host", http.StatusForbidden)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer assetHost.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "SHA256SUMS"):
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + name + "\n"))
		case strings.HasSuffix(r.URL.Path, name):
			http.Redirect(w, r, assetHost.URL+"/asset", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	oldPrefix, oldAllow := updateDownloadPrefix, updateRedirectHost
	t.Cleanup(func() { updateDownloadPrefix, updateRedirectHost = oldPrefix, oldAllow })
	updateDownloadPrefix = origin.URL + "/dl/%s/"
	base := origin.URL + "/dl/" + UpdateRepo + "/v9/"
	st := updateState{Asset: &updateAsset{Name: name, URL: base + name}, SumsURL: base + "SHA256SUMS"}
	s := &Service{Log: slog.Default()}
	dir := t.TempDir()

	// the default policy only trusts GitHub's hosts: a redirect anywhere else is refused
	if _, err := downloadRelease(context.Background(), s, st, dir); err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("a redirect to another host must be refused, got %v", err)
	}
	if left := updateTempFiles(t, dir); len(left) != 0 {
		t.Fatalf("a refused redirect left files: %v", left)
	}
	updateRedirectHost = func(host string) bool { return host == "127.0.0.1" }
	arc, err := downloadRelease(context.Background(), s, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(arc)
	staged, err := extractBinariesTo(dir, arc, name)
	if err != nil {
		t.Fatal(err)
	}
	defer removeStaged(staged)
	got, err := os.ReadFile(staged[exeName("masamongd")])
	if err != nil || string(got) != "program masamongd" {
		t.Fatalf("download through the redirect: %q %v", got, err)
	}
}

func updateTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	var left []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".masamong-dl-") || strings.Contains(e.Name(), ".new-") {
			left = append(left, e.Name())
		}
	}
	return left
}

func TestUpdateRedirectPolicyNamesGitHubHostsOnly(t *testing.T) {
	for host, want := range map[string]bool{
		"github.com": true, "release-assets.githubusercontent.com": true, "objects.githubusercontent.com": true,
		"githubusercontent.com.evil.example": false, "evilgithubusercontent.com": false, "evil.example": false, "api.github.com": false,
	} {
		if got := updateRedirectHost(host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}

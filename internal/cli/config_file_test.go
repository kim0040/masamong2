//go:build !noweb

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/launcher"
	"masamong/internal/transport/web"
)

// `config diff --file` reads the file in the command, so it works against a running service even though
// the config Actions no longer open files themselves.
func TestConfigFileIsReadByTheCommandNotTheService(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	var out, errb bytes.Buffer
	if code := Run(ctx, []string{"--data-root", root, "setup", "--instance", "live"}, &out, &errb, true); code != 0 {
		t.Fatal(errb.String())
	}
	svc, err := application.Open(root, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	server := httptest.NewServer(web.New(svc).Handler())
	defer server.Close()
	lock, err := launcher.AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := launcher.WriteInfo(root, launcher.DaemonInfo{PID: 1, Bind: strings.TrimPrefix(server.URL, "http://")}); err != nil {
		t.Fatal(err)
	}
	show := svc.Execute(ctx, action.Request{Action: "config.show", Locale: "en"})
	yaml, _ := show.Output["yaml"].(string)
	cand := filepath.Join(t.TempDir(), "candidate.yaml")
	if err := os.WriteFile(cand, []byte(strings.Replace(yaml, "id: live", "id: synthetic-renamed", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run(ctx, []string{"--data-root", root, "--output", "json", "config", "diff", "--file", cand}, &out, &errb, true); code != 0 {
		t.Fatalf("diff: %s %s", out.String(), errb.String())
	}
	var res action.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Output["changed"] != true {
		t.Fatalf("diff result %s %v", out.String(), err)
	}
	out.Reset()
	errb.Reset()
	if code := Run(ctx, []string{"--data-root", root, "--output", "json", "config", "diff", "--file", filepath.Join(t.TempDir(), "missing.yaml")}, &out, &errb, true); code == 0 || !strings.Contains(errb.String(), "--file") {
		t.Fatalf("a missing file: %s %s", out.String(), errb.String())
	}
}

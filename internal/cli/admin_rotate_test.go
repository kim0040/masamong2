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

func adminToken(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Run(context.Background(), append([]string{"--output", "json"}, args...), &out, &errb, true); code != 0 {
		t.Fatalf("%v: %s %s", args, out.String(), errb.String())
	}
	var res action.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	tok, _ := res.Output["token"].(string)
	return tok
}

// With no service running, `admin token rotate` replaces the token file itself and drops the one-time
// codes made with the old token.
func TestAdminTokenRotateWithoutTheService(t *testing.T) {
	dir := t.TempDir()
	var errb bytes.Buffer
	if code := Run(context.Background(), []string{"--data-root", dir, "setup", "--instance", "gate"}, ioDiscard(), &errb, true); code != 0 {
		t.Fatal(errb.String())
	}
	old := adminToken(t, "--data-root", dir, "admin", "token")
	code, err := web.CreateOnce(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh := adminToken(t, "--data-root", dir, "admin", "token", "rotate")
	if len(fresh) < 32 || fresh == old {
		t.Fatalf("not re-issued: %q", fresh)
	}
	if got := adminToken(t, "--data-root", dir, "admin", "token"); got != fresh {
		t.Fatal("admin token does not print the new token")
	}
	if _, err := os.Stat(filepath.Join(dir, "admin", "login-once", code)); !os.IsNotExist(err) {
		t.Fatalf("a one-time code from before the re-issue is still there: %v", err)
	}
}

// With the service running, the service re-issues the token (it holds the sessions that must end), and
// the command prints the new one.
func TestAdminTokenRotateThroughTheService(t *testing.T) {
	root := t.TempDir()
	var errb bytes.Buffer
	if code := Run(context.Background(), []string{"--data-root", root, "setup", "--instance", "live"}, ioDiscard(), &errb, true); code != 0 {
		t.Fatal(errb.String())
	}
	svc, err := application.Open(root, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	server := httptest.NewServer(web.New(svc).Handler())
	defer server.Close()
	bind := strings.TrimPrefix(server.URL, "http://")
	lock, err := launcher.AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := launcher.WriteInfo(root, launcher.DaemonInfo{PID: 1, Bind: bind}); err != nil {
		t.Fatal(err)
	}
	old, err := web.ReadToken(root)
	if err != nil {
		t.Fatal(err)
	}
	signedIn := launcher.NewClient(bind, old)
	if _, err := signedIn.Action(context.Background(), "help.version", nil); err != nil {
		t.Fatal(err)
	}
	fresh := adminToken(t, "--data-root", root, "admin", "token", "rotate")
	if fresh == old || len(fresh) < 32 {
		t.Fatalf("not re-issued: %q", fresh)
	}
	if got, _ := web.ReadToken(root); got != fresh {
		t.Fatal("printed token differs from the file")
	}
	if _, err := signedIn.Action(context.Background(), "help.version", nil); err == nil {
		t.Fatal("a session made with the old token kept working")
	}
	if _, err := launcher.NewClient(bind, fresh).Action(context.Background(), "help.version", nil); err != nil {
		t.Fatalf("new token: %v", err)
	}
}

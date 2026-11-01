//go:build !noweb

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/launcher"
	"masamong/internal/transport/web"
)

func TestCLIUsesDaemonAndConsoleRevisionStaysCurrent(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	var out, stderr bytes.Buffer
	if code := Run(ctx, []string{"--data-root", root, "setup", "--instance", "live"}, &out, &stderr, true); code != 0 {
		t.Fatalf("setup: %s %s", out.String(), stderr.String())
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
	before := svc.Revision()
	out.Reset()
	if code := Run(ctx, []string{"--data-root", root, "channels", "configure", "--guild_id", "g", "--id", "c", "--respond", "always"}, &out, &stderr, true); code != 0 {
		t.Fatalf("configure: %s %s", out.String(), stderr.String())
	}
	if svc.Revision() != before+1 || svc.Doc().Guilds["g"].Channels["c"].Respond != "always" {
		t.Fatalf("daemon did not see CLI action: revision=%d doc=%+v", svc.Revision(), svc.Doc().Guilds)
	}
	token, err := web.ReadToken(root)
	if err != nil {
		t.Fatal(err)
	}
	console := launcher.NewClient(strings.TrimPrefix(server.URL, "http://"), token)
	defer console.Close(ctx)
	res, err := console.Execute(ctx, action.Request{Action: "channels.configure", BaseRevision: svc.Revision(), Input: json.RawMessage(`{"guild_id":"g","id":"c","respond":"mention"}`), Locale: "en"})
	if err != nil || !res.OK || svc.Doc().Guilds["g"].Channels["c"].Respond != "mention" {
		t.Fatalf("console edit after CLI: %+v %v", res, err)
	}
	// Live reads and failure envelopes use the same output formatter and catalog as offline Actions.
	for _, args := range [][]string{{"config", "document"}, {"channels", "configure", "--id", "missing-guild"}} {
		opt, id, input, _ := ParseArgs(args)
		expected := svc.Execute(ctx, action.Request{Action: id, Input: input, RequestID: "cli"})
		var want bytes.Buffer
		opt.Output = "json"
		wantCode := WriteResult(&want, opt, expected)
		out.Reset()
		code := Run(ctx, append([]string{"--data-root", root}, args...), &out, &stderr, true)
		if code != wantCode || !JSONEqualSchema(out.Bytes(), want.Bytes()) {
			t.Fatalf("daemon output differs for %v: want %s got %s", args, want.String(), out.String())
		}
	}
}

func TestCLIFilePathsKeepShellMeaningForDaemonActions(t *testing.T) {
	for _, actionID := range []string{"config.apply", "config.diff", "imports.start", "imports.preview", "migration.apply"} {
		key := "path"
		if strings.HasPrefix(actionID, "config.") {
			key = "file"
		}
		raw := json.RawMessage(fmt.Sprintf(`{"%s":"relative-input","guild_id":"999999999999999999"}`, key))
		got, err := commandPaths(actionID, raw)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		if err := json.Unmarshal(got, &fields); err != nil {
			t.Fatal(err)
		}
		abs, _ := filepath.Abs("relative-input")
		if fields[key] != abs || fields["guild_id"] != "999999999999999999" {
			t.Fatalf("%s changed shell input: %s", actionID, got)
		}
	}
	for _, actionID := range []string{"channels.attachments.add", "backup.restore"} {
		raw := json.RawMessage(`{"path":"relative-input"}`)
		got, err := commandPaths(actionID, raw)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("%s changed a data-root path: %s %v", actionID, got, err)
		}
	}
}

func TestCLIUnavailableDaemonNeverFallsBackToLocalWrites(t *testing.T) {
	root := t.TempDir()
	var out, stderr bytes.Buffer
	if Run(context.Background(), []string{"--data-root", root, "setup", "--instance", "offline"}, &out, &stderr, true) != 0 {
		t.Fatal(out.String(), stderr.String())
	}
	lock, err := launcher.AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.WriteInfo(root, launcher.DaemonInfo{PID: 1, Bind: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run(context.Background(), []string{"--data-root", root, "channels", "configure", "--guild_id", "g", "--id", "c", "--respond", "always"}, &out, &stderr, true); code == 0 {
		t.Fatal("unavailable daemon reported success")
	}
	lock.Release()
	// A stale discovery record with no live lock permits offline use, and startup reads its config.
	out.Reset()
	if Run(context.Background(), []string{"--data-root", root, "channels", "configure", "--guild_id", "g", "--id", "c", "--respond", "mention"}, &out, &stderr, true) != 0 {
		t.Fatal(out.String(), stderr.String())
	}
	svc, err := application.Open(root, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if svc.Revision() != 2 || svc.Doc().Guilds["g"].Channels["c"].Respond != "mention" {
		t.Fatal("offline action was not loaded on start")
	}
}

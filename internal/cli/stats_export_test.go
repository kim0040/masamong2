package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatsExportCommandWritesTheFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if code := Run(ctx, []string{"--data-root", dir, "--output", "json", "setup", "--instance", "stats"}, ioDiscard(), ioDiscard(), false); code != 0 {
		t.Fatalf("setup %d", code)
	}
	file := filepath.Join(t.TempDir(), "stats.json")
	export := func(extra ...string) (string, int) {
		var out bytes.Buffer
		args := append([]string{"--data-root", dir, "--output", "json", "stats", "export", "--file", file}, extra...)
		code := Run(ctx, args, &out, ioDiscard(), false)
		return out.String(), code
	}
	readID := func() string {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Schema  int `json:"schema"`
			Install struct {
				ID string `json:"id"`
			} `json:"install"`
			Weekly []any `json:"weekly"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("the file is not JSON: %v\n%s", err, raw)
		}
		if doc.Schema != 1 || doc.Install.ID == "" || doc.Weekly == nil {
			t.Fatalf("file: %s", raw)
		}
		return doc.Install.ID
	}
	out, code := export()
	if code != 0 {
		t.Fatalf("export %d %s", code, out)
	}
	if !strings.Contains(out, `"written"`) || strings.Contains(out, `"weekly"`) {
		t.Errorf("with --file the command prints where it wrote, not the file: %s", out)
	}
	id := readID()
	if _, code := export(); code != 0 || readID() != id {
		t.Errorf("a second export changed the install id")
	}
	if _, code := export("--reset-id"); code != 0 || readID() == id {
		t.Errorf("--reset-id kept the install id")
	}
	// without --file the file is in the result
	var out2 bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "--output", "json", "stats", "export", "--from", "2026-01-05", "--to", "2026-02-01"}, &out2, ioDiscard(), false); code != 0 {
		t.Fatalf("export to stdout %d %s", code, out2.String())
	}
	if !strings.Contains(out2.String(), `"weekly"`) || !strings.Contains(out2.String(), `"2026-W02"`) {
		t.Errorf("stdout export: %s", out2.String())
	}
	// --file needs a path
	if _, _, _, err := ParseArgs([]string{"stats", "export", "--file"}); err == nil {
		t.Error("--file without a path was accepted")
	}
	if opt, act, raw, err := ParseArgs([]string{"stats", "export", "--file", "x.json", "--from", "2026-01-05"}); err != nil || act != "stats.export" || opt.File != "x.json" || strings.Contains(string(raw), "file") {
		t.Errorf("parse: %v %s %q %v", opt, act, raw, err)
	}
}

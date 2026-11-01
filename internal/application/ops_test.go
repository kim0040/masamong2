package application_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/application"
)

func patch(t *testing.T, svc *application.Service, path []any, value any) {
	t.Helper()
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": path, "value": value}}}); !r.OK {
		t.Fatalf("patch %v: %+v", path, r.Error)
	}
}

func TestScheduledBackupsPruneAndRestoreRoundTrip(t *testing.T) {
	svc := setupSvc(t)
	root := svc.DataRoot
	sqlitePath := svc.Store.Path()
	patch(t, svc, []any{"backup", "keep"}, 2)
	var first string
	for i := 0; i < 3; i++ {
		r := out(t, svc, "schedules.run", map[string]any{"id": "backup"})
		if i == 0 {
			first, _ = r["state"].(map[string]any)["detail"].(map[string]any)["name"].(string)
		}
	}
	list := out(t, svc, "backup.list", nil)
	auto := 0
	for _, b := range list["backups"].([]any) {
		if b.(map[string]any)["kind"] == "auto" {
			auto++
		}
	}
	if auto != 2 {
		t.Fatalf("keep=2 must leave 2 automatic backups, got %d: %v", auto, list["backups"])
	}
	if _, err := os.Stat(filepath.Join(root, "backups", first)); err == nil {
		t.Fatal("the oldest automatic backup must be pruned")
	}
	// take a backup with the budget at 10.00, change it, then restore
	b := exec(svc, "backup.create", nil)
	name := b.Output["name"].(string)
	patch(t, svc, []any{"limits", "monthly_budget_usd"}, "7.00")
	if r := exec(svc, "backup.restore", map[string]any{"name": name}); r.OK {
		t.Fatal("restore must require the name typed again")
	}
	r := exec(svc, "backup.restore", map[string]any{"name": name, "confirm": name})
	if !r.OK || r.Output["pending"] != true || r.Output["restarting"] != nil {
		t.Fatalf("restore staging: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(root, "backups", r.Output["safety_backup"].(string))); err != nil {
		t.Fatal("a safety backup must be taken before a restore")
	}
	_ = svc.Close()
	for _, sidecar := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(sqlitePath+sidecar, []byte("stale sqlite sidecar"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res := application.ApplyPendingRestore(root, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if res == nil || !res.OK || res.Method != "replace_file" {
		t.Fatalf("apply: %+v", res)
	}
	for _, sidecar := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(sqlitePath + sidecar); !os.IsNotExist(err) {
			t.Fatalf("stale SQLite sidecar survived restore: %s", sidecar)
		}
	}
	if again := application.ApplyPendingRestore(root, slog.Default()); again != nil {
		t.Fatal("a restore applies once")
	}
	svc2, err := application.Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	if svc2.Doc().Limits.MonthlyBudgetUSD != "10.00" {
		t.Fatalf("budget after restore = %s", svc2.Doc().Limits.MonthlyBudgetUSD)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc2.RunScheduler(ctx)
	last := out(t, svc2, "backup.list", nil)["last_restore"].(map[string]any)
	if last["ok"] != true || last["name"] != name {
		t.Fatalf("last restore: %v", last)
	}
	entries := out(t, svc2, "audit.list", map[string]any{"action": "backup.restore.applied"})["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("restore must be in the change log: %v", entries)
	}
}

func TestRestoreFailureRollsBackSafetyBackup(t *testing.T) {
	svc := setupSvc(t)
	root := svc.DataRoot
	target := exec(svc, "backup.create", nil).Output["name"].(string)
	patch(t, svc, []any{"limits", "monthly_budget_usd"}, "7.00")
	staged := exec(svc, "backup.restore", map[string]any{"name": target, "confirm": target})
	if !staged.OK {
		t.Fatal(staged.Error)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// Correct hashes do not authorize touching live data when a logical dump is invalid.
	dir := filepath.Join(root, "backups", target)
	dump := []byte(`{"format":2,"tables":{"unknown_table":[]}}`)
	if err := os.WriteFile(filepath.Join(dir, "dump.json"), dump, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(dump)
	manifest["db_file"] = "dump.json"
	manifest["db_sha256"] = hex.EncodeToString(sum[:])
	raw, _ = json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	res := application.ApplyPendingRestore(root, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if res == nil || res.OK || !res.RollbackOK || res.Error == "" {
		t.Fatalf("failed restore did not report a successful rollback: %+v", res)
	}
	// Validation failure retains recoverable intent and blocks normal opening until
	// the operator repairs the source or explicitly cancels the untouched request.
	if err := application.CancelMaintenance(root); err != nil {
		t.Fatal(err)
	}
	svc2, err := application.Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	if svc2.Doc().Limits.MonthlyBudgetUSD != "7.00" {
		t.Fatalf("rollback did not restore the pre-restore config: %s", svc2.Doc().Limits.MonthlyBudgetUSD)
	}
}

func TestBackupVerifyRejectsSymlinkDirectory(t *testing.T) {
	svc := setupSvc(t)
	external := t.TempDir()
	link := filepath.Join(svc.DataRoot, "backups", "backup-evil")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r := exec(svc, "backup.verify", map[string]any{"name": "backup-evil"})
	if r.OK || !strings.Contains(r.Error.Message, "real directory") {
		t.Fatalf("symlink backup accepted: %+v", r)
	}
}

func TestConfigSnapshotsRestoreKeepsIdentity(t *testing.T) {
	svc := setupSvc(t)
	snap := exec(svc, "config.snapshot", map[string]any{"name": "before experiments"})
	if !snap.OK {
		t.Fatal(snap.Error)
	}
	patch(t, svc, []any{"limits", "monthly_budget_usd"}, "1.00")
	id := snap.Output["id"].(string)
	dry := out(t, svc, "config.snapshot.restore", map[string]any{"id": id, "dry_run": true})
	if dry["changed"] != true {
		t.Fatal("dry run must report the difference")
	}
	if r := exec(svc, "config.snapshot.restore", map[string]any{"id": id}); !r.OK {
		t.Fatal(r.Error)
	}
	if svc.Doc().Limits.MonthlyBudgetUSD != "10.00" || svc.Doc().Instance.ID != "gate" {
		t.Fatalf("restored %+v %s", svc.Doc().Limits, svc.Doc().Instance.ID)
	}
	list := out(t, svc, "config.snapshots", nil)["snapshots"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["same_as_current"] != true {
		t.Fatalf("snapshots: %v", list)
	}
}

func TestServiceStopsBeforeSetup(t *testing.T) {
	svc, err := application.Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.Daemon = true
	if r := exec(svc, "runtime.stop", nil); !r.OK {
		t.Fatalf("a fresh install must be stoppable: %+v", r.Error)
	}
	if l := <-svc.LifecycleRequests(); l != application.Lifecycle("stop") {
		t.Fatalf("lifecycle %q", l)
	}
	if r := exec(svc, "runtime.restart", nil); !r.OK {
		t.Fatalf("restart before setup: %+v", r.Error)
	}
}

func TestUptimeHistoryRecordsCrash(t *testing.T) {
	svc := setupSvc(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.RunScheduler(ctx) // start
	svc.RunScheduler(ctx) // start again without a stop: the first run crashed
	svc.NoteStop(false)
	h := out(t, svc, "runtime.history", nil)
	if h["crashes"].(float64) != 1 {
		t.Fatalf("crashes: %v", h)
	}
	kinds := []string{}
	for _, e := range h["events"].([]any) {
		kinds = append(kinds, e.(map[string]any)["kind"].(string))
	}
	if strings.Join(kinds, ",") != "stop,start,crash,start" {
		t.Fatalf("events: %v", kinds)
	}
}

func TestImportPreviewAndPeopleAndReviews(t *testing.T) {
	svc := setupSvc(t)
	path := filepath.Join(svc.DataRoot, "chat.csv")
	body := "\xef\xbb\xbfDate,User,Message\n2021-12-14 18:11:17,철수,안녕\n2021-12-15 09:00:00,영희,가평 가자\n2021-12-15 09:01:00,철수,좋아\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := out(t, svc, "imports.preview", map[string]any{"path": path, "room": "kakao:friends"})
	if p["rows"].(float64) != 3 || p["speakers"].(float64) != 2 || p["new"].(float64) != 3 || p["first"] != "2021-12-14T18:11:17" {
		t.Fatalf("preview: %v", p)
	}
	if r := exec(svc, "imports.start", map[string]any{"source": "kakao_csv", "path": path, "room": "kakao:friends"}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := exec(svc, "imports.start", map[string]any{"source": "sandbox_vectors", "path": t.TempDir()}); r.OK {
		t.Fatal("processed sandbox artifacts must not be accepted as product input")
	}
	p = out(t, svc, "imports.preview", map[string]any{"path": path, "room": "kakao:friends"})
	if p["duplicate_file"] != true || p["already_imported"].(float64) != 3 {
		t.Fatalf("second preview must see the import: %v", p)
	}
	people := out(t, svc, "people.list", nil)
	if len(people["speakers"].([]any)) != 2 {
		t.Fatalf("speakers: %v", people)
	}
	if r := exec(svc, "people.merge", map[string]any{"name": "철수", "aliases": []any{"김철수", "cheolsu", "철수"}}); !r.OK {
		t.Fatal(r.Error)
	}
	if got := svc.Doc().Memory.PersonAliases["철수"]; strings.Join(got, ",") != "김철수,cheolsu" {
		t.Fatalf("aliases %v", got)
	}
	if r := exec(svc, "people.merge", map[string]any{"name": "철수", "remove": true}); !r.OK || svc.Doc().Memory.PersonAliases != nil {
		t.Fatalf("remove: %+v %v", r.Error, svc.Doc().Memory.PersonAliases)
	}
	if r := exec(svc, "memory.review", map[string]any{"leaf_id": "nope", "kind": "exclude"}); r.OK {
		t.Fatal("unknown chunk accepted")
	}
	if r := exec(svc, "memory.review", map[string]any{"leaf_id": "x", "kind": "correct"}); r.OK {
		t.Fatal("a correction needs a note")
	}
	if r := exec(svc, "eval.run", nil); r.OK {
		t.Fatal("eval without judged questions must explain itself")
	}
	if d := out(t, svc, "eval.run", map[string]any{"dry_run": true}); d["cases"].(float64) != 0 {
		t.Fatalf("dry run %v", d)
	}
}

// Provider daily counts live in the database: a config change (new registry) and a restart keep them.
func TestProviderQuotaSurvivesConfigChangeAndRestart(t *testing.T) {
	svc := setupSvc(t)
	svc.Registry().Quota.Reserve("kma")
	svc.Registry().Quota.Reserve("kma")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"limits", "budget_alert_percent"}, "value": 70}}}); !r.OK {
		t.Fatalf("patch %+v", r.Error)
	}
	if st, _ := svc.Registry().Status("kma"); st.UsedToday != 2 {
		t.Fatalf("config change reset the count: %d", st.UsedToday)
	}
	root := svc.DataRoot
	_ = svc.Close()
	again, err := application.Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if st, _ := again.Registry().Status("kma"); st.UsedToday != 2 {
		t.Fatalf("restart reset the count: %d", st.UsedToday)
	}
}

// The history shows what a change did, setting by setting.
func TestConfigChangesBetweenRevisions(t *testing.T) {
	svc := setupSvc(t)
	before := svc.Revision()
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{
		map[string]any{"path": []any{"limits", "monthly_budget_usd"}, "value": "25.00"},
		map[string]any{"path": []any{"memory", "languages"}, "value": []any{"ko", "en"}},
		map[string]any{"path": []any{"instance", "bot_names"}, "op": "unset"},
	}}); !r.OK {
		t.Fatal(r.Error)
	}
	r := exec(svc, "config.changes", map[string]any{"from": before})
	if !r.OK {
		t.Fatal(r.Error)
	}
	got := map[string][2]string{}
	for _, c := range jsonShape(r.Output)["changes"].([]any) {
		m := c.(map[string]any)
		got[m["path"].(string)] = [2]string{fmt.Sprint(m["before"]), fmt.Sprint(m["after"])}
	}
	if got["limits.monthly_budget_usd"] != [2]string{"10.00", "25.00"} {
		t.Fatalf("budget change %v", got)
	}
	if got["memory.languages"][1] != "[ko en]" {
		t.Fatalf("list change %v", got["memory.languages"])
	}
	if _, ok := got["instance.bot_names"]; !ok || got["instance.bot_names"][1] != "<nil>" {
		t.Fatalf("removed setting %v", got["instance.bot_names"])
	}
	if len(got) != 3 {
		t.Fatalf("only the changed settings: %v", got)
	}
	for _, bad := range []map[string]any{{"from": 0}, {"from": before, "to": before}, {"from": 999}} {
		if r := exec(svc, "config.changes", bad); r.OK {
			t.Fatalf("%v must be refused", bad)
		}
	}
}

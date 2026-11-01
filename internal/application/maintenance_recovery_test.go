package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/i18n"
)

// R6: a committed restore whose old, set-aside database cannot be deleted (on Windows a scanner or
// indexer holds it) is a success: the bot starts, the leftovers are retried later.
func TestCommittedRestoreSurvivesCleanupThatCannotDelete(t *testing.T) {
	s, _ := stageRestoreTest(t)
	maintenanceHook = func(p string) error {
		if p != "committed" {
			return nil
		}
		// a non-empty folder under the old database's name cannot be removed by a plain delete,
		// on any platform (a held file cannot be removed on Windows)
		old, _ := filepath.Glob(filepath.Join(s.DataRoot, ".masamong.db.old-*"))
		for _, f := range old {
			_ = os.Remove(f)
			_ = os.MkdirAll(filepath.Join(f, "held"), 0o700)
			_ = os.WriteFile(filepath.Join(f, "held", "x"), []byte("x"), 0o600)
		}
		return nil
	}
	err := RecoverMaintenance(s.DataRoot, slog.Default())
	maintenanceHook = nil
	if err != nil {
		t.Fatalf("a restore that is committed must not keep the bot down because of a leftover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.DataRoot, maintenancePending)); !os.IsNotExist(err) {
		t.Fatal("the journal must be gone: the operation is complete")
	}
	if err := verifyRestorePair(s.Store.Path(), s.CfgPath); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.DataRoot, restoreResult))
	var res RestoreResult
	if json.Unmarshal(raw, &res) != nil || !res.OK || res.Warning == "" {
		t.Fatalf("the result is ok with a warning about the leftover: %s", raw)
	}
	if left := recoveryLeftovers(s.DataRoot); len(left) == 0 {
		t.Fatal("the test did not leave a file behind")
	}
	again, err := Open(s.DataRoot, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Doc().Limits.MonthlyBudgetUSD != "10.00" {
		t.Fatalf("restored data not live: %s", again.Doc().Limits.MonthlyBudgetUSD)
	}
	// the leftovers are removed later, and doctor lists them until then
	again.leftovers.Store(&[]string{"x"})
	doctor := again.Execute(context.Background(), action.Request{Action: "runtime.doctor"})
	if raw, _ := json.Marshal(doctor.Output); !strings.Contains(string(raw), "maintenance_leftovers") {
		t.Fatalf("doctor does not list the leftovers: %s", raw)
	}
	again.cleanDataLeftovers()
	if len(recoveryLeftovers(again.DataRoot)) != 0 || len(again.leftoverList()) != 0 {
		t.Fatal("the retry did not remove the leftover")
	}
}

// R7: a restore that fails before touching live files clears its own intent, records the failure and the
// daemon keeps running on the current data.
func TestRestoreThatFailsBeforeTouchingLiveFilesKeepsTheBotRunning(t *testing.T) {
	s, name := stageRestoreTest(t)
	if err := os.RemoveAll(filepath.Join(s.DataRoot, "backups", name)); err != nil {
		t.Fatal(err)
	}
	before, _ := fileSHA256(s.Store.Path())
	if err := RecoverMaintenance(s.DataRoot, slog.Default()); err != nil {
		t.Fatalf("startup must go on: %v", err)
	}
	if !hashMatches(s.Store.Path(), before) {
		t.Fatal("live database changed")
	}
	if maintenanceNeeded(s.DataRoot) {
		t.Fatal("the intent must be cleared")
	}
	again, err := Open(s.DataRoot, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.noteRestoreResult()
	var last RestoreResult
	if ok, _ := again.kvGet("restore:last", &last); !ok || last.OK || last.Error == "" || last.Name != name {
		t.Fatalf("the failure is recorded for the console: %+v", last)
	}
	doctor := again.Execute(context.Background(), action.Request{Action: "runtime.doctor"})
	if raw, _ := json.Marshal(doctor.Output); !strings.Contains(string(raw), "restore_failed") {
		t.Fatalf("doctor does not mention the failed restore: %s", raw)
	}
}

// R9: the journal's folder may be spelled in another letter case (Windows, default macOS) or through a
// link; it is still the same folder. Cancel never hides data that was already set aside.
func TestJournalRootIsComparedByIdentityNotByBytes(t *testing.T) {
	s, _ := stageRestoreTest(t)
	interruptMaintenance(t, "install")
	_ = ApplyPendingRestore(s.DataRoot, slog.Default())
	maintenanceHook = nil
	spellings := map[string]string{}
	if alt := swapCase(s.DataRoot); alt != s.DataRoot {
		if _, err := os.Stat(alt); err == nil {
			spellings["other letter case"] = alt
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(s.DataRoot, link); err == nil {
		spellings["through a link"] = link
	}
	if len(spellings) == 0 {
		t.Skip("this platform has neither case-insensitive folders nor links")
	}
	for name, root := range spellings {
		t.Run(name, func(t *testing.T) {
			status := MaintenanceStatus(root)
			if status["case"] != caseRestoreStarted {
				t.Fatalf("a valid, started journal looks invalid: %v", status)
			}
			cmds, _ := json.Marshal(status["recovery_commands"])
			if strings.Contains(string(cmds), "cancel") {
				t.Fatalf("cancel must not be offered once live files moved: %s", cmds)
			}
			if err := CancelMaintenance(root); err == nil {
				t.Fatal("cancel must refuse after live files were moved")
			}
			if _, err := os.Stat(filepath.Join(s.DataRoot, maintenancePending)); err != nil {
				t.Fatal("cancel removed the journal")
			}
		})
	}
	// retry through the other spelling completes the restore
	for _, root := range spellings {
		if err := RecoverMaintenance(root, slog.Default()); err != nil {
			t.Fatalf("retry through %s: %v", root, err)
		}
		break
	}
	if err := verifyRestorePair(s.Store.Path(), s.CfgPath); err != nil {
		t.Fatal(err)
	}
}

func swapCase(p string) string {
	out := []rune(p)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z':
			out[i] = r - 32
		case r >= 'A' && r <= 'Z':
			out[i] = r + 32
		}
	}
	return string(out)
}

// R9: an unreadable journal is only set aside while nothing shows live data was moved.
func TestCancelRefusesAnUnreadableJournalWhenDataWasSetAside(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, maintenancePending)
	if err := os.WriteFile(journal, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if kind, cmds := MaintenanceCase(root); kind != caseJournalInvalid || len(cmds) != 1 || cmds[0] != "cancel" {
		t.Fatalf("%s %v", kind, cmds)
	}
	recovery := filepath.Join(root, ".masamong.db.old-123456")
	if err := os.WriteFile(recovery, []byte("the previous database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if kind, cmds := MaintenanceCase(root); kind != caseJournalMoved || len(cmds) != 0 {
		t.Fatalf("with recovery files no command can be offered: %s %v", kind, cmds)
	}
	if err := CancelMaintenance(root); err == nil || !strings.Contains(err.Error(), ".masamong.db.old-123456") {
		t.Fatalf("cancel must refuse and name the file: %v", err)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("the journal was moved")
	}
	_ = os.Remove(recovery)
	if err := CancelMaintenance(root); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "maintenance-refused-*.json")); len(left) != 1 {
		t.Fatalf("the unreadable record is kept: %v", left)
	}
}

// R16: only the commands that can work in the current state are offered.
func TestRecoveryCommandsFitTheState(t *testing.T) {
	t.Run("restore requested", func(t *testing.T) {
		s, _ := stageRestoreTest(t)
		kind, cmds := MaintenanceCase(s.DataRoot)
		if kind != caseRestoreRequested || strings.Join(cmds, ",") != "retry,cancel" {
			t.Fatalf("%s %v", kind, cmds)
		}
	})
	t.Run("restore started", func(t *testing.T) {
		s, _ := stageRestoreTest(t)
		interruptMaintenance(t, "install")
		_ = ApplyPendingRestore(s.DataRoot, slog.Default())
		maintenanceHook = nil
		kind, cmds := MaintenanceCase(s.DataRoot)
		if kind != caseRestoreStarted || strings.Join(cmds, ",") != "retry,rollback" {
			t.Fatalf("%s %v", kind, cmds)
		}
	})
	t.Run("old marker", func(t *testing.T) {
		root := t.TempDir()
		_ = os.WriteFile(filepath.Join(root, resetPending), []byte("{}"), 0o600)
		if kind, cmds := MaintenanceCase(root); kind != caseLegacyMarker || strings.Join(cmds, ",") != "cancel" {
			t.Fatalf("a marker without a version can never be retried: %s %v", kind, cmds)
		}
		status := MaintenanceStatus(root)
		if strings.Contains(strings.Join(status["recovery_commands"].([]string), "\n"), "retry") {
			t.Fatalf("%v", status["recovery_commands"])
		}
	})
}

// Every text the maintenance page and status can show exists in every language.
func TestMaintenanceTextsExistInEveryLanguage(t *testing.T) {
	c := i18n.New()
	keys := []string{"maintenance.page.title", "maintenance.page.stopped", "maintenance.page.what", "maintenance.page.todo", "maintenance.page.step_stop",
		"maintenance.page.step_run_one", "maintenance.page.step_run_many", "maintenance.page.step_start", "maintenance.page.none", "maintenance.page.folder",
		"maintenance.page.log", "maintenance.recovery_required", "maintenance.effect.retry.default", "maintenance.effect.rollback.default",
		"maintenance.effect.cancel.default", "maintenance.effect.cancel.aside", "maintenance.effect.abandon-update.default"}
	for _, k := range []string{caseRestoreRequested, caseRestoreStarted, caseResetRequested, caseResetStarted, caseUpdateResumable, caseUpdateStuck,
		caseJournalInvalid, caseJournalMoved, caseLegacyMarker, caseOther} {
		keys = append(keys, "maintenance.case."+k)
	}
	for _, loc := range []string{"ko", "en", "ja", "zh-Hans", "es"} {
		for _, k := range keys {
			if v, ok := c.Lookup(loc, k); !ok || v == "" {
				t.Errorf("%s has no %s", loc, k)
			}
		}
	}
	// the page renders in each, with the data folder's real commands and no leftover placeholder
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, resetPending), []byte("{}"), 0o600)
	for _, loc := range []string{"ko", "en", "ja", "zh-Hans", "es"} {
		page := MaintenancePage(root, loc)
		if !strings.Contains(page, `lang="`+loc+`"`) || strings.Contains(page, "{path}") || strings.Contains(page, "<script") {
			t.Errorf("%s: %s", loc, page)
		}
	}
}

// ----- R8: program-update journal -----

func updateFixture(t *testing.T) (dir string, staged map[string]string) {
	t.Helper()
	dir = t.TempDir()
	staged = map[string]string{}
	for _, base := range []string{"masamong", "masamongd"} {
		name := exeName(base)
		_ = os.WriteFile(filepath.Join(dir, name), []byte("old "+base), 0o755)
		p := filepath.Join(dir, "stage-"+name)
		_ = os.WriteFile(p, []byte("new "+base), 0o755)
		staged[name] = p
	}
	old := selfExecutable
	t.Cleanup(func() { selfExecutable = old })
	selfExecutable = func() (string, error) { return filepath.Join(dir, exeName("masamongd")), nil }
	return dir, staged
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// A program is runnable at every point of a crash: both old programs are never set aside before the
// first replacement is in place (no Masamong program at all after a crash, as the review found).
func TestOneProgramAlwaysRemainsDuringAnUpdate(t *testing.T) {
	for _, point := range []string{"prepared", "quarantine", "quarantine:0", "install:0", "quarantine:1", "install:1", "verify", "committed"} {
		t.Run(point, func(t *testing.T) {
			dir, staged := updateFixture(t)
			interruptMaintenance(t, point)
			if err := swapBinaries(dir, staged); !errors.Is(err, errMaintenanceInterrupted) {
				t.Fatalf("hook %s: %v", point, err)
			}
			cli, daemon := exists(filepath.Join(dir, exeName("masamong"))), exists(filepath.Join(dir, exeName("masamongd")))
			if !cli && !daemon {
				t.Fatal("no Masamong program left after the crash")
			}
			if point != "quarantine:1" && !daemon {
				t.Fatalf("the daemon, which runs the recovery, is missing at %s", point)
			}
		})
	}
}

// The operator reinstalled a different release after a crash. Nothing can resume or roll back, the
// status says so and offers only abandon-update, which keeps every file and clears only the journal.
func TestStaleProgramJournalHasAnOperatorExit(t *testing.T) {
	dir, staged := updateFixture(t)
	interruptMaintenance(t, "quarantine:1") // CLI replaced, the daemon set aside, none installed yet
	if err := swapBinaries(dir, staged); err == nil {
		t.Fatal("expected interruption")
	}
	maintenanceHook = nil
	for _, base := range []string{"masamong", "masamongd"} { // reinstall with the installer: a release 3 pair
		_ = os.Remove(filepath.Join(dir, exeName(base)))
		_ = os.WriteFile(filepath.Join(dir, exeName(base)), []byte("release 3 "+base), 0o755)
	}
	if UpdateJournalState() != updateStuck {
		t.Fatalf("state %q", UpdateJournalState())
	}
	if err := RecoverUpdateAt(dir); err == nil || !strings.Contains(err.Error(), "abandon-update") {
		t.Fatalf("recovery must say what the operator can do: %v", err)
	}
	if handled, err := RollbackPendingUpdate(); !handled || err == nil || !strings.Contains(err.Error(), "abandon-update") {
		t.Fatalf("rollback: %v %v", handled, err)
	}
	if err := CancelMaintenance(t.TempDir()); err == nil || !strings.Contains(err.Error(), "abandon-update") {
		t.Fatalf("cancel: %v", err)
	}
	root := t.TempDir()
	if kind, cmds := MaintenanceCase(root); kind != caseUpdateStuck || strings.Join(cmds, ",") != "abandon-update" {
		t.Fatalf("%s %v", kind, cmds)
	}
	before := dirListing(t, dir)
	kept, err := AbandonPendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	after := dirListing(t, dir)
	delete(before, updatePending)
	delete(after, filepath.Base(kept))
	if len(before) != len(after) {
		t.Fatalf("abandon changed more than the journal:\nbefore %v\nafter  %v", before, after)
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("%s changed", name)
		}
	}
	if maintenanceNeeded(root) {
		t.Fatal("the install is still in maintenance")
	}
	// the stale old/new copies are kept: nothing is cleaned while an abandoned record exists
	if err := CleanUpdateLeftovers(); err != nil {
		t.Fatal(err)
	}
	if len(dirListing(t, dir)) != len(after)+1 {
		t.Fatal("CleanUpdateLeftovers deleted files after an abandon")
	}
}

func dirListing(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out[e.Name()] = string(b)
	}
	return out
}

// Live files that already are one complete pair settle the journal by themselves.
func TestUpdateJournalSettlesWhenTheLiveFilesAreACompletePair(t *testing.T) {
	dir, staged := updateFixture(t)
	interruptMaintenance(t, "prepared")
	if err := swapBinaries(dir, staged); err == nil {
		t.Fatal("expected interruption")
	}
	maintenanceHook = nil
	// the operator installed the new release by hand and the staged copies are gone
	stages, _ := filepath.Glob(filepath.Join(dir, ".*.new-*"))
	for _, p := range stages {
		_ = os.Remove(p)
	}
	for _, base := range []string{"masamong", "masamongd"} {
		_ = os.Remove(filepath.Join(dir, exeName(base)))
		_ = os.WriteFile(filepath.Join(dir, exeName(base)), []byte("new "+base), 0o755)
	}
	if UpdateJournalState() != updateNewPair {
		t.Fatalf("state %q", UpdateJournalState())
	}
	if err := RecoverUpdateAt(dir); err != nil {
		t.Fatalf("a complete new pair needs no operator: %v", err)
	}
	if exists(filepath.Join(dir, updatePending)) {
		t.Fatal("the journal was not cleared")
	}
}

func TestAbandonRefusesWhenNoProgramIsInstalled(t *testing.T) {
	dir, staged := updateFixture(t)
	interruptMaintenance(t, "prepared")
	_ = swapBinaries(dir, staged)
	maintenanceHook = nil
	for _, base := range []string{"masamong", "masamongd"} {
		_ = os.Remove(filepath.Join(dir, exeName(base)))
	}
	if _, err := AbandonPendingUpdate(); err == nil {
		t.Fatal("abandoning with no program installed would leave nothing to run")
	}
	if !exists(filepath.Join(dir, updatePending)) {
		t.Fatal("the journal must stay")
	}
}

// ----- R13 -----

func TestInterruptedBackupStagingIsCleanedAtStartup(t *testing.T) {
	s := maintenanceService(t, false)
	staging := filepath.Join(s.backupDir(), ".backup-staging-abc123")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(staging, "masamong.db"), []byte("a full copy of the database"), 0o600)
	if names := s.Execute(context.Background(), action.Request{Action: "backup.list"}); !names.OK {
		t.Fatal(names.Error)
	}
	s.cleanDataLeftovers()
	if exists(staging) {
		t.Fatal("a crash left a full database copy that is never removed")
	}
	if len(s.leftoverList()) != 0 {
		t.Fatalf("%v", s.leftoverList())
	}
	// while a maintenance journal exists nothing is touched
	_ = os.MkdirAll(staging, 0o700)
	_ = os.WriteFile(filepath.Join(s.DataRoot, resetPending), []byte("{}"), 0o600)
	if left := CleanDataLeftovers(s.DataRoot); len(left) != 0 || !exists(staging) {
		t.Fatalf("cleanup ran during maintenance: %v", left)
	}
}

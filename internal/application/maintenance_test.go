package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
	"masamong/internal/fsutil"
	"masamong/internal/storage"
)

// The test executable doubles as a portable staged-program fixture. This executes
// before testing's flag parser, just like the daemon's side-effect-free --version.
func init() {
	if v := os.Getenv("_MASAMONG_TEST_PROGRAM_VERSION"); v != "" {
		if len(os.Args) > 1 && os.Args[1] == "--version" && os.Getenv("_MASAMONG_TEST_BAD_DAEMON") != "" {
			os.Exit(2)
		}
		fmt.Printf("{\"output\":{\"version\":%q}}\n", v)
		os.Exit(0)
	}
	if os.Getenv("_MASAMONG_TEST_PROGRAM_SLEEP") == "1" {
		fmt.Println("ready")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func maintenanceService(t *testing.T, custom bool) *Service {
	t.Helper()
	root := t.TempDir()
	if custom {
		if err := storage.SaveSettings(root, storage.Settings{Backend: "sqlite", SQLiteFile: "custom.db"}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: json.RawMessage(`{"instance":"maintenance-test"}`), Locale: "en"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	s.Daemon = true
	return s
}

func maintenanceAction(t *testing.T, s *Service, name string, input any) action.Result {
	t.Helper()
	b, _ := json.Marshal(input)
	r := s.Execute(context.Background(), action.Request{Action: name, Input: b, Locale: "en"})
	if !r.OK {
		t.Fatalf("%s: %+v", name, r.Error)
	}
	return r
}

func interruptMaintenance(t *testing.T, point string) {
	t.Helper()
	maintenanceHook = func(p string) error {
		if p == point {
			return errMaintenanceInterrupted
		}
		return nil
	}
	t.Cleanup(func() { maintenanceHook = nil })
}

func stageRestoreTest(t *testing.T) (*Service, string) {
	t.Helper()
	s := maintenanceService(t, false)
	b := maintenanceAction(t, s, "backup.create", nil)
	name := b.Output["name"].(string)
	maintenanceAction(t, s, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"limits", "monthly_budget_usd"}, "value": "7.00"}}})
	maintenanceAction(t, s, "backup.restore", map[string]any{"name": name, "confirm": name})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, name
}

func TestConcurrentBackupsAllSurvive(t *testing.T) {
	s := maintenanceService(t, false)
	s.Clock = clock.Fixed{T: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	var wg sync.WaitGroup
	names := make(chan string, 32)
	errs := make(chan error, 32)
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, _, err := s.createBackup(backupManual)
			if err != nil {
				errs <- err
			} else {
				names <- n
			}
		}()
	}
	close(start)
	wg.Wait()
	close(names)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]bool{}
	for n := range names {
		if seen[n] {
			t.Errorf("shared directory: %s", n)
		}
		seen[n] = true
		if _, err := verifyBackupDir(filepath.Join(s.backupDir(), n)); err != nil {
			t.Error(err)
		}
	}
	if len(seen) != 32 {
		t.Fatalf("surviving backups=%d, want 32", len(seen))
	}
}

func TestBackupTakesConfigFromSnapshot(t *testing.T) {
	s := maintenanceService(t, false)
	oldRev := s.Revision()
	maintenanceHook = func(p string) error {
		if p == "backup:snapshot" {
			maintenanceAction(t, s, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"limits", "monthly_budget_usd"}, "value": "7.00"}}})
		}
		return nil
	}
	t.Cleanup(func() { maintenanceHook = nil })
	n, m, err := s.createBackup(backupManual)
	if err != nil {
		t.Fatal(err)
	}
	if m["config_revision"] != oldRev || s.Revision() != oldRev+1 {
		t.Fatalf("snapshot revision=%v live=%d", m["config_revision"], s.Revision())
	}
	dir := filepath.Join(s.backupDir(), n)
	if _, err := verifyBackupDir(dir); err != nil {
		t.Fatal(err)
	}
	// Even correctly updated hashes cannot conceal disagreement with the committed DB.
	raw, err := os.ReadFile(s.CfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m["config_sha256"], _ = fileSHA256(filepath.Join(dir, "config.yaml"))
	b, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600)
	if _, err := verifyBackupDir(dir); err == nil {
		t.Fatal("accepted config from a different committed revision")
	}
}

func TestInvalidSQLiteMatchingChecksumRefused(t *testing.T) {
	s := maintenanceService(t, false)
	n, m, err := s.createBackup(backupManual)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.backupDir(), n)
	_ = os.WriteFile(filepath.Join(dir, "masamong.db"), []byte("not SQLite"), 0o600)
	m["db_sha256"], _ = fileSHA256(filepath.Join(dir, "masamong.db"))
	b, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600)
	if _, err := verifyBackupDir(dir); err == nil {
		t.Fatal("accepted invalid SQLite with matching hash")
	}
}

func TestRestoreRecoversEveryPhase(t *testing.T) {
	points := []string{"restore:before-copy", "restore:copy", "prepared", "quarantine", "quarantine:0", "quarantine:1", "quarantine:2", "quarantine:3", "quarantine:4", "install", "install:0", "install:4", "verify", "committed"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			s, _ := stageRestoreTest(t)
			for _, side := range []string{"-wal", "-shm", "-journal"} {
				if err := os.WriteFile(s.Store.Path()+side, []byte("crash recovery "+side), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			interruptMaintenance(t, point)
			res := ApplyPendingRestore(s.DataRoot, slog.Default())
			if res == nil || res.OK || !strings.Contains(res.Error, "interrupted") {
				t.Fatalf("hook not reached: %+v", res)
			}
			if _, err := os.Stat(filepath.Join(s.DataRoot, maintenancePending)); err != nil {
				t.Fatal("journal consumed before completion", err)
			}
			maintenanceHook = nil
			if err := RecoverMaintenance(s.DataRoot, slog.Default()); err != nil {
				t.Fatal(err)
			}
			again, err := Open(s.DataRoot, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			defer again.Close()
			if again.Doc().Limits.MonthlyBudgetUSD != "10.00" {
				t.Fatalf("mixed restored pair: %s", again.Doc().Limits.MonthlyBudgetUSD)
			}
		})
	}
}

func TestRestoreCopyFailurePreservesCrashWAL(t *testing.T) {
	s, name := stageRestoreTest(t)
	wal := s.Store.Path() + "-wal"
	if err := os.WriteFile(wal, []byte("uncheckpointed committed data"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := fileSHA256(s.Store.Path())
	// Source cannot be validated/copied; live DB and WAL must remain exactly intact.
	if err := os.Remove(filepath.Join(s.backupDir(), name, "masamong.db")); err != nil {
		t.Fatal(err)
	}
	res := ApplyPendingRestore(s.DataRoot, slog.Default())
	if res == nil || res.OK {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(wal); string(b) != "uncheckpointed committed data" {
		t.Fatal("crash-left WAL lost")
	}
	if !hashMatches(s.Store.Path(), before) {
		t.Fatal("copy failure changed DB")
	}
	// nothing live was touched, so nothing needs recovery: the failure is recorded and startup is ordinary
	if _, err := os.Stat(filepath.Join(s.DataRoot, maintenancePending)); !os.IsNotExist(err) {
		t.Fatal("a restore that never touched live files left its intent behind")
	}
	if err := RecoverMaintenance(s.DataRoot, slog.Default()); err != nil {
		t.Fatalf("a failed preparation must not keep the bot down: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(s.DataRoot, "maintenance-refused-*.json")); len(left) != 1 {
		t.Fatalf("the refused journal is kept for inspection: %v", left)
	}
	raw, err := os.ReadFile(filepath.Join(s.DataRoot, restoreResult))
	if err != nil || !strings.Contains(string(raw), `"ok":false`) || !strings.Contains(string(raw), `"error"`) {
		t.Fatalf("the failure is recorded in the result file: %s %v", raw, err)
	}
}

func TestRollbackResumesAfterEveryMove(t *testing.T) {
	for _, point := range []string{"rollback", "rollback:0", "rollback:1", "rollback:2", "rollback:3", "rollback:4", "rolled_back"} {
		t.Run(point, func(t *testing.T) {
			s, _ := stageRestoreTest(t)
			for _, side := range []string{"-wal", "-shm", "-journal"} {
				_ = os.WriteFile(s.Store.Path()+side, []byte("old "+side), 0o600)
			}
			interruptMaintenance(t, "verify")
			_ = ApplyPendingRestore(s.DataRoot, slog.Default())
			maintenanceHook = func(p string) error {
				if p == point {
					return errMaintenanceInterrupted
				}
				return nil
			}
			if err := RollbackMaintenance(s.DataRoot); !errors.Is(err, errMaintenanceInterrupted) {
				t.Fatalf("rollback hook %s: %v", point, err)
			}
			maintenanceHook = nil
			if err := RecoverMaintenance(s.DataRoot, slog.Default()); err != nil {
				t.Fatal(err)
			}
			for _, side := range []string{"-wal", "-shm", "-journal"} {
				if b, _ := os.ReadFile(s.Store.Path() + side); string(b) != "old "+side {
					t.Fatalf("rollback lost %s", side)
				}
			}
			if err := verifyRestorePair(s.Store.Path(), s.CfgPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMalformedResetNeverDeletes(t *testing.T) {
	for _, body := range []string{"", "{", "{}", `{"version":1,"kind":"reset"}`} {
		t.Run(body, func(t *testing.T) {
			s := maintenanceService(t, false)
			_ = s.Close()
			before, _ := fileSHA256(s.Store.Path())
			_ = os.WriteFile(filepath.Join(s.DataRoot, maintenancePending), []byte(body), 0o600)
			if ApplyPendingReset(s.DataRoot, slog.Default()) {
				t.Fatal("malformed reset accepted")
			}
			if err := RecoverMaintenance(s.DataRoot, slog.Default()); err == nil {
				t.Fatal("malformed journal allowed normal startup")
			}
			if !hashMatches(s.Store.Path(), before) {
				t.Fatal("malformed reset deleted DB")
			}
			if err := CancelMaintenance(s.DataRoot); err != nil {
				t.Fatal(err)
			}
			if !hashMatches(s.Store.Path(), before) {
				t.Fatal("cancellation deleted DB")
			}
		})
	}
	t.Run("legacy", func(t *testing.T) {
		s := maintenanceService(t, false)
		_ = s.Close()
		_ = os.WriteFile(filepath.Join(s.DataRoot, resetPending), []byte("{"), 0o600)
		if err := RecoverMaintenance(s.DataRoot, slog.Default()); err == nil {
			t.Fatal("malformed legacy reset accepted")
		}
		if _, err := os.Stat(s.Store.Path()); err != nil {
			t.Fatal("DB deleted")
		}
	})
}

func TestResetCustomDatabaseAndOwnedSourcesResumes(t *testing.T) {
	points := []string{"erase", "committed"}
	for i := range resetTargets(t.TempDir(), filepath.Join(t.TempDir(), "x.db"), true) {
		points = append(points, fmt.Sprintf("erase:%d", i))
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			s := maintenanceService(t, true)
			root := s.DataRoot
			external := filepath.Join(t.TempDir(), "referenced.csv")
			_ = os.WriteFile(external, []byte("external source"), 0o600)
			for _, name := range []string{"imports", "rag_staging", "rag/channels/g/c", "notices", "memory/blocks", "images", "practice", "secrets"} {
				dir := filepath.Join(root, name)
				_ = os.MkdirAll(dir, 0o700)
				_ = os.WriteFile(filepath.Join(dir, "owned"), []byte("owned"), 0o600)
			}
			_ = os.Symlink(external, filepath.Join(root, "imports", "external-link"))
			maintenanceAction(t, s, "instance.reset", map[string]any{"confirm": "reset", "backup": false, "delete_keys": true})
			_ = s.Close()
			interruptMaintenance(t, point)
			if err := RecoverMaintenance(root, slog.Default()); !errors.Is(err, errMaintenanceInterrupted) {
				t.Fatalf("reset hook %s: %v", point, err)
			}
			if _, err := os.Stat(filepath.Join(root, maintenancePending)); err != nil {
				t.Fatal("reset journal disappeared")
			}
			maintenanceHook = nil
			if err := RecoverMaintenance(root, slog.Default()); err != nil {
				t.Fatal(err)
			}
			for _, p := range resetTargets(root, s.Store.Path(), true) {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("owned reset target survived: %s (%v)", p, err)
				}
			}
			if b, _ := os.ReadFile(external); string(b) != "external source" {
				t.Fatal("external reference erased")
			}
			again, err := Open(root, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			defer again.Close()
			if again.Ready() {
				t.Fatal("custom DB survived reset")
			}
		})
	}
}

func TestUpdateRecoversEveryPhase(t *testing.T) {
	// programs are replaced one at a time: quarantine:0, install:0, quarantine:1, install:1
	for _, point := range []string{"prepared", "quarantine", "quarantine:0", "install:0", "quarantine:1", "install:1", "verify", "committed"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			staged := map[string]string{}
			for _, base := range []string{"masamong", "masamongd"} {
				name := exeName(base)
				_ = os.WriteFile(filepath.Join(dir, name), []byte("old "+base), 0o755)
				p := filepath.Join(dir, "stage-"+name)
				_ = os.WriteFile(p, []byte("new "+base), 0o755)
				staged[name] = p
			}
			interruptMaintenance(t, point)
			if err := swapBinaries(dir, staged); !errors.Is(err, errMaintenanceInterrupted) {
				t.Fatalf("hook %s: %v", point, err)
			}
			maintenanceHook = nil
			if err := RecoverUpdateAt(dir); err != nil {
				t.Fatal(err)
			}
			for _, base := range []string{"masamong", "masamongd"} {
				b, _ := os.ReadFile(filepath.Join(dir, exeName(base)))
				if string(b) != "new "+base {
					t.Fatal("mixed program pair")
				}
			}
		})
	}
}

func TestStageBinariesRunsBothPrograms(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{exeName("masamong"): b, exeName("masamongd"): b}
	t.Setenv("_MASAMONG_TEST_PROGRAM_VERSION", "0.3.0")
	t.Setenv("_MASAMONG_TEST_BAD_DAEMON", "1")
	if _, err := stageBinaries(context.Background(), t.TempDir(), files, "0.3.0"); err == nil {
		t.Fatal("non-running staged daemon accepted")
	}
	t.Setenv("_MASAMONG_TEST_BAD_DAEMON", "")
	staged, err := stageBinaries(context.Background(), t.TempDir(), files, "0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	removeStaged(staged)
}

func TestJournalValidatesPathsBeforeDestruction(t *testing.T) {
	s := maintenanceService(t, true)
	_ = s.Close()
	j, err := newMaintenance(s.DataRoot, "reset")
	if err != nil {
		t.Fatal(err)
	}
	j.DBPath = filepath.Join(t.TempDir(), "external.db")
	_ = os.WriteFile(j.DBPath, []byte("external"), 0o600)
	j.Targets = resetTargets(j.Root, j.DBPath, false)
	if err := saveMaintenance(filepath.Join(s.DataRoot, maintenancePending), &j, "requested"); err != nil {
		t.Fatal(err)
	}
	if err := RecoverMaintenance(s.DataRoot, slog.Default()); err == nil {
		t.Fatal("external reset plan accepted")
	}
	if b, _ := os.ReadFile(j.DBPath); string(b) != "external" {
		t.Fatal("external database erased")
	}
	_ = fsutil.RemoveDurable(filepath.Join(s.DataRoot, maintenancePending))
}

func TestRollbackFailureBlocksOpenAndReportsDoctor(t *testing.T) {
	s, _ := stageRestoreTest(t)
	maintenanceHook = func(point string) error {
		if point == "verify" {
			return errors.New("installed verification failed")
		}
		if point == "rollback" {
			return errors.New("rollback journal write failed")
		}
		return nil
	}
	t.Cleanup(func() { maintenanceHook = nil })
	r := ApplyPendingRestore(s.DataRoot, slog.Default())
	if r == nil || r.RollbackOK || !strings.Contains(r.RollbackError, "rollback journal write failed") {
		t.Fatalf("rollback error suppressed: %+v", r)
	}
	j, err := readMaintenance(filepath.Join(s.DataRoot, maintenancePending), s.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !hashMatches(j.Entries[0].Old, j.Entries[0].OldHash) {
		t.Fatal("rollback failure deleted original DB")
	}
	blocked, err := Open(s.DataRoot, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	if blocked.Store != nil {
		t.Fatal("opened live database during failed rollback")
	}
	doctor := maintenanceAction(t, blocked, "runtime.doctor", nil)
	if doctor.Output["maintenance"] != true || doctor.Output["recovery_commands"] == nil {
		t.Fatalf("doctor hid maintenance: %+v", doctor)
	}
	if r := blocked.Execute(context.Background(), action.Request{Action: "setup.resume"}); r.OK {
		t.Fatal("setup overwrote an incomplete restore")
	}
	maintenanceHook = nil
	if err := RecoverMaintenance(s.DataRoot, slog.Default()); err != nil {
		t.Fatal(err)
	}
	committed, _, err := snapshotConfig(s.Store.Path(), "masamong.db")
	if err != nil || !strings.Contains(string(committed), "7.00") {
		t.Fatalf("old committed config not recovered: %v", err)
	}
}

func TestProgramRollbackFailureRetainsRecoveryCopies(t *testing.T) {
	dir := t.TempDir()
	staged := map[string]string{}
	for _, base := range []string{"masamong", "masamongd"} {
		name := exeName(base)
		_ = os.WriteFile(filepath.Join(dir, name), []byte("old "+base), 0o755)
		p := filepath.Join(dir, "stage-"+name)
		_ = os.WriteFile(p, []byte("new "+base), 0o755)
		staged[name] = p
	}
	maintenanceHook = func(point string) error {
		if point == "verify" {
			return errors.New("verify failed")
		}
		if point == "rollback" {
			return errors.New("rollback failed")
		}
		return nil
	}
	t.Cleanup(func() { maintenanceHook = nil })
	if err := swapBinaries(dir, staged); err == nil || !strings.Contains(err.Error(), "program rollback failed") {
		t.Fatalf("rollback error suppressed: %v", err)
	}
	j, err := readMaintenance(filepath.Join(dir, updatePending), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range j.Entries {
		if !hashMatches(f.Old, f.OldHash) {
			t.Fatal("incomplete pair lost a recovery copy")
		}
	}
	maintenanceHook = nil
	if err := RecoverUpdateAt(dir); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"masamong", "masamongd"} {
		if b, _ := os.ReadFile(filepath.Join(dir, exeName(base))); string(b) != "old "+base {
			t.Fatal("rollback left mixed programs")
		}
	}
}

func TestCommittedUpdateCleanupChecksPairCompleteness(t *testing.T) {
	dir := t.TempDir()
	staged := map[string]string{}
	for _, base := range []string{"masamong", "masamongd"} {
		name := exeName(base)
		_ = os.WriteFile(filepath.Join(dir, name), []byte("old "+base), 0o755)
		p := filepath.Join(dir, "stage-"+name)
		_ = os.WriteFile(p, []byte("new "+base), 0o755)
		staged[name] = p
	}
	interruptMaintenance(t, "committed")
	if err := swapBinaries(dir, staged); !errors.Is(err, errMaintenanceInterrupted) {
		t.Fatal(err)
	}
	j, err := readMaintenance(filepath.Join(dir, updatePending), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(j.Entries[0].Target)
	maintenanceHook = nil
	if err := RecoverUpdateAt(dir); err == nil {
		t.Fatal("incomplete committed pair accepted")
	}
	for _, f := range j.Entries {
		if !hashMatches(f.Old, f.OldHash) {
			t.Fatal("cleanup deleted needed recovery file")
		}
	}
}

func TestLogicalBackupIsValidatedInIsolation(t *testing.T) {
	s := maintenanceService(t, false)
	dump, err := s.Store.ExportLogical()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dump.json")
	if err := os.WriteFile(path, dump, 0o600); err != nil {
		t.Fatal(err)
	}
	_, rev, err := validateLogicalSnapshot(path)
	if err != nil || rev != s.Revision() {
		t.Fatalf("valid logical snapshot refused: rev=%d err=%v", rev, err)
	}
}

func TestProgramMaintenanceStatusDoesNotRequireDataJournal(t *testing.T) {
	dir := t.TempDir()
	staged := map[string]string{}
	for _, base := range []string{"masamong", "masamongd"} {
		name := exeName(base)
		_ = os.WriteFile(filepath.Join(dir, name), []byte("old "+base), 0o755)
		p := filepath.Join(dir, "stage-"+name)
		_ = os.WriteFile(p, []byte("new "+base), 0o755)
		staged[name] = p
	}
	interruptMaintenance(t, "prepared")
	if err := swapBinaries(dir, staged); !errors.Is(err, errMaintenanceInterrupted) {
		t.Fatal(err)
	}
	maintenanceHook = nil
	previous := selfExecutable
	selfExecutable = func() (string, error) { return filepath.Join(dir, exeName("masamongd")), nil }
	t.Cleanup(func() { selfExecutable = previous })
	root := t.TempDir()
	status := MaintenanceStatus(root)
	if status["maintenance"] != true || status["operation"] != "update" || status["phase"] != "prepared" || status["error"] != nil {
		t.Fatalf("program recovery status: %v", status)
	}
	blocked, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	doctor := maintenanceAction(t, blocked, "runtime.doctor", nil)
	if doctor.Output["operation"] != "update" {
		t.Fatalf("offline doctor lost the update journal: %v", doctor.Output)
	}
	if handled, err := RollbackPendingUpdate(); !handled || err != nil {
		t.Fatalf("offline program rollback: handled=%v err=%v", handled, err)
	}
}

// A data root reached through a link (macOS /var -> /private/var, a moved home folder) is the
// operator's choice; a link inside the root still refuses destructive maintenance.
func TestOwnedPathAcceptsLinkedRootRefusesInnerLink(t *testing.T) {
	real := t.TempDir()
	linked := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(real, "imports", "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ownedPath(linked, filepath.Join(linked, "imports", "a", "x.csv")); err != nil {
		t.Fatalf("root behind a link refused: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(real, "memory")); err != nil {
		t.Fatal(err)
	}
	if err := ownedPath(linked, filepath.Join(linked, "memory", "catalog.json")); err == nil {
		t.Fatal("a link inside the root was accepted")
	}
	if err := ownedPath(linked, filepath.Join(filepath.Dir(linked), "elsewhere")); err == nil {
		t.Fatal("a path outside the root was accepted")
	}
}

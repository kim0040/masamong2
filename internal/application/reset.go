package application

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/fsutil"
	"masamong/internal/secrets"
	"masamong/internal/storage"
)

// instance.reset starts the instance over, as if it had just been installed: the database (memory,
// conversations, people, settings history), config.yaml and the practice instance go; the next start
// opens the first-run setup. Like a restore it is staged: the Action writes maintenance-operation.json and
// restarts, and the daemon clears the files before it opens the database again.
//
// What stays: the backups folder (by default a backup is taken first, so the reset can be undone with
// a restore), the logs and the console sign-in. Saved keys stay unless delete_keys is set; then the
// key files the console wrote and this instance's keychain entries go too. Keys read from environment
// variables or other files are never touched.

const (
	resetPending   = "reset-pending.json"
	backupPreReset = "prereset"
	resetConfirm   = "reset" // the word the request must carry, whatever the console's language
)

func (s *Service) instanceReset(req action.Request) action.Result {
	if !s.Daemon {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "the service does not run in this process: reset from the console", nil)
	}
	unlock, err := lockMaintenance(s.DataRoot, false)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer unlock()
	in := inputMap(req)
	if strVal(in, "confirm", "") != resetConfirm {
		return action.Fail(req, errcode.InvalidInput, "generic.error", `confirm must be "reset"`, nil)
	}
	backup := ""
	if _, given := in["backup"]; !given || boolVal(in, "backup") { // a backup unless asked not to
		name, _, err := s.createBackupLocked(backupPreReset)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", "backup failed, nothing was changed: "+err.Error(), nil)
		}
		backup = name
	}
	j, err := newMaintenance(s.DataRoot, "reset")
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	opt, err := storage.OptionsFromEnv(s.DataRoot, storage.Settings{})
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	j.DBPath, err = filepath.Abs(opt.SQLitePath)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	deleteKeys := boolVal(in, "delete_keys")
	j.DeleteKeys, j.Safety = deleteKeys, backup
	j.Instance = s.Doc().Instance.ID
	j.Targets = resetTargets(j.Root, j.DBPath, deleteKeys)
	if deleteKeys {
		doc := s.Doc()
		prefix := "keychain:" + doc.Instance.ID + "/"
		refs := []string{doc.Discord.TokenRef}
		for _, p := range doc.Providers {
			refs = append(refs, p.CredentialRef)
		}
		for _, ref := range refs {
			if acct, ok := strings.CutPrefix(ref, prefix); ok && acct != "" {
				j.KeyAccounts = append(j.KeyAccounts, doc.Instance.ID+"/"+acct)
			}
		}
	}
	if err := validateMaintenance(&j); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	if err := saveMaintenance(filepath.Join(s.DataRoot, maintenancePending), &j, "requested"); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	s.Log.Info("reset requested; restarting", "backup", backup, "delete_keys", deleteKeys)
	s.requestLifecycle(lifecycleRestart)
	return action.OK(req, map[string]any{"pending": true, "backup": backup, "delete_keys": deleteKeys, "restarting": true, "message": s.msg(req, "reset.pending")})
}

// dataRootEntries classifies every top-level entry the product creates under a data root: erased by a
// reset, or kept (and why). TestEveryDataRootEntryIsClassified fails when code starts using a name that is
// listed in neither, so a new folder cannot silently survive "erase everything".
var dataRootEntries = map[string]string{
	// erased by instance.reset
	"masamong.db": "erase", "config.yaml": "erase", storage.SettingsName: "erase", "imports": "erase",
	"rag_staging": "erase", "rag": "erase", "memory": "erase", "images": "erase", "notices": "erase",
	"practice": "erase", restorePending: "erase", restoreResult: "erase",
	// erased only with delete_keys
	"secrets": "erase-with-keys",
	// kept: backups are how a reset is undone; logs and the console sign-in are not instance data
	"backups": "keep", "logs": "keep", "admin": "keep", "sessions.json": "keep", "daemon.json": "keep",
	"tray.lock": "keep", "daemon.lock": "keep", "maintenance.lock": "keep", maintenancePending: "keep",
}

func resetTargets(root, dbPath string, deleteKeys bool) []string {
	targets := []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"}
	for _, name := range []string{"config.yaml", storage.SettingsName, "imports", "rag_staging", "memory", "images", filepath.Base(PracticeRoot(root)), restorePending, restoreResult} {
		targets = append(targets, filepath.Join(root, name))
	}
	if deleteKeys {
		targets = append(targets, filepath.Join(root, "secrets"))
	}
	// Added after the first release of the journal: an older journal is a prefix of this list (see
	// validateMaintenance), so it resumes and then also erases what it did not know of.
	for _, name := range []string{"rag", "notices"} {
		targets = append(targets, filepath.Join(root, name))
	}
	return targets
}

func applyReset(path string, j *maintenanceJournal) error {
	if err := validateMaintenance(j); err != nil {
		return err
	}
	if j.Phase == "committed" {
		for _, target := range j.Targets {
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				j.Next = 0
				if err := saveMaintenance(path, j, "erase"); err != nil {
					return err
				}
				break
			}
		}
	}
	if j.Phase != "committed" {
		if j.Phase == "requested" {
			if err := saveMaintenance(path, j, "erase"); err != nil {
				return err
			}
		}
		for j.Next < len(j.Targets) {
			target := j.Targets[j.Next]
			if err := ownedPath(j.Root, target); err != nil {
				return err
			}
			// RemoveAll does not follow a directory symlink; linked source files outside
			// these owned folders are never traversed or added from the DB's references.
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := fsutil.SyncDir(filepath.Dir(target)); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := maintenancePoint(fmt.Sprintf("erase:%d", j.Next)); err != nil {
				return err
			}
			j.Next++
			if err := saveMaintenance(path, j, "erase"); err != nil {
				return err
			}
		}
		for _, acct := range j.KeyAccounts {
			if err := secrets.KeychainDelete(acct); err != nil {
				return err
			}
		}
		if err := saveMaintenance(path, j, "committed"); err != nil {
			return err
		}
	}
	return fsutil.RemoveDurable(path)
}

// ApplyPendingReset retains the historical bool API; startup uses RecoverMaintenance
// for an explicit error and never opens the bot on an unfinished reset.
func ApplyPendingReset(dataRoot string, log *slog.Logger) bool {
	unlock, err := lockMaintenance(dataRoot, true)
	if err != nil {
		log.Error("reset incomplete", "err", err)
		return false
	}
	defer unlock()
	path := filepath.Join(dataRoot, maintenancePending)
	j, err := readMaintenance(path, dataRoot)
	if err != nil || j.Kind != "reset" {
		return false
	}
	if err := applyReset(path, j); err != nil {
		log.Error("reset incomplete", "err", err)
	}
	return true
}

// RecoverMaintenance must succeed before opening any live database or starting workers.
func RecoverMaintenance(dataRoot string, log *slog.Logger) error {
	unlock, err := lockMaintenance(dataRoot, true)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dataRoot, maintenancePending)
	j, err := readMaintenance(path, dataRoot)
	if os.IsNotExist(err) {
		for _, legacy := range []string{resetPending, restorePending} {
			if _, err := os.Lstat(filepath.Join(dataRoot, legacy)); !os.IsNotExist(err) {
				return fmt.Errorf("unversioned maintenance marker %s: inspect it, then use masamongd --maintenance cancel", legacy)
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	switch j.Kind {
	case "reset":
		return applyReset(path, j)
	case "restore":
		res := applyPendingRestoreLocked(dataRoot, log)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return nil
		}
		if res.RollbackError != "" {
			return fmt.Errorf("restore incomplete: %s; rollback also failed: %s", res.Error, res.RollbackError)
		}
		return fmt.Errorf("restore incomplete: %s", res.Error)
	default:
		return fmt.Errorf("unexpected data maintenance kind %s", j.Kind)
	}
}

// Situations the maintenance status tells apart. Each one lists only the commands that can work in it
// (the console page and masamongd --maintenance status print exactly these, in this order).
const (
	caseRestoreRequested = "restore_requested" // asked for, nothing moved
	caseRestoreStarted   = "restore_started"   // live files were set aside
	caseResetRequested   = "reset_requested"
	caseResetStarted     = "reset_started"
	caseUpdateResumable  = "update_resumable"
	caseUpdateStuck      = "update_stuck" // the programs are not the ones the update expected
	caseJournalInvalid   = "journal_invalid"
	caseJournalMoved     = "journal_invalid_started" // unreadable, and recovery files show live data was moved
	caseLegacyMarker     = "legacy_marker"
	caseOther            = "other"
)

// MaintenanceCase names the situation and lists the commands that can work in it.
func MaintenanceCase(dataRoot string) (string, []string) {
	if _, pj, perr := pendingProgramMaintenance(); !os.IsNotExist(perr) {
		if perr != nil {
			return caseUpdateStuck, []string{"abandon-update"}
		}
		switch classifyUpdate(pj) {
		case updateStuck:
			return caseUpdateStuck, []string{"abandon-update"}
		case updateNewPair, updateOldPair:
			return caseUpdateResumable, []string{"retry"}
		}
		return caseUpdateResumable, []string{"retry", "rollback"}
	}
	j, err := readMaintenance(filepath.Join(dataRoot, maintenancePending), dataRoot)
	switch {
	case err == nil:
		started := j.Phase != "requested"
		switch j.Kind {
		case "reset":
			if !started {
				return caseResetRequested, []string{"retry", "cancel"}
			}
			return caseResetStarted, []string{"retry"}
		case "restore":
			switch j.Phase {
			case "requested":
				return caseRestoreRequested, []string{"retry", "cancel"}
			case "committed", "rolled_back":
				return caseRestoreStarted, []string{"retry"}
			}
			return caseRestoreStarted, []string{"retry", "rollback"}
		}
	case os.IsNotExist(err):
		for _, name := range []string{resetPending, restorePending} {
			if _, err := os.Lstat(filepath.Join(dataRoot, name)); !os.IsNotExist(err) {
				return caseLegacyMarker, []string{"cancel"}
			}
		}
		return caseOther, []string{"retry"}
	default:
		if len(recoveryLeftovers(dataRoot)) > 0 {
			return caseJournalMoved, nil // cancelling would hide set-aside live data: inspect by hand
		}
		return caseJournalInvalid, []string{"cancel"}
	}
	return caseOther, []string{"retry"}
}

// MaintenanceStatus is readable without opening or migrating the live database.
func MaintenanceStatus(dataRoot string) map[string]any {
	out := map[string]any{"maintenance": maintenanceNeeded(dataRoot), "bot_running": false, "schedulers_running": false}
	kind, commands := MaintenanceCase(dataRoot)
	out["case"] = kind
	if _, program, err := pendingProgramMaintenance(); !os.IsNotExist(err) {
		out["maintenance"] = true
		out["operation"] = "update"
		if err == nil {
			out["phase"] = program.Phase
		}
	}
	if j, err := readMaintenance(filepath.Join(dataRoot, maintenancePending), dataRoot); err == nil {
		out["operation"], out["phase"], out["completed_targets"] = j.Kind, j.Phase, j.Next
	}
	var recovery []string
	var steps []map[string]string
	for _, command := range commands {
		line := fmt.Sprintf("masamongd --data-root \"%s\" --maintenance %s", dataRoot, command)
		recovery = append(recovery, line)
		steps = append(steps, map[string]string{"name": command, "command": line, "effect": "maintenance.effect." + command + "." + effectVariant(kind, command)})
	}
	out["recovery_commands"], out["recovery"] = recovery, steps
	out["recovery_requires_stopped_daemon"] = true
	return out
}

// effectVariant picks the sentence that says what a command does in this situation: cancelling an
// intent that never started forgets it, cancelling an unreadable record only moves it aside.
func effectVariant(kind, command string) string {
	if command == "cancel" && (kind == caseJournalInvalid || kind == caseLegacyMarker) {
		return "aside"
	}
	return "default"
}

// moveJournalAside renames a maintenance record next to the data (maintenance-refused-*.json) instead
// of deleting it, so it can still be inspected.
func moveJournalAside(dataRoot, path string) error {
	f, err := os.CreateTemp(dataRoot, "maintenance-refused-*.json")
	if err != nil {
		return err
	}
	dst := f.Name()
	if err := f.Close(); err != nil {
		return err
	}
	return fsutil.RenameDurable(path, dst)
}

// CancelMaintenance cancels only intent that has not touched live data. A malformed journal is
// quarantined for inspection, rather than interpreted as deletion consent, and only while nothing shows
// that live data was already set aside: with recovery files present, cancelling would leave the data
// moved aside and the next start would open a first-run setup, as if the data were gone.
func CancelMaintenance(dataRoot string) error {
	unlock, err := lockMaintenance(dataRoot, true)
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, err := pendingProgramMaintenance(); !os.IsNotExist(err) {
		return fmt.Errorf("a program update cannot be cancelled: use retry or rollback, or abandon-update if the programs were reinstalled")
	}
	path := filepath.Join(dataRoot, maintenancePending)
	j, err := readMaintenance(path, dataRoot)
	if err == nil && j.Phase != "requested" {
		return fmt.Errorf("maintenance has started and live files were moved; use retry or rollback instead")
	}
	if err != nil && !os.IsNotExist(err) {
		if left := recoveryLeftovers(dataRoot); len(left) > 0 {
			return fmt.Errorf("the maintenance record is unreadable and %d recovery file(s) such as %s show that live data was set aside; cancelling would hide it. Inspect the record and those files first", len(left), left[0])
		}
	}
	for _, name := range []string{maintenancePending, resetPending, restorePending} {
		p := filepath.Join(dataRoot, name)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			continue
		}
		if err := moveJournalAside(dataRoot, p); err != nil {
			return err
		}
	}
	return nil
}

func RollbackMaintenance(dataRoot string) error {
	unlock, err := lockMaintenance(dataRoot, true)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dataRoot, maintenancePending)
	j, err := readMaintenance(path, dataRoot)
	if err != nil {
		return err
	}
	if j.Kind != "restore" || j.Phase == "requested" || j.Phase == "committed" {
		return fmt.Errorf("this operation cannot roll back; retry or cancel")
	}
	return rollbackPair(path, j)
}

// BackupOnUpgrade takes a backup on the first start of a new version, however it was installed (the
// console, install.sh, a package), so going back to the previous version always has a copy to restore:
// the database only ever moves forward. Schema changes add tables and columns only, so a copy taken
// right after opening still holds everything the previous version wrote.
func (s *Service) BackupOnUpgrade() {
	if s.Store == nil || s.practice || !s.Ready() || devBuild(Version) {
		return
	}
	var last string
	_, _ = s.kvGet("runtime:version", &last)
	if last == Version {
		return
	}
	var applied struct {
		To string `json:"to"`
	}
	_, _ = s.kvGet("updates:applied", &applied)
	// an instance older than this record counts as an upgrade too; a version the console installed was
	// backed up right before, so it needs no second copy
	if applied.To != Version && (last != "" || s.Revision() > 1) {
		name, _, err := s.createBackup(backupUpgrade)
		if err != nil {
			s.Log.Warn("backup on upgrade failed", "from", last, "to", Version, "err", err)
			return // tried again on the next start
		}
		s.pruneKind(backupUpgrade, updateBackupsKept)
		s.Log.Info("backup on upgrade", "from", last, "to", Version, "backup", name)
	}
	_ = s.kvSet("runtime:version", Version)
}

package application

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"masamong/internal/fsutil"
	"masamong/internal/launcher"
)

// Recovery of an interrupted program update (.masamong-update.json beside the programs).
//
// The journal can outlive what it describes: the operator reinstalls with the installer, copies programs
// by hand, or the files were replaced some other way. Resuming or rolling back would then overwrite files
// the update never saw, so the journal is compared with what is on disk first:
//
//	resumable  every live file is one the journal expects (old, new, or set aside and recoverable)
//	new_pair   both live programs are exactly the new ones: the update did finish
//	old_pair   both live programs are exactly the old ones: it was rolled back
//	stuck      some live file is neither, or a missing one cannot be recovered
//
// new_pair and old_pair settle themselves. stuck needs the operator: AbandonPendingUpdate (the command
// masamongd --maintenance abandon-update) keeps every file and clears only the journal.

const (
	updateResumable = "resumable"
	updateNewPair   = "new_pair"
	updateOldPair   = "old_pair"
	updateStuck     = "stuck"
)

func classifyUpdate(j *maintenanceJournal) string {
	news, olds := 0, 0
	for _, f := range j.Entries {
		live, err := regularHash(f.Target)
		switch {
		case err == nil && f.Stage != "" && live == f.NewHash:
			news++
		case err == nil && f.OldHash != "" && live == f.OldHash:
			olds++
		case err == nil:
			return updateStuck // a file the update never saw
		case !os.IsNotExist(err):
			return updateStuck
		case f.OldHash != "" && hashMatches(f.Old, f.OldHash), f.Stage != "" && hashMatches(f.Stage, f.NewHash):
			// set aside or staged: the journal can finish or undo it
		default:
			return updateStuck
		}
	}
	switch {
	case len(j.Entries) > 0 && news == len(j.Entries):
		return updateNewPair
	case len(j.Entries) > 0 && olds == len(j.Entries) && (j.Phase == "rollback" || j.Phase == "rolled_back"):
		return updateOldPair
	}
	return updateResumable
}

var errUpdateStuck = errors.New("the program files in this folder are not the ones the interrupted update expected (reinstalled or replaced by hand?); look at them, then use masamongd --maintenance abandon-update to keep every file and clear only the update record")

// settleUpdate finishes a journal whose live files already are a complete pair.
func settleUpdate(path string, j *maintenanceJournal, state string) error {
	phase := "committed"
	if state == updateOldPair {
		phase = "rolled_back"
	}
	if err := saveMaintenance(path, j, phase); err != nil {
		return err
	}
	return finishPair(path, j)
}

// UpdateJournalState describes the pending program-update journal for the maintenance page and the
// operator commands ("" = none, "invalid" = unreadable).
func UpdateJournalState() string {
	_, j, err := pendingProgramMaintenance()
	switch {
	case os.IsNotExist(err):
		return ""
	case err != nil:
		return "invalid"
	}
	return classifyUpdate(j)
}

// AbandonPendingUpdate is the operator's exit from a program-update journal nothing else can settle. It
// deletes nothing: the journal is renamed beside the programs (kept as evidence) and every program,
// set-aside and staged file stays where it is. At least one program must be installed, because this
// command is itself a program; the usual cleanup removes the stale copies at the next start once both
// programs are present. The caller confirms with the operator first.
func AbandonPendingUpdate() (string, error) {
	dir, err := InstallDir()
	if err != nil {
		return "", err
	}
	lock, err := launcher.AcquireNamedLock(dir, ".masamong-update.lock")
	if err != nil {
		return "", err
	}
	defer lock.Release()
	path := filepath.Join(dir, updatePending)
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("there is no program update record to abandon")
	}
	installed := 0
	for _, b := range []string{"masamong", "masamongd"} {
		if st, err := os.Stat(filepath.Join(dir, exeName(b))); err == nil && st.Mode().IsRegular() {
			installed++
		}
	}
	if installed == 0 {
		return "", fmt.Errorf("no Masamong program is installed in %s; reinstall with the installer instead", dir)
	}
	dst := filepath.Join(dir, fmt.Sprintf(".masamong-update.abandoned-%d.json", time.Now().Unix()))
	if err := fsutil.RenameDurable(path, dst); err != nil {
		return "", err
	}
	return dst, nil
}

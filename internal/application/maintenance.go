package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"masamong/internal/fsutil"
	"masamong/internal/secrets"
)

const maintenancePending = "maintenance-operation.json"
const updatePending = ".masamong-update.json"

func maintenanceNeeded(root string) bool {
	for _, name := range []string{maintenancePending, restorePending, resetPending} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			return true
		}
	}
	if dir, err := InstallDir(); err == nil {
		if _, err := os.Lstat(filepath.Join(dir, updatePending)); !os.IsNotExist(err) {
			return true
		}
	}
	return false
}

// One journal format is used for data replacement, erasure and program replacement.
// Intent is durable before any live move. Filesystem state reconciles a crash between a move
// and its checkpoint; committed and rolled_back are cleanup-only phases.
type maintenanceJournal struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Phase       string            `json:"phase"`
	Root        string            `json:"root"`
	Instance    string            `json:"instance,omitempty"`
	Name        string            `json:"name,omitempty"`
	Safety      string            `json:"safety,omitempty"`
	Method      string            `json:"method,omitempty"`
	DBPath      string            `json:"db_path,omitempty"`
	Entries     []maintenanceFile `json:"files,omitempty"`
	Targets     []string          `json:"targets,omitempty"`
	Next        int               `json:"next,omitempty"`
	DeleteKeys  bool              `json:"delete_keys,omitempty"`
	KeyAccounts []string          `json:"key_accounts,omitempty"`
}

type maintenanceFile struct {
	Target  string `json:"target"`
	Stage   string `json:"stage,omitempty"`
	Old     string `json:"old"`
	OldHash string `json:"old_sha256,omitempty"`
	NewHash string `json:"new_sha256,omitempty"`
}

var maintenanceMu sync.Mutex
var errMaintenanceInterrupted = errors.New("maintenance interrupted")

// A test hook models process death after every durable phase and individual file operation.
var maintenanceHook func(string) error

func maintenancePoint(point string) error {
	if maintenanceHook != nil {
		return maintenanceHook(point)
	}
	return nil
}

func lockMaintenance(root string, recovery bool) (func(), error) {
	return lockMaintenanceContext(context.Background(), root, recovery)
}

func newMaintenance(root, kind string) (maintenanceJournal, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return maintenanceJournal{}, err
	}
	f, err := os.CreateTemp(root, ".maintenance-id-*")
	if err != nil {
		return maintenanceJournal{}, err
	}
	id := strings.TrimPrefix(filepath.Base(f.Name()), ".maintenance-id-")
	err = errors.Join(f.Close(), os.Remove(f.Name()))
	return maintenanceJournal{Version: 1, ID: id, Kind: kind, Root: abs, Phase: "requested"}, err
}

func saveMaintenance(path string, j *maintenanceJournal, phase string) error {
	j.Phase = phase
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteDurable(path, b, 0o600); err != nil {
		return err
	}
	return maintenancePoint(phase)
}

func readMaintenance(path, root string) (*maintenanceJournal, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("maintenance journal must be a regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j maintenanceJournal
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&j); err != nil {
		return nil, fmt.Errorf("invalid maintenance journal: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid trailing maintenance journal data")
	}
	if j.Version != 1 || j.ID == "" || strings.ContainsAny(j.ID, `/\\.`) || !sameRoot(j.Root, root) {
		return nil, fmt.Errorf("invalid maintenance journal version, identity or root")
	}
	if err := validateMaintenance(&j); err != nil {
		return nil, err
	}
	return &j, nil
}

// sameRoot reports whether the journal's root and the folder the operator named are one folder. A
// byte comparison made a valid, already started journal "invalid" when the folder was spelled in another
// letter case (Windows and default macOS volumes ignore case) or through a link, and then offered to
// cancel it. Folders that exist are compared by identity; otherwise by their resolved, case-folded names.
func sameRoot(journalRoot, root string) bool {
	abs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if journalRoot == abs {
		return true
	}
	a, errA := os.Stat(journalRoot)
	b, errB := os.Stat(abs)
	if errA == nil && errB == nil {
		return os.SameFile(a, b)
	}
	resolve := func(p string) string {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		return filepath.Clean(p)
	}
	x, y := resolve(journalRoot), resolve(abs)
	return x == y || runtime.GOOS == "windows" && strings.EqualFold(x, y)
}

// Refuse links in every parent before destructive operations, including Windows junctions
// reported by EvalSymlinks. Only files created/managed inside the instance may be erased.
func ownedPath(root, path string) error {
	root = filepath.Clean(root)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("maintenance requires a canonical absolute path")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("maintenance path is outside its owned root: %s", path)
	}
	// Links above the root are the operator's choice (macOS /var -> /private/var, a moved home
	// folder); only directories inside the root must resolve to themselves under the real root.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for p := filepath.Dir(path); p != root; p = filepath.Dir(p) {
		if filepath.Dir(p) == p {
			return fmt.Errorf("maintenance path is outside its owned root: %s", path)
		}
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("maintenance parent is a link: %s", p)
		}
		inner, _ := filepath.Rel(root, p)
		if real, err := filepath.EvalSymlinks(p); err != nil || !strings.EqualFold(real, filepath.Join(realRoot, inner)) {
			return fmt.Errorf("maintenance parent resolves elsewhere: %s", p)
		}
	}
	return nil
}

func validateMaintenance(j *maintenanceJournal) error {
	phaseOK := map[string]bool{"requested": true, "prepared": true, "quarantine": true, "install": true, "verify": true, "committed": true, "rollback": true, "rolled_back": true, "erase": true}
	if !phaseOK[j.Phase] {
		return fmt.Errorf("invalid maintenance phase %q", j.Phase)
	}
	switch j.Kind {
	case "reset":
		if j.Phase != "requested" && j.Phase != "erase" && j.Phase != "committed" {
			return fmt.Errorf("invalid reset phase")
		}
		if len(j.Targets) == 0 || j.Next < 0 || j.Next > len(j.Targets) {
			return fmt.Errorf("invalid reset deletion plan")
		}
		expected := resetTargets(j.Root, j.DBPath, j.DeleteKeys)
		if len(j.Targets) > len(expected) {
			return fmt.Errorf("invalid reset targets")
		}
		for i, p := range j.Targets {
			if p != expected[i] {
				return fmt.Errorf("invalid reset target")
			}
		}
		// a journal from before folders were added to the plan is a prefix of it: finish the whole plan
		for _, p := range expected {
			if err := ownedPath(j.Root, p); err != nil {
				return err
			}
		}
		j.Targets = expected
		if len(j.KeyAccounts) > 0 && !j.DeleteKeys {
			return fmt.Errorf("invalid key deletion plan")
		}
		for _, acct := range j.KeyAccounts {
			if j.Instance == "" || !strings.HasPrefix(acct, j.Instance+"/") || acct == j.Instance+"/" || !secrets.ValidAccount(acct) {
				return fmt.Errorf("invalid key account")
			}
		}
	case "restore", "update":
		if j.Phase == "erase" {
			return fmt.Errorf("invalid replacement phase")
		}
		if j.Kind == "restore" {
			if _, err := backupPathIn(filepath.Join(j.Root, "backups"), j.Name); err != nil {
				return err
			}
			if err := ownedPath(j.Root, j.DBPath); err != nil {
				return err
			}
		}
		if j.Phase != "requested" && len(j.Entries) == 0 {
			return fmt.Errorf("missing maintenance file plan")
		}
		if len(j.Entries) > 0 {
			var expected []string
			if j.Kind == "update" {
				expected = []string{filepath.Join(j.Root, exeName("masamong")), filepath.Join(j.Root, exeName("masamongd"))}
			} else {
				expected = []string{j.DBPath, j.DBPath + "-wal", j.DBPath + "-shm", j.DBPath + "-journal", filepath.Join(j.Root, "config.yaml")}
			}
			if len(expected) != len(j.Entries) {
				return fmt.Errorf("invalid replacement plan")
			}
			for i, f := range j.Entries {
				if f.Target != expected[i] || f.Old != recoveryName(f.Target, "old", j.ID) {
					return fmt.Errorf("invalid replacement path")
				}
				if err := ownedPath(j.Root, f.Target); err != nil {
					return err
				}
				if f.Stage != "" && (f.Stage != recoveryName(f.Target, "new", j.ID) || len(f.NewHash) != 64) {
					return fmt.Errorf("invalid staged file")
				}
				if f.OldHash != "" && len(f.OldHash) != 64 {
					return fmt.Errorf("invalid old checksum")
				}
			}
		}
	default:
		return fmt.Errorf("invalid maintenance operation %q", j.Kind)
	}
	return nil
}

func recoveryName(target, kind, id string) string {
	return filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+"."+kind+"-"+id)
}

func regularHash(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("maintenance file is not regular: %s", path)
	}
	return fileSHA256(path)
}

func planFile(target, stage, id string) (maintenanceFile, error) {
	f := maintenanceFile{Target: target, Stage: stage, Old: recoveryName(target, "old", id)}
	if _, err := os.Lstat(f.Old); !os.IsNotExist(err) {
		return f, fmt.Errorf("recovery path is already reserved: %s", f.Old)
	}
	h, err := regularHash(target)
	if err != nil && !os.IsNotExist(err) {
		return f, err
	}
	f.OldHash = h
	if stage != "" {
		f.NewHash, err = regularHash(stage)
	} else {
		err = nil
	}
	return f, err
}

func hashMatches(path, want string) bool {
	h, err := regularHash(path)
	return err == nil && h == want
}

// quarantineEntry sets one live file aside under its recovery name.
func quarantineEntry(i int, f maintenanceFile) error {
	if f.OldHash == "" {
		if _, err := os.Lstat(f.Target); !os.IsNotExist(err) {
			return fmt.Errorf("unexpected live file: %s", f.Target)
		}
		return nil
	}
	if hashMatches(f.Old, f.OldHash) {
		return nil
	}
	if _, err := os.Lstat(f.Old); !os.IsNotExist(err) {
		return fmt.Errorf("unexpected recovery file: %s", f.Old)
	}
	if !hashMatches(f.Target, f.OldHash) {
		return fmt.Errorf("live file changed during maintenance: %s", f.Target)
	}
	if err := fsutil.RenameDurable(f.Target, f.Old); err != nil {
		return err
	}
	return maintenancePoint(fmt.Sprintf("quarantine:%d", i))
}

// installEntry moves one staged file to its live name.
func installEntry(i int, f maintenanceFile) error {
	if f.Stage == "" || hashMatches(f.Target, f.NewHash) {
		return nil
	}
	if _, err := os.Lstat(f.Target); !os.IsNotExist(err) {
		return fmt.Errorf("unexpected replacement destination: %s", f.Target)
	}
	if !hashMatches(f.Stage, f.NewHash) {
		return fmt.Errorf("staged file unavailable: %s", f.Stage)
	}
	if err := fsutil.RenameDurable(f.Stage, f.Target); err != nil {
		return err
	}
	return maintenancePoint(fmt.Sprintf("install:%d", i))
}

func installPair(path string, j *maintenanceJournal, verify func() error) error {
	if err := validateMaintenance(j); err != nil {
		return err
	}
	if j.Phase == "rollback" {
		return rollbackPair(path, j)
	}
	if j.Phase == "rolled_back" || j.Phase == "committed" {
		return finishPair(path, j)
	}
	if j.Phase == "prepared" {
		for _, f := range j.Entries {
			if f.Stage != "" && !hashMatches(f.Stage, f.NewHash) {
				return fmt.Errorf("staged file checksum mismatch")
			}
		}
		if err := saveMaintenance(path, j, "quarantine"); err != nil {
			return err
		}
	}
	if j.Phase == "quarantine" && j.Kind == "update" {
		// Programs are replaced one at a time (the CLI first, the daemon that runs this recovery last),
		// so a program is runnable at every point of a crash: never both set aside before the first
		// replacement is in place.
		for i, f := range j.Entries {
			if err := quarantineEntry(i, f); err != nil {
				return err
			}
			if err := installEntry(i, f); err != nil {
				return err
			}
		}
		if err := saveMaintenance(path, j, "verify"); err != nil {
			return err
		}
	}
	if j.Phase == "quarantine" {
		for i, f := range j.Entries {
			if err := quarantineEntry(i, f); err != nil {
				return err
			}
		}
		if err := saveMaintenance(path, j, "install"); err != nil {
			return err
		}
	}
	if j.Phase == "install" {
		for i, f := range j.Entries {
			if err := installEntry(i, f); err != nil {
				return err
			}
		}
		if err := saveMaintenance(path, j, "verify"); err != nil {
			return err
		}
	}
	if j.Phase == "verify" {
		for _, f := range j.Entries {
			if f.Stage != "" && !hashMatches(f.Target, f.NewHash) {
				return fmt.Errorf("installed checksum mismatch")
			}
		}
		if err := verify(); err != nil {
			return err
		}
		if err := saveMaintenance(path, j, "committed"); err != nil {
			return err
		}
	}
	return finishPair(path, j)
}

func rollbackPair(path string, j *maintenanceJournal) error {
	if j.Phase != "rollback" {
		if err := saveMaintenance(path, j, "rollback"); err != nil {
			return err
		}
	}
	for i, f := range j.Entries {
		if f.OldHash != "" && hashMatches(f.Target, f.OldHash) {
			continue
		}
		if j.Kind == "update" && f.OldHash != "" {
			if err := restoreProgram(i, f); err != nil {
				return err
			}
			continue
		}
		if f.OldHash != "" && !hashMatches(f.Old, f.OldHash) {
			return fmt.Errorf("old recovery file unavailable: %s", f.Old)
		}
		if _, err := os.Lstat(f.Target); err == nil {
			if f.NewHash == "" || !hashMatches(f.Target, f.NewHash) {
				return fmt.Errorf("refusing to remove unexpected live file: %s", f.Target)
			}
			if err := fsutil.RemoveDurable(f.Target); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if f.OldHash != "" {
			if err := fsutil.RenameDurable(f.Old, f.Target); err != nil {
				return err
			}
		}
		if err := maintenancePoint(fmt.Sprintf("rollback:%d", i)); err != nil {
			return err
		}
	}
	if err := saveMaintenance(path, j, "rolled_back"); err != nil {
		return err
	}
	return finishPair(path, j)
}

// Copy the old image beside the installed one, then replace the name atomically. A sharing violation
// leaves both the installed image and the old recovery copy intact; the journal retries it later.
func restoreProgram(i int, f maintenanceFile) error {
	if !hashMatches(f.Old, f.OldHash) {
		return fmt.Errorf("old recovery file unavailable: %s", f.Old)
	}
	if _, err := os.Lstat(f.Target); err == nil && !hashMatches(f.Target, f.NewHash) {
		return fmt.Errorf("unexpected installed program: %s", f.Target)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	stage := f.Stage
	if stage == "" {
		return fmt.Errorf("program rollback has no staging path")
	}
	if !hashMatches(stage, f.OldHash) {
		if err := fsutil.RemoveDurable(stage); err != nil {
			return err
		}
		if err := maintenancePoint(fmt.Sprintf("rollback:stage:%d", i)); err != nil {
			return err
		}
		in, err := os.Open(f.Old)
		if err != nil {
			return err
		}
		defer in.Close()
		st, err := in.Stat()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, st.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		err = errors.Join(copyErr, out.Sync(), out.Close())
		if err != nil {
			return err
		}
		if err := fsutil.SyncDir(filepath.Dir(stage)); err != nil {
			return err
		}
		if !hashMatches(stage, f.OldHash) {
			return fmt.Errorf("rollback staging checksum mismatch")
		}
		if err := maintenancePoint(fmt.Sprintf("rollback:copy:%d", i)); err != nil {
			return err
		}
	}
	if err := fsutil.RenameDurable(stage, f.Target); err != nil {
		return err
	}
	if err := maintenancePoint(fmt.Sprintf("rollback:replace:%d", i)); err != nil {
		return err
	}
	return maintenancePoint(fmt.Sprintf("rollback:%d", i))
}

func finishPair(path string, j *maintenanceJournal) error {
	if j.Phase != "committed" && j.Phase != "rolled_back" {
		return fmt.Errorf("maintenance is incomplete")
	}
	// Even cleanup after a crash must establish pair completeness before deleting
	// the remaining old files. A missing/corrupt target retains all recovery material.
	for _, f := range j.Entries {
		want := f.NewHash
		if j.Phase == "rolled_back" {
			want = f.OldHash
		}
		if want != "" && !hashMatches(f.Target, want) {
			return fmt.Errorf("completed maintenance pair is incomplete: %s", f.Target)
		}
		if want == "" {
			if _, err := os.Lstat(f.Target); !os.IsNotExist(err) {
				return fmt.Errorf("unexpected file in completed pair: %s", f.Target)
			}
		}
	}
	// The pair is complete and verified. An old or staged copy that cannot be deleted now (a running
	// Windows supervisor holds the old image; an antivirus scanner or indexer holds the old database)
	// must not undo that: the operation is done, the unique names cannot collide with a later one, and
	// the leftovers are retried later (CleanUpdateLeftovers, CleanDataLeftovers) and shown in doctor.
	for _, f := range j.Entries {
		if f.Stage != "" {
			_ = fsutil.RemoveDurable(f.Stage)
		}
		_ = fsutil.RemoveDurable(f.Old)
	}
	return fsutil.RemoveDurable(path)
}

package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"

	"masamong/internal/fsutil"
)

// ExitRestart is the exit code a supervised daemon uses to ask its supervisor for a fresh copy (an
// operator restart). The supervisor starts it again at once, on the same console address.
const ExitRestart = 75

// ExitLocked is the exit code of a daemon that found another one owning the data folder.
const ExitLocked = 3

// CrashNote is what the supervisor leaves in the data folder when the daemon ended abnormally. The next
// daemon reads it once, records it in the uptime history and tells the operators.
type CrashNote struct {
	At       string `json:"at"`               // when the daemon ended (RFC 3339, UTC)
	Reason   string `json:"reason"`           // exit status or signal, e.g. "exit status 2"
	Detail   string `json:"detail,omitempty"` // the first panic or fatal error line, if any
	Restarts int    `json:"restarts"`         // abnormal ends in the supervisor's recent window
	GaveUp   bool   `json:"gave_up"`          // too many in a row: the supervisor stopped restarting
}

func crashPath(dataRoot string) string { return filepath.Join(dataRoot, "logs", "crash.json") }

// WriteCrashNote records an abnormal end (0600).
func WriteCrashNote(dataRoot string, n CrashNote) error {
	if err := fsutil.PrivateDir(dataRoot); err != nil {
		return err
	}
	if err := fsutil.PrivateDir(filepath.Join(dataRoot, "logs")); err != nil {
		return err
	}
	raw, _ := json.Marshal(n)
	return fsutil.WriteAtomic(crashPath(dataRoot), raw, 0o600)
}

// TakeCrashNote returns the recorded abnormal end, if any, and removes it.
func TakeCrashNote(dataRoot string) (CrashNote, bool) {
	raw, err := os.ReadFile(crashPath(dataRoot))
	if err != nil {
		return CrashNote{}, false
	}
	_ = os.Remove(crashPath(dataRoot))
	var n CrashNote
	return n, json.Unmarshal(raw, &n) == nil && n.Reason != ""
}

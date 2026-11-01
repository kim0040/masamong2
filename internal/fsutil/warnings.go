package fsutil

import (
	"log/slog"
	"sort"
	"sync"
)

var (
	warnMu   sync.Mutex
	warnings = map[string]string{}
)

// Warn records a problem with the data folder's permissions that the daemon can live with. It is
// logged the first time a key is seen and kept for doctor, so a folder that cannot be made private shows
// up as an attention item instead of ending the daemon.
func Warn(key, message string, attrs ...any) {
	warnMu.Lock()
	_, seen := warnings[key]
	warnings[key] = message
	warnMu.Unlock()
	if !seen {
		slog.Warn(message, attrs...)
	}
}

// ResetWarnings forgets what Warn recorded; tests use it to start clean.
func ResetWarnings() {
	warnMu.Lock()
	warnings = map[string]string{}
	warnMu.Unlock()
}

// Warnings lists the permission problems found so far, sorted by key. It is empty on a healthy
// data folder and on platforms where permissions are plain mode bits.
func Warnings() []string {
	warnMu.Lock()
	defer warnMu.Unlock()
	keys := make([]string, 0, len(warnings))
	for k := range warnings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, warnings[k])
	}
	return out
}

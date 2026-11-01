package application

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"masamong/internal/storage"
)

// Files a maintenance operation or a backup leaves behind when it cannot clean up after itself:
//
//   - .<name>.old-<id>, .<name>.new-<id>: a restore's set-aside live files and staged copies. After a
//     committed restore a scanner or indexer may hold the old database open, so deleting it fails; the
//     restore is still a success and the files are removed later.
//   - .backup-staging-*: a backup's working folder (a full copy of the database). A crash or reboot
//     during a backup leaves it, invisible to backup.list, and it was never removed.
//
// With no journal present and the maintenance lock held, no operation owns any of them.

var recoveryFileName = regexp.MustCompile(`^\..+\.(old|new)-[0-9A-Za-z]+$`)

// dataFolders are the folders a recovery file can sit in: the data root and the folder of the database
// (a custom sqlite_file may live in a subfolder).
func dataFolders(root string) []string {
	folders := []string{root}
	if opt, err := storage.OptionsFromEnv(root, storage.Settings{}); err == nil && opt.SQLitePath != "" {
		if dir := filepath.Dir(opt.SQLitePath); filepath.Clean(dir) != filepath.Clean(root) {
			folders = append(folders, dir)
		}
	}
	return folders
}

// recoveryLeftovers lists recovery files in the data folders.
func recoveryLeftovers(root string) []string {
	var out []string
	for _, dir := range dataFolders(root) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if recoveryFileName.MatchString(e.Name()) {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

// stagingLeftovers lists backup working folders no backup owns.
func stagingLeftovers(root string) []string {
	entries, _ := os.ReadDir(filepath.Join(root, "backups"))
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-staging-") {
			out = append(out, filepath.Join(root, "backups", e.Name()))
		}
	}
	return out
}

// CleanDataLeftovers removes what no operation owns and returns what it could not remove. It does
// nothing while a maintenance journal exists or another operation holds the maintenance lock.
func CleanDataLeftovers(root string) (remaining []string) {
	unlock, err := lockMaintenance(root, false)
	if err != nil {
		return nil
	}
	defer unlock()
	if maintenanceNeeded(root) {
		return nil
	}
	for _, p := range append(recoveryLeftovers(root), stagingLeftovers(root)...) {
		if err := os.RemoveAll(p); err != nil {
			remaining = append(remaining, p)
		}
	}
	return remaining
}

func (s *Service) leftoverList() []string {
	if p := s.leftovers.Load(); p != nil {
		return *p
	}
	return nil
}

// cleanDataLeftovers is CleanDataLeftovers for the running service: what stays is shown in doctor.
func (s *Service) cleanDataLeftovers() {
	remaining := CleanDataLeftovers(s.DataRoot)
	s.leftovers.Store(&remaining)
	if len(remaining) > 0 {
		s.Log.Warn("maintenance: files left by a restore or a backup could not be removed yet; they are retried every hour",
			"count", len(remaining), "first", filepath.Base(remaining[0]))
	}
}

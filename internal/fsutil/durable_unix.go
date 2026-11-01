//go:build !windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
)

func SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func RenameDurable(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	err := SyncDir(filepath.Dir(dst))
	if filepath.Dir(src) != filepath.Dir(dst) {
		err = errors.Join(err, SyncDir(filepath.Dir(src)))
	}
	return err
}

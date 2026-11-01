package fsutil

import (
	"errors"
	"os"
	"path/filepath"
)

// WriteDurable publishes a flushed file through an exclusively owned temporary file.
func WriteDurable(path string, body []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".maintenance-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(perm); err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return RenameDurable(f.Name(), path)
}

func RemoveDurable(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

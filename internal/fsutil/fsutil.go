// Package fsutil writes small state files (config, storage settings) so that a crash or power cut
// leaves either the old or the new file, never an empty one.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
)

// WriteAtomic writes body to path through a temporary file that is flushed to disk before it replaces
// path. Without the flush some filesystems (and SD cards) can keep the rename but lose the data,
// leaving a zero-length file the daemon then cannot start from.
//
// The temporary file gets a random name created exclusively (a fixed name could be a link someone
// planted) and is removed on every failure, so a failed write leaves nothing next to path.
func WriteAtomic(path string, body []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // after a successful rename there is nothing left to remove
	if err = f.Chmod(perm); err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	// A replacement file keeps the temporary file's security descriptor after rename. On
	// Windows, mode bits do not protect it, so apply the private ACL before installation.
	if err := PrivateFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// the directory entry too (best effort: not supported on Windows)
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

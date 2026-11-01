//go:build !windows

package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// PrivateDir makes sure dir is readable by its owner only (0700). A folder that was open to others is
// tightened together with what is already inside it (the group and other bits are dropped): while the root was open,
// a child readable by others could be reached. A root that is already private needs no walk, since
// nobody else can get past it. Links are not followed, items that vanish meanwhile are ignored, and a
// child that cannot be tightened is a warning (Warnings, doctor), not an error.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm()&0o077 == 0 {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	failed, first := 0, ""
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == dir || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode().Perm()&0o077 == 0 {
			return nil
		}
		if !d.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		// drop the group and other bits; the owner's stay (a script keeps its execute bit)
		if err := os.Chmod(path, info.Mode().Perm()&^0o077); err != nil && !os.IsNotExist(err) {
			if failed++; first == "" {
				first = path
			}
		}
		return nil
	})
	if failed > 0 {
		Warn("children:"+dir, fmt.Sprintf("%d item(s) in the data folder %s could not be made private (first: %s)", failed, dir, first))
	}
	return nil
}

// PrivateFile limits an existing file to its owner (0600); a missing one is not an error.
func PrivateFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	return os.Chmod(path, 0o600)
}

// VerifyPrivateDir and VerifyPrivateFile check the platform's private-access policy.
func VerifyPrivateDir(path string) error  { return verifyPrivateMode(path, true, 0o700) }
func VerifyPrivateFile(path string) error { return verifyPrivateMode(path, false, 0o600) }

func verifyPrivateMode(path string, dir bool, want os.FileMode) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if dir && !fi.IsDir() || !dir && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s has unexpected file type", filepath.Clean(path))
	}
	if fi.Mode().Perm()&0o077 != 0 || fi.Mode().Perm()&want != want {
		return fmt.Errorf("%s mode %04o is not private", path, fi.Mode().Perm())
	}
	return nil
}

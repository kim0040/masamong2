//go:build windows

package fsutil

import "golang.org/x/sys/windows"

// Windows does not support directory fsync. MoveFileEx WRITE_THROUGH flushes the move.
func SyncDir(string) error { return nil }

func RenameDurable(src, dst string) error {
	s, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	d, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(s, d, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

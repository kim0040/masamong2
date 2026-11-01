//go:build !windows

package fsutil

import "golang.org/x/sys/unix"

// FreeBytes is the space available to this process on the filesystem holding path.
func FreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

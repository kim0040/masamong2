// Package logfile is a size-capped, rotating log file for the long-running daemon: daemon.log is
// renamed to daemon.log.1 (and older ones shift up) when it reaches MaxBytes, and only Keep old files
// stay. Without it a service that runs for months fills the disk with one ever-growing log.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Rotating struct {
	Path     string
	MaxBytes int64 // rotate when the file would grow past this (default 10 MiB)
	Keep     int   // rotated files kept: Path.1 … Path.Keep (default 5)

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open creates (or appends to) path and rotates it right away if it is already too large.
func Open(path string, maxBytes int64, keep int) (*Rotating, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	if keep <= 0 {
		keep = 5
	}
	r := &Rotating{Path: path, MaxBytes: maxBytes, Keep: keep}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	if r.size >= r.MaxBytes {
		if err := r.rotate(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Rotating) open() error {
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

// rotate shifts Path.(n) to Path.(n+1), drops the oldest and starts a new Path. Caller holds mu.
func (r *Rotating) rotate() error {
	if r.f != nil {
		_ = r.f.Close() // closed before renaming: Windows cannot rename an open file
		r.f = nil
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", r.Path, r.Keep))
	for i := r.Keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.Path, i), fmt.Sprintf("%s.%d", r.Path, i+1))
	}
	if err := os.Rename(r.Path, r.Path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.open()
}

// Write appends p, rotating first when p would take the file past MaxBytes. A failed rotation keeps
// writing to the current file rather than losing log lines.
func (r *Rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.MaxBytes {
		if err := r.rotate(); err != nil && r.f == nil {
			if oerr := r.open(); oerr != nil {
				return 0, err
			}
		}
	}
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *Rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

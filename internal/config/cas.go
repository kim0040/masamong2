package config

import (
	"errors"
	"fmt"

	"masamong/internal/errcode"
)

// CAS is the file+DB recovery protocol from 06장.
// We do not treat file rename and the SQLite row as one SQL transaction.
// Crash recovery compares the prepared content hash to the bytes on disk.

type Status string

const (
	StatusCommitted Status = "committed"
	StatusPrepared  Status = "prepared"
	StatusDiscarded Status = "discarded"
)

type Revision struct {
	Revision     int
	Status       Status
	ContentHash  string
	Content      []byte
	BaseRevision int
}

type State struct {
	Current   *Revision // last committed
	Prepared  *Revision // at most one
	FileHash  string
	FileBytes []byte
}

var (
	ErrStale    = errors.New(errcode.ConfigConflict)
	ErrInvalid  = errors.New(errcode.ConfigInvalid)
	ErrConflict = errors.New(errcode.ConfigConflict)
)

func ValidateCandidate(raw []byte) (Document, error) {
	return Parse(raw)
}

// BeginApply returns a prepared revision or ErrStale when base does not match committed.
func BeginApply(st State, baseRevision int, candidate []byte) (Revision, error) {
	if _, err := ValidateCandidate(candidate); err != nil {
		return Revision{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	curRev := 0
	if st.Current != nil {
		curRev = st.Current.Revision
	}
	if baseRevision != curRev {
		return Revision{}, fmt.Errorf("%w: expected %d got %d", ErrStale, curRev, baseRevision)
	}
	if st.Prepared != nil && st.Prepared.Status == StatusPrepared {
		return Revision{}, fmt.Errorf("%w: prepared revision already in flight", ErrConflict)
	}
	return Revision{
		Revision:     curRev + 1,
		Status:       StatusPrepared,
		ContentHash:  Hash(candidate),
		Content:      append([]byte(nil), candidate...),
		BaseRevision: baseRevision,
	}, nil
}

// Recover decides whether a crash left a prepared revision that already landed on disk.
func Recover(st State) (commit *Revision, discard *Revision) {
	if st.Prepared == nil || st.Prepared.Status != StatusPrepared {
		return nil, nil
	}
	if st.FileHash == st.Prepared.ContentHash {
		c := *st.Prepared
		c.Status = StatusCommitted
		return &c, nil
	}
	d := *st.Prepared
	d.Status = StatusDiscarded
	return nil, &d
}

package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"masamong/internal/fsutil"
)

type PendingEvent struct {
	ID      string
	Payload json.RawMessage
}

// EventWake is a coalesced notification for the application worker. The directory remains the source
// of truth; a single buffered signal is enough to interrupt its idle backoff after an enqueue.
func (s *Store) EventWake() <-chan struct{} {
	s.eventWakeOnce.Do(func() { s.eventWake = make(chan struct{}, 1) })
	return s.eventWake
}

func (s *Store) notifyEventWorker() {
	_ = s.EventWake()
	select {
	case s.eventWake <- struct{}{}:
	default:
	}
}

// HasPendingEvents fails closed on filesystem errors: a pending privacy intent must not be ignored.
func (s *Store) HasPendingEvents(kind string) bool {
	file, err := os.Open(s.eventDir(kind))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	defer file.Close()
	for {
		entries, err := file.ReadDir(1)
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".failed") {
				return true
			}
		}
		if err != nil {
			return err != io.EOF
		}
	}
}

func (s *Store) eventDir(kind string) string {
	return filepath.Join(filepath.Dir(s.path), "pending-events", kind)
}

// EnqueueEvent uses a flushed, private file rather than the locked database. Each event is immutable;
// replay acknowledges it only after the SQLite transaction commits, so a crash causes safe replay.
func (s *Store) EnqueueEvent(kind string, payload any) (string, error) {
	if kind != "capture" && kind != "source-change" {
		return "", fmt.Errorf("unknown event kind %q", kind)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	s.eventFileMu.Lock()
	defer s.eventFileMu.Unlock()
	dir := s.eventDir(kind)
	if err := fsutil.PrivateDir(dir); err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	id := fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), hex.EncodeToString(nonce[:]))
	if err := fsutil.WriteDurable(filepath.Join(dir, id), body, 0o600); err != nil {
		return "", err
	}
	s.notifyEventWorker()
	return id, nil
}

func (s *Store) PendingEvents(kind string) ([]PendingEvent, error) {
	s.eventFileMu.Lock()
	defer s.eventFileMu.Unlock()
	entries, err := os.ReadDir(s.eventDir(kind))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []PendingEvent
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(s.eventDir(kind), entry.Name()))
		if err != nil {
			return nil, err
		}
		events = append(events, PendingEvent{ID: entry.Name(), Payload: body})
		if len(events) == 200 {
			break
		}
	}
	return events, nil
}

func (s *Store) AcknowledgeEvent(kind, id string) error {
	if filepath.Base(id) != id || !strings.HasSuffix(id, ".json") {
		return fmt.Errorf("invalid event id")
	}
	s.eventFileMu.Lock()
	defer s.eventFileMu.Unlock()
	err := fsutil.RemoveDurable(filepath.Join(s.eventDir(kind), id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ApplyEvent serializes foreground commits and replay. A worker may have committed this immutable
// event while its caller waited for the writer; the missing file then means there is nothing to do.
func (s *Store) ApplyEvent(ctx context.Context, kind, id string, apply func() error) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !s.eventMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer s.eventMu.Unlock()
	if _, err := os.Stat(filepath.Join(s.eventDir(kind), id)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := apply(); err != nil {
		return err
	}
	return s.AcknowledgeEvent(kind, id)
}

// SuspendEvent preserves a rejected intent without repeatedly running a deterministic SQL error.
// It remains a privacy barrier and is retried on the next service start, after recovery or repair.
func (s *Store) SuspendEvent(kind, id string) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	s.eventFileMu.Lock()
	defer s.eventFileMu.Unlock()
	err := fsutil.RenameDurable(filepath.Join(s.eventDir(kind), id), filepath.Join(s.eventDir(kind), id+".failed"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) ResumeEvents() error {
	for _, kind := range []string{"capture", "source-change"} {
		entries, err := os.ReadDir(s.eventDir(kind))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json.failed") {
				if err := fsutil.RenameDurable(filepath.Join(s.eventDir(kind), entry.Name()), filepath.Join(s.eventDir(kind), strings.TrimSuffix(entry.Name(), ".failed"))); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

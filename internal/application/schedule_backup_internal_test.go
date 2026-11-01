package application

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduledBackupRunsOutsideTickAndCancelsCleanly(t *testing.T) {
	s, _ := deliveryService(t)
	doc := s.Doc()
	doc.Backup.Schedule, doc.Backup.Hour = "daily", 0
	s.SetDocForTest(doc)
	var starts atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	oldHook := maintenanceHook
	maintenanceHook = func(point string) error {
		if point == "backup:snapshot" {
			starts.Add(1)
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-s.lifeCtx.Done():
			}
		}
		return nil
	}
	defer func() { maintenanceHook = oldHook }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tickReturned := make(chan struct{})
	go func() {
		s.schedulerTickContext(ctx)
		close(tickReturned)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("scheduled backup did not reach its snapshot")
	}
	select {
	case <-tickReturned:
	case <-time.After(time.Second):
		cancel()
		close(release)
		<-tickReturned
		t.Fatal("scheduler tick waited for a slow backup")
	}
	if !s.backupRunning.Load() {
		t.Fatal("scheduled backup is not owned as an in-flight service task")
	}
	s.schedulerTickContext(ctx)
	if starts.Load() != 1 {
		t.Fatalf("single-flight started %d backups", starts.Load())
	}
	var state string
	if err := s.Store.DB.QueryRow(`SELECT v FROM admin_state WHERE k='schedule:backup'`).Scan(&state); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cancelled backup was recorded as a completed schedule: %s", state)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("service shutdown waited on the scheduled backup")
	}
	if s.backupRunning.Load() {
		t.Fatal("backup flight stayed active after service shutdown")
	}
	entries, err := os.ReadDir(s.backupDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".backup-staging-") {
			t.Fatalf("cancelled backup left staging directory %q", entry.Name())
		}
	}
}

func TestManualBackupCancellationRemovesItsStage(t *testing.T) {
	s, _ := deliveryService(t)
	ctx, cancel := context.WithCancel(context.Background())
	oldHook := maintenanceHook
	maintenanceHook = func(point string) error {
		if point == "backup:snapshot" {
			cancel()
		}
		return nil
	}
	defer func() {
		maintenanceHook = oldHook
		cancel()
	}()
	_, _, err := s.createBackupContext(ctx, backupManual)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backup returned %v", err)
	}
	entries, err := os.ReadDir(s.backupDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled backup left stage files: %v", entries)
	}
}

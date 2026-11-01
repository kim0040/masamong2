package application

import (
	"context"
	"testing"
	"time"

	"masamong/internal/action"
)

// cancelRequestAtSnapshot ends the request's context at every backup snapshot, as a console reload or a
// proxy dropping the connection would while VACUUM INTO is still copying.
func cancelRequestAtSnapshot(t *testing.T, cancel context.CancelFunc) {
	t.Helper()
	old := maintenanceHook
	maintenanceHook = func(point string) error {
		if point == "backup:snapshot" {
			cancel()
		}
		return nil
	}
	t.Cleanup(func() { maintenanceHook = old; cancel() })
}

func TestManualBackupSurvivesTheRequestEnding(t *testing.T) {
	s, _ := deliveryService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancelRequestAtSnapshot(t, cancel)
	res := s.Execute(ctx, action.Request{Action: "backup.create", Locale: "en"})
	if !res.OK {
		t.Fatalf("backup.create failed after its request ended: %+v", res.Error)
	}
	name, _ := res.Output["name"].(string)
	target, err := s.backupPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBackupDir(target); err != nil {
		t.Fatalf("the backup is not whole: %v", err)
	}

	// the scheduled backup run by hand (schedules.run) is detached the same way
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancelRequestAtSnapshot(t, cancel2)
	res = s.Execute(ctx2, action.Request{Action: "schedules.run", Locale: "en", Input: mustJSON(map[string]any{"id": "backup"})})
	if !res.OK {
		t.Fatalf("schedules.run backup failed after its request ended: %+v", res.Error)
	}

	// and so is the safety backup of a restore
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancelRequestAtSnapshot(t, cancel3)
	res = s.Execute(ctx3, action.Request{Action: "backup.restore", Locale: "en", Input: mustJSON(map[string]any{"name": name, "confirm": name, "restart": "false"})})
	if !res.OK {
		t.Fatalf("backup.restore failed after its request ended: %+v", res.Error)
	}
}

func TestManualBackupIsStillCancelledByShutdown(t *testing.T) {
	s, _ := deliveryService(t)
	reached := make(chan struct{})
	old := maintenanceHook
	maintenanceHook = func(point string) error {
		if point == "backup:snapshot" {
			close(reached)
			<-s.lifeCtx.Done()
		}
		return nil
	}
	defer func() { maintenanceHook = old }()
	done := make(chan action.Result, 1)
	go func() { done <- s.Execute(context.Background(), action.Request{Action: "backup.create", Locale: "en"}) }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("backup never reached its snapshot")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case res := <-done:
		if res.OK {
			t.Fatal("a backup running at shutdown reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not cancel the manual backup")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
}

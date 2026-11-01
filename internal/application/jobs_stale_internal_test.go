package application

import (
	"context"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/jobs"
)

// A run that read its job while it was cancelled must not write that row back over a retry made in
// between: both rows have an empty lease, so the save compares the status it read as well.
func TestStaleCancelledRowDoesNotUndoARetry(t *testing.T) {
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"stale"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	j, err := s.Enqueue("test.stale", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string) {
		t.Helper()
		if r := s.Execute(context.Background(), action.Request{Action: name, Input: []byte(`{"id":"` + j.ID + `"}`)}); !r.OK {
			t.Fatalf("%s: %+v", name, r.Error)
		}
	}
	run("jobs.cancel")
	stale, err := getJob(s.Store, j.ID)
	if err != nil || stale.Status != jobs.Cancelled {
		t.Fatalf("after cancel %+v %v", stale, err)
	}
	run("jobs.retry")
	stale.Checkpoint = jobs.EncodeProgress(jobs.Progress{Phase: "sync", Done: 1, Total: 1})
	if err := saveJobExpected(s.Store, stale, stale.LeaseToken, stale.Status); err == nil || err.Error() != errcode.LeaseMismatch {
		t.Fatalf("stale save: %v, want %s", err, errcode.LeaseMismatch)
	}
	if cur, _ := getJob(s.Store, j.ID); cur.Status != jobs.Pending {
		t.Fatalf("retry was undone: %s", cur.Status)
	}
	// a run whose lease is gone does not write its progress onto the job a new run took
	if _, err := s.TakeLease(j.ID, "lease_new", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetJobProgress(j.ID, "lease_old", jobs.Progress{Phase: "sync", Done: 1, Total: 2}); err == nil || err.Error() != errcode.LeaseMismatch {
		t.Fatalf("stale progress: %v, want %s", err, errcode.LeaseMismatch)
	}
	if _, err := s.SetJobProgress(j.ID, "lease_new", jobs.Progress{Phase: "sync", Done: 2, Total: 2}); err != nil {
		t.Fatal(err)
	}
	if cur, _ := getJob(s.Store, j.ID); jobs.ParseProgress(cur.Checkpoint).Done != 2 || cur.LeaseToken != "lease_new" {
		t.Fatalf("own progress not saved: %+v", cur)
	}
}

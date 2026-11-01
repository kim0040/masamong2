package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/jobs"
)

func jobTestSvc(t *testing.T) *Service {
	t.Helper()
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"attempt"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	return s
}

func jobAction(t *testing.T, s *Service, name, id string) {
	t.Helper()
	if r := s.Execute(context.Background(), action.Request{Action: name, Input: []byte(`{"id":"` + id + `"}`)}); !r.OK {
		t.Fatalf("%s: %+v", name, r.Error)
	}
}

// Cancelled → retried → cancelled again leaves two cancelled rows with the same empty lease. A very late
// first run must not write its result over the second attempt's, nor think it still owns the job.
func TestLateRunDoesNotOverwriteALaterCancelledAttempt(t *testing.T) {
	s := jobTestSvc(t)
	j, err := s.Enqueue("test.attempt", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.TakeLease(j.ID, "lease_first", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	jobAction(t, s, "jobs.cancel", j.ID)
	jobAction(t, s, "jobs.retry", j.ID)
	second, err := s.TakeLease(j.ID, "lease_second", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if second.Attempt == first.Attempt {
		t.Fatalf("attempt did not advance: %d", second.Attempt)
	}
	jobAction(t, s, "jobs.cancel", j.ID)
	s.finishJob(j.ID, "lease_second", second.Attempt, map[string]any{"run": "second"}, context.Canceled, "CANCELLED")
	if s.runOwnsJob(j.ID, "lease_first", first.Attempt) {
		t.Fatal("the first run still thinks it owns the job")
	}
	if !s.runOwnsJob(j.ID, "lease_second", second.Attempt) {
		t.Fatal("the second run lost its own cancelled job")
	}
	s.finishJob(j.ID, "lease_first", first.Attempt, map[string]any{"run": "first"}, context.Canceled, "CANCELLED")
	cur, err := getJob(s.Store, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != jobs.Cancelled || !strings.Contains(cur.Result, `"second"`) {
		t.Fatalf("late first run overwrote the second attempt: %s %s", cur.Status, cur.Result)
	}
}

// A run longer than its lease renews it and can still record its end.
func TestRunRenewsItsLeasePastTheTTL(t *testing.T) {
	s := jobTestSvc(t)
	j, err := s.Enqueue("test.lease", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	const ttl = 2 * time.Second
	run, err := s.TakeLease(j.ID, "lease_long", ttl)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelCause := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchJob(ctx, cancelCause, run, "lease_long", ttl, 100*time.Millisecond)
	}()
	time.Sleep(ttl + 1500*time.Millisecond) // past the first expiry (stored to the second)
	if ctx.Err() != nil {
		t.Fatalf("the watcher stopped its own run: %v", context.Cause(ctx))
	}
	cancelCause(nil)
	<-done
	if cur, _ := getJob(s.Store, j.ID); !cur.LeaseExpires.After(run.LeaseExpires) {
		t.Fatalf("lease not renewed: %v, taken until %v", cur.LeaseExpires, run.LeaseExpires)
	}
	s.finishJob(j.ID, "lease_long", run.Attempt, map[string]any{"ok": true}, nil, "")
	if cur, _ := getJob(s.Store, j.ID); cur.Status != jobs.Succeeded {
		t.Fatalf("a run past its first lease could not record its end: %s", cur.Status)
	}
}

// Only the run that holds the job renews it; one that lost it is stopped as taken, a cancel as a cancel.
func TestLeaseRenewalNeedsOwnership(t *testing.T) {
	s := jobTestSvc(t)
	j, err := s.Enqueue("test.lease", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.TakeLease(j.ID, "lease_a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.renewJobLease(j.ID, "lease_other", run.Attempt, time.Hour); err == nil || err.Error() != errcode.LeaseMismatch {
		t.Fatalf("another token renewed: %v", err)
	}
	if err := s.renewJobLease(j.ID, "lease_a", run.Attempt+1, time.Hour); err == nil || err.Error() != errcode.LeaseMismatch {
		t.Fatalf("another attempt renewed: %v", err)
	}
	if err := s.renewJobLease(j.ID, "lease_a", run.Attempt, time.Hour); err != nil {
		t.Fatal(err)
	}
	watch := func(token string, run jobs.Job) (context.Context, chan struct{}) {
		ctx, cancelCause := context.WithCancelCause(context.Background())
		t.Cleanup(func() { cancelCause(nil) })
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.watchJob(ctx, cancelCause, run, token, 200*time.Millisecond, 50*time.Millisecond)
		}()
		return ctx, done
	}
	wait := func(done chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("watcher did not stop")
		}
	}
	// a cancel stops the run as a cancel
	ctx, done := watch("lease_a", run)
	jobAction(t, s, "jobs.cancel", j.ID)
	wait(done)
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		t.Fatalf("cancel cause %v", cause)
	}
	// a retry taken by a new run stops the old one as taken, and the old one can no longer renew
	jobAction(t, s, "jobs.retry", j.ID)
	newer, err := s.TakeLease(j.ID, "lease_b", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, done = watch("lease_a", run)
	wait(done)
	if cause := context.Cause(ctx); !errors.Is(cause, errJobTaken) {
		t.Fatalf("taken cause %v", cause)
	}
	if err := s.renewJobLease(j.ID, "lease_a", run.Attempt, time.Hour); err == nil {
		t.Fatal("the old run renewed the new run's lease")
	}
	if cur, _ := getJob(s.Store, j.ID); cur.LeaseToken != "lease_b" || cur.Attempt != newer.Attempt || cur.Status != jobs.Running {
		t.Fatalf("new run's job changed: %+v", cur)
	}
}

// Cancelled → retried → cancelled again before any new run took it: the retry was already a new
// attempt, so the late first run still cannot write over the second cancel nor think it owns the job.
func TestRetryCancelledBeforeARunTookItIsANewAttempt(t *testing.T) {
	s := jobTestSvc(t)
	j, err := s.Enqueue("test.attempt", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.TakeLease(j.ID, "lease_first", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	jobAction(t, s, "jobs.cancel", j.ID)
	jobAction(t, s, "jobs.retry", j.ID)
	jobAction(t, s, "jobs.cancel", j.ID)
	if s.runOwnsJob(j.ID, "lease_first", first.Attempt) {
		t.Fatal("the first run owns the second attempt's cancelled job")
	}
	s.finishJob(j.ID, "lease_first", first.Attempt, map[string]any{"run": "first"}, context.Canceled, "CANCELLED")
	cur, err := getJob(s.Store, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != jobs.Cancelled || strings.Contains(cur.Result, `"first"`) || cur.Attempt != first.Attempt+1 {
		t.Fatalf("late first run wrote the second attempt: %s attempt %d %s", cur.Status, cur.Attempt, cur.Result)
	}
	// a run that takes the retried job keeps the attempt the retry counted
	jobAction(t, s, "jobs.retry", j.ID)
	run, err := s.TakeLease(j.ID, "lease_third", time.Hour)
	if err != nil || run.Attempt != first.Attempt+2 {
		t.Fatalf("attempt %d (%v), want %d", run.Attempt, err, first.Attempt+2)
	}
}

// A run whose lease ran out with nobody taking the job (the machine slept past it) still records its end.
func TestRunPastAnExpiredLeaseNobodyTookRecordsItsEnd(t *testing.T) {
	s := jobTestSvc(t)
	j, err := s.Enqueue("test.lease", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.TakeLease(j.ID, "lease_sleepy", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond) // the expiry is stored to the second
	s.finishJob(j.ID, "lease_sleepy", run.Attempt, map[string]any{"ok": true}, nil, "")
	if cur, _ := getJob(s.Store, j.ID); cur.Status != jobs.Succeeded {
		t.Fatalf("status %s", cur.Status)
	}
}

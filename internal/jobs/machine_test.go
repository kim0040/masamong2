package jobs

import (
	"testing"
	"time"

	"masamong/internal/errcode"
)

func TestStaleWorkerCannotComplete(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	j := Job{ID: "j1", Type: "demo", Status: Pending}
	j, err := Lease(j, "token-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	j, err = Start(j, "token-a", now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Minute)
	j, err = Lease(j, "token-b", later, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Complete(j, "token-a", later, `{"ok":true}`)
	if err == nil || err.Error() != errcode.LeaseMismatch {
		t.Fatalf("stale complete must fail, got %v", err)
	}
	j, err = Complete(j, "token-b", later, `{"ok":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Succeeded || j.Result != `{"ok":true}` {
		t.Fatalf("got %+v", j)
	}
}

func TestRunningUnexpiredCannotBeStolen(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	j := Job{ID: "j1", Type: "demo", Status: Pending}
	j, err := Lease(j, "token-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	j, err = Start(j, "token-a", now)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Running {
		t.Fatalf("status %s", j.Status)
	}
	_, err = Lease(j, "token-b", now.Add(time.Second), time.Minute)
	if err == nil || err.Error() != errcode.LeaseMismatch {
		t.Fatalf("running unexpired lease must not be stolen, got %v", err)
	}
	taken, err := Lease(j, "token-b", now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if taken.LeaseToken != "token-b" {
		t.Fatalf("%+v", taken)
	}
}

func TestCancelAndRetry(t *testing.T) {
	j := Job{ID: "j2", Status: Pending, Result: `{"stopped_by":"cancelled"}`}
	j, err := Cancel(j)
	if err != nil {
		t.Fatal(err)
	}
	j, err = Retry(j)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != Pending {
		t.Fatalf("status %s", j.Status)
	}
	if j.Result != "" {
		t.Fatalf("retry must clear the previous attempt result, got %q", j.Result)
	}
}

// A renewed lease lets the run complete past the first expiry; only the holder renews.
func TestRenewKeepsTheHoldersLease(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	j, err := Lease(Job{ID: "j1", Status: Pending}, "token-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if j, err = Start(j, "token-a", now); err != nil {
		t.Fatal(err)
	}
	if _, err := Renew(j, "token-b", now, time.Minute); err == nil || err.Error() != errcode.LeaseMismatch {
		t.Fatalf("another token renewed: %v", err)
	}
	if j, err = Renew(j, "token-a", now.Add(50*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := Complete(j, "token-a", now.Add(90*time.Second), `{}`); err != nil {
		t.Fatalf("complete after renewal: %v", err)
	}
	c, _ := Cancel(j)
	if _, err := Renew(c, "token-a", now, time.Minute); err == nil {
		t.Fatal("a cancelled job's lease was renewed")
	}
}

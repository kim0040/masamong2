package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/jobs"
)

func TestStaleEmptyTokenLeaseCannotReviveCancelledJob(t *testing.T) {
	svc, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	input, _ := json.Marshal(map[string]any{"instance": "jobs-cas-test"})
	if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: input, Locale: "en"}); !res.OK {
		t.Fatalf("setup: %+v", res)
	}

	job, err := svc.Enqueue("index", "stale-empty-token", "{}")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := getJob(svc.Store, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := jobs.Cancel(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveJob(svc.Store, cancelled, stale.LeaseToken); err != nil {
		t.Fatal(err)
	}
	late, err := jobs.Lease(stale, "late-worker", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	late, err = jobs.Start(late, "late-worker", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = saveJobExpected(svc.Store, late, stale.LeaseToken, stale.Status)
	if err == nil || err.Error() != errcode.LeaseMismatch {
		t.Fatalf("stale pending snapshot should lose the compare-and-swap: %v", err)
	}
	current, err := getJob(svc.Store, job.ID)
	if err != nil || current.Status != jobs.Cancelled || current.LeaseToken != "" {
		t.Fatalf("cancelled job changed after stale lease: %+v %v", current, err)
	}
}

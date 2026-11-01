package application_test

import (
	"masamong/internal/action"
	"testing"
	"time"
)

func TestJobsListReleasesSingleSQLiteConnection(t *testing.T) {
	svc := setupSvc(t)
	for _, key := range []string{"first", "second"} {
		if _, err := svc.Enqueue("synthetic", key, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan action.Result, 1)
	go func() { done <- exec(svc, "jobs.list", nil) }()
	select {
	case res := <-done:
		if !res.OK {
			t.Fatal("jobs.list failed")
		}
		rows, ok := res.Output["jobs"].([]map[string]any)
		if !ok || len(rows) != 2 {
			t.Fatalf("want 2 jobs, got %d", len(rows))
		}
	case <-time.After(2 * time.Second):
		// Unblock the old nested-query implementation so cleanup itself cannot hang.
		svc.Store.DB.SetMaxOpenConns(2)
		<-done
		t.Fatal("jobs.list blocked with a single SQLite connection")
	}
}

// The console list shows every job that can still change plus the newest 100 finished ones: newer open
// jobs must not push the finished history out, and old finished ones beyond 100 are cut.
func TestJobsListLimitsConsoleHistoryWithoutChangingOutputShape(t *testing.T) {
	svc := setupSvc(t)
	for i := 0; i < 120; i++ {
		if _, err := svc.Enqueue("synthetic", "", "{}"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Store.DB.Exec(`UPDATE jobs SET status='succeeded', created_at='2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		if _, err := svc.Enqueue("synthetic", "", "{}"); err != nil {
			t.Fatal(err)
		}
	}
	res := exec(svc, "jobs.list", nil)
	if !res.OK {
		t.Fatal("jobs.list failed")
	}
	rows, ok := res.Output["jobs"].([]map[string]any)
	if !ok {
		t.Fatalf("jobs.list output = %T, want a jobs array", res.Output["jobs"])
	}
	finished := 0
	for _, r := range rows {
		if r["status"] == "succeeded" {
			finished++
		}
	}
	if len(rows) != 201 || finished != 100 {
		t.Fatalf("jobs.list = %d rows, %d finished; want the 101 open jobs and the newest 100 finished", len(rows), finished)
	}
}

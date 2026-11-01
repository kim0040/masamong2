package application

import (
	"context"
	"fmt"
	"testing"

	"masamong/internal/jobs"
)

func TestRunRetentionPrunesFinishedJobsAndKeepsLatestPreparePerRoom(t *testing.T) {
	s, _ := deliveryService(t)
	insert := func(id, typ, status, payload, created string) {
		t.Helper()
		j := jobs.Job{ID: id, Type: typ, Status: jobs.Status(status), Payload: payload}
		if err := insertJob(s.Store, j); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.DB.Exec(`UPDATE jobs SET created_at=?, updated_at=? WHERE id=?`, created, created, id); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 110; i++ {
		insert(fmt.Sprintf("old-%03d", i), "synthetic", string(jobs.Succeeded), `{}`, "2020-01-01T00:00:00Z")
	}
	insert("prep-a-old", jobPrepare, string(jobs.Succeeded), `{"source_room":"room-a"}`, "2020-01-01T00:00:00Z")
	insert("prep-a-new", jobPrepare, string(jobs.Failed), `{"source_room":"room-a"}`, "2020-01-02T00:00:00Z")
	insert("prep-b", jobPrepare, string(jobs.Succeeded), `{"source_room":"room-b"}`, "2020-01-01T00:00:00Z")
	insert("prep-c-active", jobPrepare, string(jobs.Pending), `{"source_room":"room-c"}`, "2020-01-01T00:00:00Z")

	if _, err := s.runRetention(context.Background()); err != nil {
		t.Fatalf("retention: %v", err)
	}
	var remaining int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&remaining); err != nil || remaining != 3 {
		t.Fatalf("retention left %d jobs, want latest room jobs plus one active job: %v", remaining, err)
	}
	if _, err := getJob(s.Store, "prep-a-old"); err == nil {
		t.Fatal("retention kept an obsolete prepare job for room-a")
	}
	for _, id := range []string{"prep-a-new", "prep-b", "prep-c-active"} {
		if _, err := getJob(s.Store, id); err != nil {
			t.Fatalf("retention removed protected job %q: %v", id, err)
		}
	}
	latest := s.latestJobs()
	if latest["room-a"].ID != "prep-a-new" || latest["room-b"].ID != "prep-b" || latest["room-c"].ID != "prep-c-active" {
		t.Fatalf("latest job per room = %#v", latest)
	}
}

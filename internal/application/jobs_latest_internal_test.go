package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"masamong/internal/jobs"
)

// seedPrepareJobs inserts n prepare jobs over rooms rooms in one transaction. Timestamps repeat on
// purpose (several jobs of a room share a second) so the tie rule matters, and a few payloads are not
// JSON or name no room.
func seedPrepareJobs(t *testing.T, s *Service, n, rooms int) {
	t.Helper()
	tx, err := s.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO jobs(id, type, status, attempt, checkpoint, payload, result, error_code, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		payload := fmt.Sprintf(`{"source_room":"room-%03d"}`, i%rooms)
		switch i % 97 {
		case 0:
			payload = `not json`
		case 1:
			payload = `{}`
		}
		status := string(jobs.Succeeded)
		if i%5 == 0 {
			status = string(jobs.Failed)
		}
		// ids run against insertion order, so only the rowid tie rule can find the newest of a second
		ts := base.Add(time.Duration(i/3) * time.Second).Format(time.RFC3339)
		if _, err := stmt.Exec(fmt.Sprintf("job-%06d", n-i), jobPrepare, status, 0, "", payload, "", "", ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// referenceLatestPrepare is the original read-everything logic: walk the jobs in insertion order and
// let each room's last prepare job win.
func referenceLatestPrepare(t *testing.T, s *Service) map[string]string {
	t.Helper()
	rows, err := s.Store.DB.Query(`SELECT id, payload FROM jobs WHERE type=? ORDER BY created_at, rowid`, jobPrepare)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		var payload sql.NullString
		if err := rows.Scan(&id, &payload); err != nil {
			t.Fatal(err)
		}
		var p preparePayload
		if err := json.Unmarshal([]byte(payload.String), &p); err != nil || p.SourceRoom == "" {
			continue
		}
		out[p.SourceRoom] = id
	}
	return out
}

func TestLatestPrepareJobsMatchOldLogicAndStayFastAtScale(t *testing.T) {
	s, _ := deliveryService(t)
	seedPrepareJobs(t, s, 2500, 120)
	want := referenceLatestPrepare(t, s)

	start := time.Now()
	list, err := latestPrepareJobs(s.Store)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	// 5,000 jobs took 98 s with the correlated "is there a newer one" subquery; the window pass is a
	// few ms. The bound is generous for a slow shared machine and still far below the quadratic cost.
	if took > 2*time.Second {
		t.Fatalf("latestPrepareJobs over 2500 jobs took %v, want a single linear pass", took)
	}
	got := map[string]string{}
	for _, j := range list {
		var p preparePayload
		if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
			t.Fatal(err)
		}
		got[p.SourceRoom] = j.ID
	}
	if len(got) != len(want) || len(want) < 100 {
		t.Fatalf("got %d rooms, old logic %d", len(got), len(want))
	}
	for room, id := range want {
		if got[room] != id {
			t.Fatalf("room %s: latest %s, old logic %s", room, got[room], id)
		}
	}
	if !sort.SliceIsSorted(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt }) {
		t.Fatal("latest jobs are not oldest first")
	}

	// retention shares the rule: it keeps exactly those rows (all are long past the cut) and removes the rest
	start = time.Now()
	removed, err := s.pruneFinishedJobs(context.Background(), time.Now().UTC().Format(time.RFC3339))
	took = time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if took > 2*time.Second {
		t.Fatalf("pruneFinishedJobs over 2500 jobs took %v", took)
	}
	var left int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if int(removed)+left != 2500 || left != len(want) {
		t.Fatalf("removed %d, left %d; want %d kept", removed, left, len(want))
	}
	after := referenceLatestPrepare(t, s)
	for room, id := range want {
		if after[room] != id {
			t.Fatalf("room %s lost its latest job %s (now %s)", room, id, after[room])
		}
	}
}

func TestLatestPrepareJobTieBreaksByInsertionOrder(t *testing.T) {
	s, _ := deliveryService(t)
	// the first job of the second fails at once and is retried: both carry the same created_at, and the
	// first one's random id sorts later than the retry's
	for _, j := range []struct{ id, status string }{{"zzzz-failed-first", string(jobs.Failed)}, {"aaaa-retry-second", string(jobs.Running)}} {
		if _, err := s.Store.DB.Exec(`INSERT INTO jobs(id, type, status, attempt, checkpoint, payload, result, error_code, created_at, updated_at) VALUES(?,?,?,?,'',?,'','',?,?)`,
			j.id, jobPrepare, j.status, 0, `{"source_room":"room-t"}`, "2024-05-05T05:05:05Z", "2024-05-05T05:05:05Z"); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.latestJobs()["room-t"].ID; got != "aaaa-retry-second" {
		t.Fatalf("latest job of a same-second pair = %q, want the one inserted last", got)
	}
	if _, err := s.pruneFinishedJobs(context.Background(), "2030-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := getJob(s.Store, "aaaa-retry-second"); err != nil {
		t.Fatalf("retention removed the newest job: %v", err)
	}
	if _, err := getJob(s.Store, "zzzz-failed-first"); err == nil {
		t.Fatal("retention kept the older failed job (not latest, finished, expired)")
	}
}

func TestPruneFinishedJobsJudgesAgeByUpdatedAtAndListKeepsOpenJobs(t *testing.T) {
	s, _ := deliveryService(t)
	ins := func(id, status, created, updated string) {
		t.Helper()
		if _, err := s.Store.DB.Exec(`INSERT INTO jobs(id, type, status, attempt, checkpoint, payload, result, error_code, created_at, updated_at) VALUES(?,?,?,?,'',?,'','',?,?)`,
			id, "synthetic", status, 0, `{}`, created, updated); err != nil {
			t.Fatal(err)
		}
	}
	ins("made-long-ago-finished-today", string(jobs.Failed), "2020-01-01T00:00:00Z", "2026-10-01T00:00:00Z")
	ins("finished-long-ago", string(jobs.Failed), "2020-01-01T00:00:00Z", "2020-01-02T00:00:00Z")
	if n, err := s.pruneFinishedJobs(context.Background(), "2026-09-01T00:00:00Z"); err != nil || n != 1 {
		t.Fatalf("pruned %d (err %v), want only the job last touched long ago", n, err)
	}
	if _, err := getJob(s.Store, "made-long-ago-finished-today"); err != nil {
		t.Fatalf("a job finished recently was pruned: %v", err)
	}

	// jobs.list shows every open job even when 100+ newer finished jobs push it out of the latest 100
	ins("old-paused", string(jobs.Paused), "2019-01-01T00:00:00Z", "2019-01-01T00:00:00Z")
	for i := 0; i < consoleJobLimit+20; i++ {
		ins(fmt.Sprintf("done-%03d", i), string(jobs.Succeeded), "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z")
	}
	list, err := listJobs(s.Store)
	if err != nil {
		t.Fatal(err)
	}
	var open, finished int
	recent := false
	for _, j := range list {
		recent = recent || j.ID == "made-long-ago-finished-today" // finished most recently: in the window by updated_at
		if j.ID == "old-paused" {
			open++
		} else if j.Status == jobs.Succeeded {
			finished++
		}
	}
	if !recent {
		t.Fatal("a job created long ago but finished most recently is missing from the finished window")
	}
	if open != 1 || finished > consoleJobLimit || len(list) > consoleJobLimit+2 {
		t.Fatalf("list has open=%d finished=%d total=%d", open, finished, len(list))
	}
	if list[0].ID != "old-paused" {
		t.Fatalf("list not oldest first: first is %s", list[0].ID)
	}
}

package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/observability"
	"masamong/internal/storage"
)

// TestCloseOrderIsWorkLedgerStatisticsStore pins the order Close shuts down in (service.go Close):
// the work that is still running finishes, the call ledger drains, the statistics counter flushes,
// and only then is the store closed. A split of the package turns the ledger and the statistics
// into other packages; this test is what keeps them called in this order.
//
// The order is observed through the data, not through hooks. SQLite triggers write one row to a
// probe table whenever the ledger or the statistics write their rows; the probe row of the running
// work is written by the work itself after Close has started. Everything the work hands over late
// (a model call held in the job aggregate, an action count) can only reach the database if each
// stage of Close ran after the one before it and before the store closed.
func TestCloseOrderIsWorkLedgerStatisticsStore(t *testing.T) {
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"closeorder","timezone":"Asia/Seoul"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE close_probe(id INTEGER PRIMARY KEY AUTOINCREMENT, step TEXT NOT NULL)`,
		`CREATE TRIGGER close_probe_ledger AFTER INSERT ON llm_calls WHEN NEW.role = 'late'
			BEGIN INSERT INTO close_probe(step) VALUES('ledger'); END`,
		`CREATE TRIGGER close_probe_stats_insert AFTER INSERT ON stats_daily WHEN NEW.metric = 'actions.calls'
			BEGIN INSERT INTO close_probe(step) VALUES('stats'); END`,
		`CREATE TRIGGER close_probe_stats_update AFTER UPDATE ON stats_daily WHEN NEW.metric = 'actions.calls'
			BEGIN INSERT INTO close_probe(step) VALUES('stats'); END`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	path := s.Store.Path()

	// a call before Close, so the ledger writer goroutine exists and stops with the work
	s.noteLLMCall(context.Background(), llmCall{Role: "early", Model: "m"})

	closing := make(chan struct{})
	release := make(chan struct{})
	workDone := make(chan struct{})
	s.ownedGo("close order probe", func(ctx context.Context) {
		defer close(workDone)
		<-ctx.Done() // Close cancelled the service's work
		close(closing)
		<-release
		if _, err := db.DB.Exec(`INSERT INTO close_probe(step) VALUES('work')`); err != nil {
			t.Errorf("probe write by the running work: %v", err)
		}
		// handed over after the ledger writer goroutine has gone: only Close's own flush and drain can write these
		jobCtx := observability.WithJob(context.Background(), "job-close-order")
		s.noteMemoryLLMCall(jobCtx, llmCall{Role: "late", Model: "m"})
		s.statsCount(action.Request{Action: "chat.ask"}, action.Result{OK: true})
	})

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-closing:
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not cancel the running work")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while work was still running: it must wait for the work", err)
	default:
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return after the work finished")
	}
	<-workDone

	check, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	rows, err := check.DB.Query(`SELECT step FROM close_probe ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			t.Fatal(err)
		}
		if n := len(steps); n == 0 || steps[n-1] != step { // one statistics flush writes several rows
			steps = append(steps, step)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if got := strings.Join(steps, ","); got != "work,ledger,stats" {
		t.Fatalf("Close wrote %q; want work,ledger,stats (the running work, then the call ledger, then the statistics, all before the store closed)", got)
	}
	// the store is closed: another Close reports the same outcome and nothing else happens
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := s.Store.DB.Ping(); err == nil {
		t.Fatal("the store is still open after Close")
	}
}

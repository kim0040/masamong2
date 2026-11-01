//go:build ledgerload

package application_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/memory/corpus"
)

// TestFullScaleMemoryLedgerWithFakeProvider runs the actual indexing path over the evaluation-set
// scale using synthetic observations and a local provider stub. Run with -tags ledgerload.
func TestFullScaleMemoryLedgerWithFakeProvider(t *testing.T) {
	const (
		messageCount = 102132
		leafCount    = 14182
		// checkpoint rows: one per 1,000 calls of a role or per minute of the run, so tens of rows
		jobRoleRowsMax = 100
	)
	f := &ledgerSearchProvider{}
	srv := f.server(t)
	t.Setenv("FAKE_OR_KEY", "synthetic")
	svc := embedSvc(t, t.TempDir(), srv.URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["embed"] = config.Model{Provider: "openrouter", Model: "fake/embed", Dimensions: 8}
		d.Models["summary"] = config.Model{Provider: "openrouter", Model: "fake/summary"}
		d.Memory.Summaries = true
		d.Memory.IndexWorkers = 8
		d.Memory.DerivedBudgetUSD = "0"
	})
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}

	const room = "discord:synthetic:ledger-load"
	start := time.Now().Add(-5 * 365 * 24 * time.Hour)
	observations := make([]corpus.Observation, 0, messageCount)
	for group := 0; group < leafCount; group++ {
		size := 7
		if group >= 11324 {
			size = 8
		}
		groupStart := start.Add(time.Duration(group) * 40 * time.Minute)
		for offset := 0; offset < size; offset++ {
			seq := len(observations)
			at := groupStart.Add(time.Duration(offset) * time.Minute)
			observations = append(observations, corpus.Observation{
				ObservationID:   fmt.Sprintf("synthetic-observation-%06d", seq),
				Platform:        "discord",
				Collection:      "synthetic",
				SourceRoom:      room,
				NativeMessageID: fmt.Sprintf("synthetic-message-%06d", seq),
				ActorLabel:      "synthetic participant",
				Content:         fmt.Sprintf("synthetic message %06d", seq),
				ParsedTime:      at.UTC().Format(time.RFC3339),
				RecordKind:      corpus.HumanText,
			})
		}
	}
	if len(observations) != messageCount {
		t.Fatalf("synthetic observation count=%d, want %d", len(observations), messageCount)
	}
	for i := 0; i < len(observations); i += 2000 {
		if _, err := eng.Store.InsertObservations(observations[i:min(i+2000, len(observations))], nil); err != nil {
			t.Fatal(err)
		}
	}
	eng.MarkDirty(room)

	started := time.Now()
	svc.MemoryPassForTest(context.Background())
	elapsed := time.Since(started)
	embedCalls, summaryCalls := int(f.embeds.Load()), int(f.summaries.Load())
	if embedCalls != 17818 || summaryCalls != leafCount {
		t.Fatalf("fake provider calls: embed=%d summary=%d, want embed=17818 summary=%d", embedCalls, summaryCalls, leafCount)
	}
	if got := svc.LedgerDropped(); got != 0 {
		t.Fatalf("ledger writer dropped %d callbacks", got)
	}
	var rows, calls int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := svc.Store.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(call_count), 0) FROM llm_calls WHERE turn_id='' AND job_id<>''`).Scan(&rows, &calls); err != nil {
			t.Fatal(err)
		}
		if calls == embedCalls+summaryCalls {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rows < 2 || rows > jobRoleRowsMax || calls != embedCalls+summaryCalls {
		t.Fatalf("index ledger rows=%d calls=%d; want 2..%d rows and calls=%d", rows, calls, jobRoleRowsMax, embedCalls+summaryCalls)
	}
	var leaves int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_leaves WHERE room=?`, room).Scan(&leaves); err != nil {
		t.Fatal(err)
	}
	if leaves != leafCount {
		t.Fatalf("indexed leaf count=%d, want %d", leaves, leafCount)
	}
	t.Logf("합성 전체 색인: 메시지 %d, leaf %d, 공급자 요청 %d, 장부 행 %d, 버림 %d, 경과 %s",
		messageCount, leaves, calls, rows, svc.LedgerDropped(), elapsed.Round(time.Millisecond))
}

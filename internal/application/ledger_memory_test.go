package application

import (
	"context"
	"math"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
	"masamong/internal/observability"
)

// A long index job writes checkpoint rows (every memoryLedgerCheckpointCalls calls or
// memoryLedgerCheckpointAge), Close writes what a job still held, and a call with neither a turn nor a
// job leaves no row.
func TestMemoryLedgerCheckpointsSkipsOrphansAndFlushesOnClose(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"ledger"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	s.Clock = clock.Fixed{T: now}
	s.noteMemoryLLMCall(context.Background(), llmCall{Role: "embed", Model: "m", CostUSD: 0.001})
	long := observability.WithJob(context.Background(), "job-long")
	for i := 0; i < 2*memoryLedgerCheckpointCalls+500; i++ {
		s.noteMemoryLLMCall(long, llmCall{Role: "embed", Model: "m", PromptTokens: 3})
	}
	slow := observability.WithJob(context.Background(), "job-slow")
	s.noteMemoryLLMCall(slow, llmCall{Role: "rerank", Model: "r"})
	s.Clock = clock.Fixed{T: now.Add(memoryLedgerCheckpointAge)}
	s.noteMemoryLLMCall(slow, llmCall{Role: "rerank", Model: "r"}) // a minute later: checkpoint
	s.noteMemoryLLMCall(slow, llmCall{Role: "rerank", Model: "r"}) // held until Close
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	db, err := s2.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	var rows, calls, prompt int
	if err := db.DB.QueryRow(`SELECT COUNT(*), SUM(call_count), SUM(prompt_tokens) FROM llm_calls WHERE job_id='job-long'`).Scan(&rows, &calls, &prompt); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || calls != 2*memoryLedgerCheckpointCalls+500 || prompt != 3*calls {
		t.Fatalf("long job: rows=%d calls=%d prompt=%d; want two checkpoints and the rest written by Close", rows, calls, prompt)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*), SUM(call_count) FROM llm_calls WHERE job_id='job-slow'`).Scan(&rows, &calls); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || calls != 3 {
		t.Fatalf("slow job: rows=%d calls=%d; want an age checkpoint of two and one written by Close", rows, calls)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM llm_calls WHERE job_id='' AND turn_id=''`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("orphan engine call rows = %d (%v)", rows, err)
	}
}

// The 32,000 events model a 102,132-message / 14,182-leaf index: 3,192 message batches, 444 leaf
// batches, and one summary plus one summary-vector request for each leaf. The direct burst keeps this
// pressure check small while exercising the same aggregation and bounded writer used by fake-provider tests.
func TestMemoryIndexLedgerAggregatesFullScaleBurst(t *testing.T) {
	const (
		jobID         = "job-full-scale-index"
		messageCalls  = 3192
		leafCalls     = 444
		summaryCalls  = 14182
		totalEmbed    = messageCalls + leafCalls + summaryCalls
		totalRequests = totalEmbed + summaryCalls
	)
	s := jobTestSvc(t)
	ctx := observability.WithJob(context.Background(), jobID)
	started := time.Now()
	for i := 0; i < totalEmbed; i++ {
		s.noteMemoryLLMCall(ctx, llmCall{Role: "embed", Model: "fake/embed", Provider: "fake", PromptTokens: 7,
			Latency: time.Millisecond, CostUSD: 0.000001})
	}
	for i := 0; i < summaryCalls; i++ {
		s.noteMemoryLLMCall(ctx, llmCall{Role: "memory_summary", Model: "fake/summary", Provider: "fake", PromptTokens: 21,
			CompletionTokens: 4, CachedTokens: 2, Latency: time.Millisecond, CostUSD: 0.0000005})
	}
	s.flushMemoryLLMCalls(ctx)

	deadline := time.Now().Add(5 * time.Second)
	var rows int
	for time.Now().Before(deadline) {
		var calls int
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(call_count),0) FROM llm_calls WHERE job_id=?`, jobID).Scan(&rows, &calls); err != nil {
			t.Fatal(err)
		}
		if calls == totalRequests {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// one checkpoint row per memoryLedgerCheckpointCalls calls of a role, plus the remainder
	want := (totalEmbed+memoryLedgerCheckpointCalls-1)/memoryLedgerCheckpointCalls + (summaryCalls+memoryLedgerCheckpointCalls-1)/memoryLedgerCheckpointCalls
	if rows != want || s.LedgerDropped() != 0 {
		t.Fatalf("rows=%d dropped=%d; want %d role checkpoints and no queue loss", rows, s.LedgerDropped(), want)
	}
	got := map[string]struct {
		count, prompt, completion, cached, latency int
		cost                                       float64
	}{}
	dbRows, err := s.Store.DB.Query(`SELECT role, SUM(call_count), SUM(prompt_tokens), SUM(completion_tokens), SUM(cached_tokens), SUM(latency_ms), SUM(cost_usd)
		FROM llm_calls WHERE job_id=? GROUP BY role`, jobID)
	if err != nil {
		t.Fatal(err)
	}
	for dbRows.Next() {
		var role string
		var v struct {
			count, prompt, completion, cached, latency int
			cost                                       float64
		}
		if err := dbRows.Scan(&role, &v.count, &v.prompt, &v.completion, &v.cached, &v.latency, &v.cost); err != nil {
			dbRows.Close()
			t.Fatal(err)
		}
		got[role] = v
	}
	if err := dbRows.Close(); err != nil {
		t.Fatal(err)
	}
	embed, summary := got["embed"], got["memory_summary"]
	if embed.count != totalEmbed || embed.prompt != totalEmbed*7 || embed.completion != 0 || embed.cached != 0 || embed.latency != totalEmbed || math.Abs(embed.cost-float64(totalEmbed)*0.000001) > 1e-8 {
		t.Fatalf("embed aggregate: %+v", embed)
	}
	if summary.count != summaryCalls || summary.prompt != summaryCalls*21 || summary.completion != summaryCalls*4 || summary.cached != summaryCalls*2 || summary.latency != summaryCalls || math.Abs(summary.cost-float64(summaryCalls)*0.0000005) > 1e-8 {
		t.Fatalf("summary aggregate: %+v", summary)
	}
	t.Logf("synthetic full-index burst: %d provider events → %d ledger rows, dropped=%d, elapsed=%s", totalRequests, rows, s.LedgerDropped(), time.Since(started).Round(time.Millisecond))
}

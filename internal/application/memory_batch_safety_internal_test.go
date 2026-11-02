package application

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/providers/openai"
)

func prepareJobsOf(t *testing.T, s *Service, room string) int {
	t.Helper()
	var n int
	if err := s.Store.DB.QueryRow(`SELECT count(*) FROM jobs WHERE type=? AND payload LIKE ?`, jobPrepare, "%"+room+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Finding C: a graceful stop while the job waits on its batch records the job INTERRUPTED; the next start
// runs that same job again (a new attempt) instead of adding a job, and nothing is submitted twice.
func TestBatchRestartRetriesTheInterruptedJob(t *testing.T) {
	t.Cleanup(engine.ShortBatchWaitsForTest())
	host := newBatchHost(openai.BatchRunning)
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "graceful", 24)
	daemon, stop := context.WithCancel(context.Background())
	s.ResumeJobs(daemon)
	started := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch"})
	if !started.OK {
		t.Fatal(started.Error)
	}
	waitFor(t, "the first poll", func() bool { return host.polls.Load() > 0 })
	stop() // the daemon stops gracefully
	first := waitJobDone(t, s, started.JobID)
	if first["status"] != "failed" || first["error_code"] != "INTERRUPTED" {
		t.Fatalf("a graceful stop records the job interrupted: %+v", first)
	}
	if _, ok := batchPlanOf(eng, room); !ok {
		t.Fatal("the plan stays for the next start")
	}
	host.set(openai.BatchDone)
	s.ResumeJobs(context.Background()) // the next start
	done := waitJobDone(t, s, started.JobID)
	if done["status"] != "succeeded" || done["id"] != started.JobID || fmt.Sprint(done["attempt"]) != "2" {
		t.Fatalf("the same job runs again: %+v", done)
	}
	if n := prepareJobsOf(t, s, room); n != 1 {
		t.Fatalf("a restart must not add jobs: %d", n)
	}
	if host.submitted() != 2 || live.Load() != 0 {
		t.Fatalf("batches %d, live %d", host.submitted(), live.Load())
	}
}

// Finding D: a submission the host refuses (a rate limit here) fails the job with the reason; nothing
// runs at the live price and no plan holds the room.
func TestBatchRefusedSubmitFailsTheJobWithoutLiveCalls(t *testing.T) {
	t.Cleanup(engine.ShortBatchWaitsForTest())
	host := newBatchHost(openai.BatchRunning)
	host.submitErr = &openai.APIError{Status: 429, Message: "rate limited"}
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "refused", 24)
	started := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch"})
	if !started.OK {
		t.Fatal(started.Error)
	}
	done := waitJobDone(t, s, started.JobID)
	res := jobResult(t, done)
	if done["status"] != "failed" || done["error_code"] != "BATCH_SUBMIT_REFUSED" || !strings.Contains(res["error"].(string), "refused") {
		t.Fatalf("job %+v result %+v", done, res)
	}
	if live.Load() != 0 {
		t.Fatalf("a refused batch ran live: %d calls", live.Load())
	}
	if _, ok := batchPlanOf(eng, room); ok {
		t.Fatal("no plan after a refusal with nothing sent")
	}
}

// Finding 3: a cancel at a host that cannot cancel ends the job cancelled at once, while the plan drains
// in the background (the room held, nothing live) until the batch ends and its results are applied.
func TestBatchCancelDrainsInTheBackground(t *testing.T) {
	t.Cleanup(engine.ShortBatchWaitsForTest())
	host := newBatchHost(openai.BatchRunning)
	host.noCancel = true
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "drain", 24)
	started := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch"})
	if !started.OK {
		t.Fatal(started.Error)
	}
	waitFor(t, "the first poll", func() bool { return host.polls.Load() > 0 })
	if r := runAction(t, s, "jobs.cancel", map[string]any{"id": started.JobID}); !r.OK {
		t.Fatal(r.Error)
	}
	if got := waitJobDone(t, s, started.JobID); got["status"] != "cancelled" {
		t.Fatalf("job %+v", got)
	}
	// the job row reads cancelled at once; the run then asks the host to cancel, waits a little (the batch
	// keeps running), and hands the plan to a background drain. The batch ends only once that drain runs,
	// so the cancel itself cannot collect it
	waitFor(t, "the background drain", func() bool { _, running := batchDrains.Load(drainKey(s, room)); return running })
	info, ok := batchPlanOf(eng, room)
	if !ok || info.State != engine.BatchPlanDraining {
		t.Fatalf("the plan drains: %v %+v", ok, info)
	}
	b := blockOf(t, s, room)
	if bb, _ := b["batch"].(map[string]any); b["waiting_batch"] != true || bb["state"] != engine.BatchPlanDraining {
		t.Fatalf("the block shows the drain: %+v", b)
	}
	host.set(openai.BatchDone) // the batch ends on its own
	waitFor(t, "the drain", func() bool { _, ok := batchPlanOf(eng, room); return !ok })
	waitFor(t, "the drain to end", func() bool { _, running := batchDrains.Load(drainKey(s, room)); return !running })
	var pending int
	if err := eng.Store.DB.DB.QueryRow(`SELECT count(*) FROM memory_leaves WHERE room=? AND closed=1 AND leaf_vec IS NULL`, room).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || live.Load() != 0 || host.cleanups.Load() != 2 {
		t.Fatalf("drained results applied: pending %d, live %d, cleanups %d", pending, live.Load(), host.cleanups.Load())
	}
}

// Finding 1: a submission whose answer was lost at a host without a batch list fails the job with what to
// do; the block shows it, the room stays held, and memory.blocks.update discard_batch releases it.
func TestBatchUnknownSubmitIsHeldUntilDiscarded(t *testing.T) {
	t.Cleanup(engine.ShortBatchWaitsForTest())
	host := newBatchHost(openai.BatchRunning)
	host.lost = true
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "unknown", 24)
	started := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch"})
	if !started.OK {
		t.Fatal(started.Error)
	}
	done := waitJobDone(t, s, started.JobID)
	res := jobResult(t, done)
	if done["error_code"] != "BATCH_SUBMIT_UNKNOWN" || !strings.Contains(res["error"].(string), "discard_batch") {
		t.Fatalf("job %+v result %+v", done, res)
	}
	b := blockOf(t, s, room)
	if bb, _ := b["batch"].(map[string]any); b["waiting_batch"] != true || b["status"] != "failed" || bb["state"] != engine.BatchPlanSubmitUnknown || bb["reason"] == "" {
		t.Fatalf("the block shows the unknown submission: %+v", b)
	}
	if held, _ := eng.BatchWaitingRooms(); !held[room] {
		t.Fatal("the room stays held")
	}
	if r := runAction(t, s, "memory.blocks.update", map[string]any{"source_room": room, "discard_batch": true}); !r.OK ||
		r.Output["changed"].([]string)[0] != "discard_batch" {
		t.Fatalf("discard %+v", r)
	}
	if _, ok := batchPlanOf(eng, room); ok {
		t.Fatal("the plan is discarded")
	}
	if r := runAction(t, s, "memory.blocks.update", map[string]any{"source_room": room, "discard_batch": true}); r.OK {
		t.Fatal("nothing left to discard")
	}
	if host.submitted() != 2 || live.Load() != 0 {
		t.Fatalf("sent %d (once per group), live %d", host.submitted(), live.Load())
	}
}

// Finding 6: a batch is read through the provider it was submitted to: the engine's adapter while the
// role still uses it, a provider built from the config by id otherwise, and an error when that provider
// is gone or now has another address.
func TestBatchResolverFindsTheSubmittingProvider(t *testing.T) {
	host := newBatchHost(openai.BatchRunning)
	s, eng, _ := batchService(t, host)
	r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Providers["other"] = config.Provider{Kind: config.KindOpenAI, BaseURL: "https://batch.example.invalid/v1", CredentialRef: "env:FAKE_BATCH_EMBED_KEY"}
		return nil
	})
	if !r.OK {
		t.Fatal(r.Error)
	}
	resolve := s.batchResolver(s.Doc(), eng)
	own := engine.BatchProvider{ID: "batchhost", BaseURL: eng.LLM.BaseURL, API: eng.LLM.API}
	if c, err := resolve(own); err != nil || c != engine.BatchClient(eng.LLM) {
		t.Fatalf("the role's own adapter: %v %v", c, err)
	}
	other := engine.BatchProvider{ID: "other", BaseURL: "https://batch.example.invalid/v1", API: openai.ResolveAPI("", "https://batch.example.invalid/v1")}
	c, err := resolve(other)
	if a, ok := c.(*openai.Adapter); err != nil || !ok || a == eng.LLM || a.BaseURL != other.BaseURL {
		t.Fatalf("a provider the role no longer uses is built by id: %v %v", c, err)
	}
	moved := own
	moved.BaseURL = "https://moved.example.invalid/v1"
	if _, err := resolve(moved); err == nil {
		t.Fatal("a provider at another address cannot read the batch")
	}
	if _, err := resolve(engine.BatchProvider{ID: "removed", BaseURL: own.BaseURL, API: own.API}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("a removed provider: %v", err)
	}
}

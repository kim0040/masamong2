package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/jobs"
	"masamong/internal/memory/engine"
	"masamong/internal/providers/openai"
)

// batchHost is a provider batch API in memory for the prepare job: it accepts embedding batches,
// reports the state the test sets and answers every input with a small vector.
type batchHost struct {
	mu       sync.Mutex
	state    openai.BatchState
	subs     map[string][]openai.BatchItem
	order    []string
	polls    atomic.Int32
	cancels  atomic.Int32
	cleanups atomic.Int32
	// noCancel: the host cannot cancel (OpenRouter); its batches run to their end
	noCancel bool
	// submitErr refuses every create call; lost takes them but loses the answer (and keeps no list)
	submitErr error
	lost      bool
}

func newBatchHost(state openai.BatchState) *batchHost {
	return &batchHost{state: state, subs: map[string][]openai.BatchItem{}}
}

func (h *batchHost) BatchSupport(kind openai.BatchKind) bool { return kind == openai.BatchEmbed }
func (h *batchHost) BatchMaxItems(openai.BatchKind) int      { return 0 }

func (h *batchHost) BatchDiscount(openai.BatchKind, string) float64 { return 0.5 }
func (h *batchHost) BatchEmbedCost(string, int, time.Time) *float64 { return nil }
func (h *batchHost) ReconcileBatch(_ context.Context, ref openai.BatchRef) (openai.BatchRef, error) {
	return openai.BatchRef{}, &openai.SubmitUnknownError{Ref: ref, Err: errors.New("the host keeps no list")}
}

func (h *batchHost) SubmitBatch(_ context.Context, kind openai.BatchKind, model string, items []openai.BatchItem, key string) (openai.BatchRef, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.submitErr != nil {
		return openai.BatchRef{}, h.submitErr
	}
	id := fmt.Sprintf("batch-%d", len(h.order)+1)
	h.subs[id] = append([]openai.BatchItem(nil), items...)
	h.order = append(h.order, id)
	if h.lost {
		return openai.BatchRef{}, &openai.SubmitUnknownError{Err: errors.New("connection reset after the request was sent")}
	}
	return openai.BatchRef{API: "fake", ID: id, Kind: kind, Model: model, Count: len(items), SubmittedAt: time.Now(), SubmitKey: key}, nil
}

func (h *batchHost) PollBatch(_ context.Context, ref openai.BatchRef) (openai.BatchStatus, error) {
	h.polls.Add(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	items, ok := h.subs[ref.ID]
	if !ok {
		return openai.BatchStatus{}, errors.New("unknown batch")
	}
	return openai.BatchStatus{State: h.state, Total: len(items)}, nil
}

func (h *batchHost) BatchResults(_ context.Context, ref openai.BatchRef) ([]openai.BatchOutcome, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == openai.BatchCancelled {
		return nil, nil // nothing had finished
	}
	var out []openai.BatchOutcome
	for _, it := range h.subs[ref.ID] {
		cost := 0.000001
		res := &openai.EmbedResult{CostUSD: &cost}
		for i := range it.Input {
			v := make([]float32, 8)
			v[i%8] = 1
			res.Vectors = append(res.Vectors, v)
		}
		out = append(out, openai.BatchOutcome{ID: it.ID, Embed: res})
	}
	return out, nil
}

func (h *batchHost) CancelBatch(context.Context, openai.BatchRef) error {
	h.cancels.Add(1)
	if h.noCancel {
		return openai.ErrBatchUnsupported
	}
	h.set(openai.BatchCancelled)
	return nil
}

func (h *batchHost) CleanupBatch(context.Context, openai.BatchRef) error {
	h.cleanups.Add(1)
	return nil
}

func (h *batchHost) set(s openai.BatchState) {
	h.mu.Lock()
	h.state = s
	h.mu.Unlock()
}

func (h *batchHost) submitted() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.order)
}

// batchService is a service whose embed role is a counting test server, with captures, embeddings and
// no summaries, and whose engine uses host for batches.
func batchService(t *testing.T, host engine.BatchClient) (*Service, *engine.Engine, *atomic.Int32) {
	t.Helper()
	var live atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			http.Error(w, `{"error":{"message":"not here"}}`, http.StatusBadRequest)
			return
		}
		live.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var data []map[string]any
		for i := range req.Input {
			v := make([]float32, 8)
			v[(i+3)%8] = 1
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"cost": 0.000002}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_BATCH_EMBED_KEY", "synthetic")
	s := privacyService(t)
	r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Providers["batchhost"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_BATCH_EMBED_KEY"}
		d.Models["embed"] = config.Model{Provider: "batchhost", Model: "fake/embed"}
		d.Memory.Embed, d.Memory.Capture, d.Memory.Summaries = true, true, false
		return nil
	})
	if !r.OK {
		t.Fatal(r.Error)
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	eng.EmbedBatcher = host
	return s, eng, &live
}

func captureRoom(t *testing.T, eng *engine.Engine, channel string, n int) string {
	t.Helper()
	start := time.Now().Add(-time.Duration(n+90) * time.Minute)
	for i := 0; i < n; i++ {
		m := engine.LiveMessage{GuildID: "g", ChannelID: channel, MessageID: fmt.Sprintf("%s-%d", channel, i), AuthorID: "u",
			AuthorName: "철수", Content: fmt.Sprintf("주말 등산 계획 %d번째 메시지", i), At: start.Add(time.Duration(i) * time.Minute)}
		if _, err := eng.Capture(m); err != nil {
			t.Fatal(err)
		}
	}
	return engine.RoomKey("g", channel)
}

func runAction(t *testing.T, s *Service, act string, in map[string]any) action.Result {
	t.Helper()
	raw, _ := json.Marshal(in)
	return s.Execute(context.Background(), action.Request{Action: act, Input: raw, Locale: "en"})
}

func waitJobDone(t *testing.T, s *Service, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r := runAction(t, s, "jobs.get", map[string]any{"id": id})
		if r.OK {
			switch r.Output["status"] {
			case "succeeded", "failed", "cancelled":
				return r.Output
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func jobResult(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	var res map[string]any
	raw, _ := json.Marshal(out["result"])
	if err := json.Unmarshal(raw, &res); err != nil {
		if str, ok := out["result"].(string); ok {
			_ = json.Unmarshal([]byte(str), &res)
		}
	}
	return res
}

func blockOf(t *testing.T, s *Service, room string) map[string]any {
	t.Helper()
	for _, b := range s.mergedBlocks() {
		if b["source_room"] == room {
			return b
		}
	}
	return nil
}

// memory.blocks.prepare in batch mode: a dry run prices the batch, the job submits it, waits, applies the
// results and ends like a live preparation; only leftovers would be paid live (here: none).
func TestBatchPrepareJobEndToEnd(t *testing.T) {
	host := newBatchHost(openai.BatchDone)
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "hiking", 24)
	if r := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "weekly"}); r.OK {
		t.Fatal("an unknown mode must be refused")
	}
	dry := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch", "dry_run": true})
	if !dry.OK || dry.Output["mode"] != "batch" || dry.Output["note"] == nil {
		t.Fatalf("dry run %+v", dry)
	}
	rep := dry.Output["report"].(engine.IndexReport)
	if rep.Estimate == nil || rep.Estimate.BatchUSD == nil || *rep.Estimate.BatchUSD >= rep.Estimate.TotalUSD || rep.Mode != "batch" {
		t.Fatalf("a batch dry run is priced at batch prices: %+v", rep.Estimate)
	}
	// batch always runs as a job, even without background
	started := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch", "budget_usd": "1"})
	if !started.OK || started.JobID == "" || started.Output["mode"] != "batch" || started.Output["background"] != true {
		t.Fatalf("start %+v", started)
	}
	done := waitJobDone(t, s, started.JobID)
	res := jobResult(t, done)
	// 3 leaves and their 24 messages (memory.message_vectors is on by default), in two batches
	if done["status"] != "succeeded" || res["mode"] != "batch" || res["batch_items"].(float64) != 27 || res["batch_failed"].(float64) != 0 ||
		res["live_items"].(float64) != 0 || res["leaf_vectors"].(float64) != 3 || res["message_vectors"].(float64) != 24 {
		t.Fatalf("job %+v result %+v", done, res)
	}
	if live.Load() != 0 || host.submitted() != 2 || host.cleanups.Load() != 2 {
		t.Fatalf("live calls %d, batches %d, cleanups %d", live.Load(), host.submitted(), host.cleanups.Load())
	}
	b := blockOf(t, s, room)
	if b["status"] != "ready" || b["waiting_batch"] != nil {
		t.Fatalf("block after the batch %+v", b)
	}
	if _, ok := batchPlanOf(eng, room); ok {
		t.Fatal("the plan must be deleted once finished")
	}
	// the block can keep batch as its default mode
	if r := runAction(t, s, "memory.blocks.update", map[string]any{"source_room": room, "mode": "batch"}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := runAction(t, s, "memory.blocks.update", map[string]any{"source_room": room, "mode": "sometimes"}); r.OK {
		t.Fatal("an unknown default mode must be refused")
	}
	if b := blockOf(t, s, room); b["mode"] != "batch" {
		t.Fatalf("block mode %v", b["mode"])
	}
	again := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room})
	if !again.OK || again.Output["mode"] != "batch" {
		t.Fatalf("the block's default mode applies: %+v", again)
	}
	waitJobDone(t, s, again.JobID)
}

// After a restart, a prepare job that was waiting on its batch runs again by itself and polls the same
// batch (nothing is submitted twice); a stored plan whose job is gone gets a job of its own.
func TestBatchPrepareResumesAfterRestart(t *testing.T) {
	host := newBatchHost(openai.BatchRunning)
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "resume", 24)
	orphan := captureRoom(t, eng, "orphan", 16)
	for _, r := range []string{room, orphan} {
		plan, _, err := eng.PrepareBatch(context.Background(), r, engine.BatchOptions{BudgetUSD: 1}, nil)
		if err != nil || len(plan.Groups) != 2 {
			t.Fatalf("submit %s: %v %+v", r, err, plan)
		}
	}
	// the job of the first room was running when the process stopped
	raw, _ := json.Marshal(preparePayload{SourceRoom: room, BudgetUSD: 1, Mode: prepareBatch})
	j, err := s.Enqueue(jobPrepare, "", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if j, err = s.TakeLease(j.ID, "lease_dead_run", jobLeaseTTL); err != nil || j.Status != jobs.Running {
		t.Fatalf("lease %v %+v", err, j)
	}
	if b := blockOf(t, s, room); b["status"] != "preparing" || b["waiting_batch"] != true {
		t.Fatalf("a waiting block reads preparing, waiting_batch: %+v", b)
	}
	host.set(openai.BatchDone)
	s.ResumeJobs(context.Background())
	done := waitJobDone(t, s, j.ID)
	if done["status"] != "succeeded" || fmt.Sprint(done["attempt"]) != "2" {
		t.Fatalf("resumed job %+v", done)
	}
	var orphanJob string
	for id, jj := range s.latestJobs() {
		if id == orphan {
			orphanJob = jj.ID
		}
	}
	if orphanJob == "" {
		t.Fatal("the orphan plan must get a job")
	}
	if o := waitJobDone(t, s, orphanJob); o["status"] != "succeeded" {
		t.Fatalf("orphan job %+v", o)
	}
	if host.submitted() != 4 || host.polls.Load() == 0 || live.Load() != 0 {
		t.Fatalf("batches %d (want the 4 from before the restart), polls %d, live %d", host.submitted(), host.polls.Load(), live.Load())
	}
	for _, r := range []string{room, orphan} {
		if _, ok := batchPlanOf(eng, r); ok {
			t.Fatalf("plan of %s left behind", r)
		}
	}
}

// A cancel from the console while the batch waits cancels it at the host, keeps what finished (here
// nothing), deletes the plan and ends the job cancelled.
func TestBatchPrepareCancelStopsTheBatch(t *testing.T) {
	host := newBatchHost(openai.BatchRunning)
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "cancel", 24)
	started := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch"})
	if !started.OK {
		t.Fatal(started.Error)
	}
	deadline := time.Now().Add(10 * time.Second)
	for host.polls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b := blockOf(t, s, room); b["status"] != "preparing" || b["waiting_batch"] != true {
		t.Fatalf("waiting block %+v", b)
	}
	if r := runAction(t, s, "jobs.cancel", map[string]any{"id": started.JobID}); !r.OK {
		t.Fatal(r.Error)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		if _, ok := batchPlanOf(eng, room); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled plan was not deleted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := waitJobDone(t, s, started.JobID); got["status"] != "cancelled" {
		t.Fatalf("job %+v", got)
	}
	if host.cancels.Load() == 0 || live.Load() != 0 {
		t.Fatalf("cancels %d live %d", host.cancels.Load(), live.Load())
	}
}

// The background index pass leaves a room whose batch is at the provider alone (its leaves are paid
// for there) and keeps it queued; other rooms are indexed as usual, and the room is indexed again once
// its batch is done.
func TestMemoryPassSkipsRoomWaitingForBatch(t *testing.T) {
	host := newBatchHost(openai.BatchRunning)
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "waiting", 24)
	plan, _, err := eng.PrepareBatch(context.Background(), room, engine.BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 {
		t.Fatalf("%v %+v", err, plan)
	}
	other := captureRoom(t, eng, "busy", 16)
	eng.MarkDirty(room, other)
	s.memoryPass(context.Background())
	pending := func(r string) int {
		var n int
		if err := eng.Store.DB.DB.QueryRow(`SELECT count(*) FROM memory_leaves WHERE room=? AND closed=1 AND leaf_vec IS NULL`, r).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if pending(room) != 3 || pending(other) != 0 || live.Load() == 0 {
		t.Fatalf("waiting room pending %d (want 3), other %d, live calls %d", pending(room), pending(other), live.Load())
	}
	queued := false
	for _, r := range eng.QueuedRooms() {
		queued = queued || r == room
	}
	if !queued {
		t.Fatal("the waiting room must stay queued")
	}
	// memory.reindex leaves it out too
	r := runAction(t, s, "memory.reindex", map[string]any{})
	if !r.OK || fmt.Sprint(r.Output["report"].(engine.IndexReport).BatchWaiting) != fmt.Sprint([]string{room}) || r.Output["note"] == nil {
		t.Fatalf("reindex %+v", r.Output)
	}
	if pending(room) != 3 {
		t.Fatal("reindex embedded a waiting room live")
	}
	// a live prepare of the room resumes its batch instead of paying again
	resumed := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "background": true})
	if !resumed.OK || resumed.Output["mode"] != "batch" || resumed.Output["batch_resumed"] != true {
		t.Fatalf("live prepare with a batch in flight %+v", resumed)
	}
	calls := live.Load()
	host.set(openai.BatchDone)
	if got := waitJobDone(t, s, resumed.JobID); got["status"] != "succeeded" {
		t.Fatalf("resumed job %+v", got)
	}
	if pending(room) != 0 || live.Load() != calls || host.submitted() != 2 {
		t.Fatalf("after the batch: pending %d, live calls %d→%d, batches %d", pending(room), calls, live.Load(), host.submitted())
	}
}

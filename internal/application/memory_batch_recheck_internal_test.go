package application

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/providers/openai"
)

// R6: discarding a plan that drains in the background stops that drain, and a new preparation of the
// room then starts at once instead of waiting for the discarded batch, which keeps running at the host.
func TestBatchDiscardRetiresTheBackgroundDrain(t *testing.T) {
	t.Cleanup(engine.ShortBatchWaitsForTest())
	host := newBatchHost(openai.BatchRunning)
	host.noCancel = true // OpenRouter: the batch runs to its end
	s, eng, live := batchService(t, host)
	room := captureRoom(t, eng, "discard-drain", 24)
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
	waitFor(t, "the background drain", func() bool { _, running := batchDrains.Load(drainKey(s, room)); return running })
	sent := host.submitted()
	if r := runAction(t, s, "memory.blocks.update", map[string]any{"source_room": room, "discard_batch": true}); !r.OK {
		t.Fatalf("discard %+v", r)
	}
	if _, running := batchDrains.Load(drainKey(s, room)); running {
		t.Fatal("the discard retires the plan's drain")
	}
	if _, ok := batchPlanOf(eng, room); ok {
		t.Fatal("the plan is discarded")
	}
	again := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch"})
	if !again.OK {
		t.Fatal(again.Error)
	}
	waitFor(t, "the new preparation's batches", func() bool { return host.submitted() == 2*sent })
	if r := runAction(t, s, "jobs.cancel", map[string]any{"id": again.JobID}); !r.OK {
		t.Fatal(r.Error)
	}
	waitJobDone(t, s, again.JobID)
	if live.Load() != 0 {
		t.Fatalf("nothing ran live: %d", live.Load())
	}
}

// summaryHost batches summary chats only, for a model whose request shape is not learned yet: a batch
// preparation probes the summary model live first.
type summaryHost struct{ *batchHost }

func (summaryHost) BatchSupport(kind openai.BatchKind) bool        { return kind == openai.BatchChat }
func (summaryHost) ShapeLearned(string) bool                       { return false }
func (summaryHost) LearnRefusal(openai.ChatRequest, error) bool    { return false }
func (summaryHost) BatchDiscount(openai.BatchKind, string) float64 { return 0.5 }

// R8: when the live summary probe takes the room's only summary and nothing goes to a batch, the live
// pass that follows counts the probe's cost against the budget: a probe that spent it starts nothing more.
func TestBatchProbeSpendCountsAgainstTheLivePass(t *testing.T) {
	t.Cleanup(engine.ShortBatchWaitsForTest())
	var embeds, chats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/embeddings":
			embeds.Add(1)
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var data []map[string]any
			for i := range req.Input {
				v := make([]float32, 8)
				v[i%8] = 1
				data = append(data, map[string]any{"index": i, "embedding": v})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"cost": 0.000002}})
		case "/chat/completions":
			chats.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "주말 등산 계획을 정한 대화"}}},
				"usage": map[string]any{"prompt_tokens": 40, "completion_tokens": 8, "cost": 0.01}})
		default:
			http.Error(w, `{"error":{"message":"not here"}}`, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_BATCH_EMBED_KEY", "synthetic")
	s := privacyService(t)
	if r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Providers["batchhost"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_BATCH_EMBED_KEY"}
		d.Models["embed"] = config.Model{Provider: "batchhost", Model: "fake/embed"}
		d.Models["summary"] = config.Model{Provider: "batchhost", Model: "fake/summary"}
		d.Memory.Embed, d.Memory.Capture, d.Memory.Summaries = true, true, true
		return nil
	}); !r.OK {
		t.Fatal(r.Error)
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	host := summaryHost{newBatchHost(openai.BatchRunning)}
	eng.SummaryBatcher = host                      // embeddings cannot batch on this host: they would run live
	room := captureRoom(t, eng, "probe-budget", 8) // one leaf, one summary: the probe's
	started := runAction(t, s, "memory.blocks.prepare", map[string]any{"source_room": room, "mode": "batch", "budget_usd": "0.005"})
	if !started.OK {
		t.Fatal(started.Error)
	}
	done := waitJobDone(t, s, started.JobID)
	res := jobResult(t, done)
	if chats.Load() != 1 || host.submitted() != 0 {
		t.Fatalf("one live probe, no batch: chats %d, batches %d, job %+v", chats.Load(), host.submitted(), done)
	}
	if embeds.Load() != 0 {
		t.Fatalf("the probe spent the budget: no live embedding may start (%d), job %+v", embeds.Load(), res)
	}
	if fmt.Sprint(res["spent_usd"]) != "0.01" || res["stopped_by"] != "budget" {
		t.Fatalf("the report carries the probe and the budget stop: %+v", res)
	}
}

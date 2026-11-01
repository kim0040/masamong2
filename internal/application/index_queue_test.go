package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/observability"
)

// flakyEmbedder serves /embeddings, failing while down is set.
type flakyEmbedder struct {
	down      atomic.Bool
	width     atomic.Int32
	calls     atomic.Int32
	expensive atomic.Bool
}

func (f *flakyEmbedder) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" || f.down.Load() {
			http.Error(w, `{"error":{"message":"rejected"}}`, http.StatusBadRequest) // not retried: keeps the test fast
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.calls.Add(1)
		var data []map[string]any
		for i := range req.Input {
			dims := int(f.width.Load())
			if dims == 0 {
				dims = 8
			}
			v := make([]float32, dims)
			v[i%dims] = 1
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		cost := 0.000001
		if f.expensive.Load() {
			cost = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"cost": cost}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMemoryPassMessageVectorWidthRepairQueuesQuietRoom(t *testing.T) {
	f := &flakyEmbedder{}
	srv := f.server(t)
	t.Setenv("FAKE_OR_KEY", "synthetic")
	svc := embedSvc(t, t.TempDir(), srv.URL)
	ctx := context.Background()
	const active, quiet = "discord:g:active-width", "discord:g:quiet-width"
	capture(t, svc, "active-width", 9)
	capture(t, svc, "quiet-width", 17)
	svc.MemoryPassForTest(ctx)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.QueuedRooms()) != 0 || eng.MessageVectorDims() != 8 {
		t.Fatal("initial pass must finish both rooms at width 8")
	}
	// Only the active room receives a new message, which establishes the provider's new width.
	f.width.Store(6)
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "active-width", MessageID: "new-width",
		AuthorName: "synthetic", Content: "synthetic new width message", At: time.Now().Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if q := eng.QueuedRooms(); len(q) != 1 || q[0] != active {
		t.Fatalf("only the active room must start dirty: %v", q)
	}
	svc.MemoryPassForTest(ctx)
	lo, hi, err := eng.Store.LeafVectorWidths(eng.EmbedSpace(), eng.Settings.Recipe.Version())
	if err != nil || lo != 6 || hi != 8 || eng.MessageSpace() != "" || !slices.Contains(svc.IndexQueueForTest(), active) {
		t.Fatalf("mixed pass must retain the active room: widths=%d/%d err=%v", lo, hi, err)
	}
	// Limit the next pass to one leaf batch, proving the quiet room stays durably queued mid-repair.
	eng.Settings.EmbedBatch, eng.Settings.Workers = 1, 1
	f.expensive.Store(true)
	svc.MemoryPassForTest(ctx)
	lo, hi, err = eng.Store.LeafVectorWidths(eng.EmbedSpace(), eng.Settings.Recipe.Version())
	if err != nil || lo == hi || !slices.Contains(eng.QueuedRooms(), quiet) || !slices.Contains(svc.IndexQueueForTest(), quiet) {
		t.Fatalf("quiet room must stay queued until convergence: widths=%d/%d err=%v", lo, hi, err)
	}
	f.expensive.Store(false)
	eng.Settings.EmbedBatch = 64
	for pass := 0; pass < 3 && len(eng.QueuedRooms()) > 0; pass++ {
		svc.MemoryPassForTest(ctx)
	}
	lo, hi, err = eng.Store.LeafVectorWidths(eng.EmbedSpace(), eng.Settings.Recipe.Version())
	if err != nil || lo != 6 || hi != 6 || eng.MessageSpace() != "fake/embed:6:l2" || eng.Index().MsgVecs.Dims != 6 || len(svc.IndexQueueForTest()) != 0 {
		t.Fatalf("automatic passes did not converge both rooms: widths=%d/%d err=%v", lo, hi, err)
	}
	if n, err := eng.Store.CountPendingMessages(nil, eng.MessageSpace(), eng.Settings.Recipe.Version()); err != nil || n != 0 {
		t.Fatalf("quiet room messages must also finish indexing: pending=%d err=%v", n, err)
	}
	before := f.calls.Load()
	svc.MemoryPassForTest(ctx)
	if f.calls.Load() != before {
		t.Fatal("converged automatic pass made an extra provider call")
	}
}

func embedSvc(t *testing.T, dir string, url string) *application.Service {
	t.Helper()
	svc, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if url != "" {
		raw, _ := json.Marshal(map[string]any{"instance": "gate"})
		if res := exec(svc, "setup.resume", map[string]any{"instance": "gate"}); !res.OK {
			t.Fatalf("setup: %+v %s", res.Error, raw)
		}
		applyDoc(t, svc, func(d *config.Document) {
			d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: url, CredentialRef: "env:FAKE_OR_KEY"}
			d.Models["embed"] = config.Model{Provider: "openrouter", Model: "fake/embed"}
			d.Memory.Embed, d.Memory.Capture, d.Memory.Summaries = true, true, false
		})
	}
	return svc
}

// capture stores n messages one minute apart in one channel (8 make a closed leaf).
func capture(t *testing.T, svc *application.Service, channel string, n int) {
	t.Helper()
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Duration(n+60) * time.Minute)
	for i := 0; i < n; i++ {
		m := engine.LiveMessage{GuildID: "g", ChannelID: channel, MessageID: fmt.Sprintf("%s-%d", channel, i), AuthorID: "u",
			AuthorName: "철수", Content: fmt.Sprintf("가평 캠핑 얘기 %d", i), At: start.Add(time.Duration(i) * time.Minute)}
		if _, err := eng.Capture(m); err != nil {
			t.Fatal(err)
		}
	}
}

func embedded(t *testing.T, svc *application.Service, room string) int {
	t.Helper()
	eng, _ := svc.MemoryEngine()
	rs, err := eng.Store.Rooms()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Room.Room == room {
			return r.Embedded
		}
	}
	return 0
}

// A pass whose embedding calls fail must keep its rooms queued: only embedded leaves are searchable,
// and a quiet channel would otherwise wait for its next message to be indexed at all.
func TestIndexPassRequeuesRoomsAfterProviderError(t *testing.T) {
	f := &flakyEmbedder{}
	srv := f.server(t)
	t.Setenv("FAKE_OR_KEY", "k")
	svc := embedSvc(t, t.TempDir(), srv.URL)
	const room = "discord:g:quiet"
	capture(t, svc, "quiet", 9)
	if q := svc.IndexQueueForTest(); !slices.Contains(q, room) {
		t.Fatalf("captured room not recorded in the queue: %v", q)
	}

	f.down.Store(true)
	svc.MemoryPassForTest(context.Background())
	if n := embedded(t, svc, room); n != 0 {
		t.Fatalf("embedded %d leaves with the provider down", n)
	}
	if q := svc.IndexQueueForTest(); !slices.Contains(q, room) {
		t.Fatalf("room dropped from the queue after a failed pass: %v", q)
	}
	eng, _ := svc.MemoryEngine()
	if !slices.Contains(eng.QueuedRooms(), room) {
		t.Fatal("room not queued in the engine after a failed pass")
	}

	f.down.Store(false) // no new message arrives in the room
	svc.MemoryPassForTest(context.Background())
	if n := embedded(t, svc, room); n == 0 {
		t.Fatal("the next pass did not retry the room")
	}
	if q := svc.IndexQueueForTest(); len(q) != 0 {
		t.Fatalf("queue not cleared after a clean pass: %v", q)
	}
}

// Rooms captured into but not indexed before the process stopped are indexed after a restart.
func TestIndexQueueSurvivesRestart(t *testing.T) {
	f := &flakyEmbedder{}
	srv := f.server(t)
	t.Setenv("FAKE_OR_KEY", "k")
	dir := t.TempDir()
	svc := embedSvc(t, dir, srv.URL)
	const room = "discord:g:restart"
	capture(t, svc, "restart", 9)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	again := embedSvc(t, dir, "")
	eng, err := again.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(eng.QueuedRooms(), room) {
		t.Fatalf("restarted engine lost the queued room: %v", eng.QueuedRooms())
	}
	again.MemoryPassForTest(context.Background())
	if n := embedded(t, again, room); n == 0 {
		t.Fatal("room captured before the restart was not indexed")
	}
}

type ledgerSearchProvider struct {
	embeds    atomic.Int32
	reranks   atomic.Int32
	summaries atomic.Int32
}

func (f *ledgerSearchProvider) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/embeddings":
			f.embeds.Add(1)
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			data := make([]map[string]any, len(req.Input))
			for i := range req.Input {
				v := make([]float32, 8)
				v[i%len(v)] = 1
				data[i] = map[string]any{"index": i, "embedding": v}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"prompt_tokens": 8, "cost": 0.000001}})
		case "/rerank":
			f.reranks.Add(1)
			var req struct {
				TopN int `json:"top_n"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			results := make([]map[string]any, req.TopN)
			for i := range results {
				results[i] = map[string]any{"index": i, "relevance_score": 0.9 - 0.1*float64(i)}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results, "usage": map[string]any{"total_tokens": 12, "cost": 0.000002}})
		case "/chat/completions":
			n := f.summaries.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": fmt.Sprintf("synthetic summary %d", n)}, "finish_reason": "stop"}},
				"usage": map[string]any{"prompt_tokens": 21, "completion_tokens": 4, "cost": 0.000001}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMemorySearchCallLedgerAndIndexJobSummary(t *testing.T) {
	f := &ledgerSearchProvider{}
	srv := f.server(t)
	t.Setenv("FAKE_OR_KEY", "k")
	svc := embedSvc(t, t.TempDir(), srv.URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["rerank"] = config.Model{Provider: "openrouter", Model: "fake/rerank"}
	})
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	capture(t, svc, "ledger", 9)
	const room = "discord:g:ledger"
	svc.MemoryPassForTest(context.Background())
	indexEmbeds := int(f.embeds.Load())
	if indexEmbeds == 0 {
		t.Fatal("synthetic index made no embedding requests")
	}
	if got := svc.LedgerDropped(); got != 0 {
		t.Fatalf("index call summaries were dropped: %d", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	var jobID string
	var jobCalls int
	for time.Now().Before(deadline) {
		rows, err := svc.Store.DB.Query(`SELECT job_id, call_count FROM llm_calls WHERE turn_id='' AND job_id<>'' AND role='embed'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id string
			var count int
			if err := rows.Scan(&id, &count); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			jobID, jobCalls = id, count
		}
		rows.Close()
		if jobID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if jobID == "" || jobCalls != indexEmbeds {
		t.Fatalf("index ledger job=%q calls=%d, provider requests=%d", jobID, jobCalls, indexEmbeds)
	}
	if got := svc.LedgerDropped(); got != 0 {
		t.Fatalf("ledger writer dropped rows: %d", got)
	}

	_, before, err := eng.Store.SpendSince("2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	turnCtx := observability.WithTurn(context.Background(), "turn-memory-ledger")
	request := engine.SearchRequest{Query: "가평 캠핑", Rooms: []string{room}, Ranking: retrieval.Rerank,
		Intent: search.Intent{Kind: "other", Source: "none"}, RemainingUSD: 1}
	if _, err := eng.Search(turnCtx, request); err != nil {
		t.Fatal(err)
	}
	_, afterFirst, err := eng.Store.SpendSince("2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Search(turnCtx, request); err != nil {
		t.Fatal(err)
	}
	_, afterSecond, err := eng.Store.SpendSince("2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs((afterFirst-before)-(0.000001+0.000002)) > 1e-9 || math.Abs((afterSecond-afterFirst)-0.000002) > 1e-9 {
		t.Fatalf("memory_spend changed unexpectedly: before=%f after1=%f after2=%f", before, afterFirst, afterSecond)
	}

	deadline = time.Now().Add(5 * time.Second)
	var turnRoles = map[string]int{}
	for time.Now().Before(deadline) {
		rows, err := svc.Store.DB.Query(`SELECT role, call_count, provider FROM llm_calls WHERE turn_id=? ORDER BY id`, "turn-memory-ledger")
		if err != nil {
			t.Fatal(err)
		}
		turnRoles = map[string]int{}
		for rows.Next() {
			var role, provider string
			var count int
			if err := rows.Scan(&role, &count, &provider); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if count != 1 || provider != "openrouter" {
				rows.Close()
				t.Fatalf("turn call role=%s count=%d provider=%s", role, count, provider)
			}
			turnRoles[role]++
		}
		rows.Close()
		if turnRoles["embed"] >= 1 && turnRoles["rerank"] >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if turnRoles["embed"] != 1 || turnRoles["rerank"] != 2 {
		t.Fatalf("turn ledger roles: %v", turnRoles)
	}
	if got := f.embeds.Load() - int32(indexEmbeds); got != 1 {
		t.Fatalf("two equal searches made %d query embeddings; want one cache miss", got)
	}
}

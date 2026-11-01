package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// A local-only provider with widths large enough to prove both the legacy cut and native storage.
func messageWidthEngine(t *testing.T, dims int) (*Engine, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	e, _ := messageEngine(t)
	t.Cleanup(e.Close)
	width, calls := new(atomic.Int32), new(atomic.Int32)
	width.Store(int32(dims))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var data []map[string]any
		for i := range req.Input {
			v := make([]float32, int(width.Load()))
			for j := range v {
				v[j] = float32(j%17 + 1)
			}
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	cred, err := providers.Resolve("env:FAKE_KEY")
	if err != nil {
		t.Fatal(err)
	}
	e.LLM = openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
	e.Models = Models{Embed: "gemini-embedding-2"}
	return e, width, calls
}

func assertMessagePending(t *testing.T, e *Engine, want int) {
	t.Helper()
	n, err := e.Store.CountPendingMessages(nil, e.MessageSpace(), e.Settings.Recipe.Version())
	if err != nil || n != want {
		t.Fatalf("pending=%d want=%d err=%v", n, want, err)
	}
}

func TestNonMRLWidthChangeRebuildsWithoutMixingSpaces(t *testing.T) {
	old, _, calls := messageWidthEngine(t, 1536)
	old.Settings.MessageDims = 1024 // the previous default, not the MRL 512 default
	seed(t, old)
	ctx := context.Background()
	if _, err := old.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if old.MessageSpace() != "gemini-embedding-2:1024:l2" || old.Index().MsgVecs.Dims != 1024 || old.Index().LeafVecs.Dims != 1536 {
		t.Fatal("legacy messages must cut the provider's 1536 dimensions to 1024 while leaves keep 1536")
	}
	assertMessagePending(t, old, 0)
	settings := old.Settings
	settings.MessageDims = 0
	next := New(old.Store, old.LLM, old.Models, settings)
	t.Cleanup(next.Close)
	before := calls.Load()
	if next.MessageSpace() != "gemini-embedding-2:1536:l2" {
		t.Fatalf("space=%q", next.MessageSpace())
	}
	info, err := next.Reload()
	if err != nil || info["message_vectors"] != 0 || next.Index().HasMessageVectors() || calls.Load() != before {
		t.Fatalf("upgrade must exclude old rows without calling the provider: info=%v err=%v", info, err)
	}
	assertMessagePending(t, next, seededEligible)
	req := SearchRequest{Query: "synthetic query", Rooms: []string{"discord:1:10", "discord:1:99"}, Ranking: retrieval.Hybrid, Intent: search.Intent{Kind: "other", Source: "none"}}
	res, err := next.Search(ctx, req)
	if err != nil || len(res.Hits) == 0 {
		t.Fatal("leaf search must remain available during upgrade")
	}
	if _, has := res.Trace["message_dense"]; has {
		t.Fatal("old-space rows were searched")
	}

	// Upgrade #4: N -> N-2 -> 0, with old and new rows coexisting in the database mid-upgrade.
	rep, err := next.IndexRooms(ctx, IndexOptions{BudgetUSD: 1, Limit: 2}, nil)
	if err != nil || rep.MessageVectors != 2 {
		t.Fatalf("partial rebuild: report=%+v err=%v", rep, err)
	}
	assertMessagePending(t, next, seededEligible-2)
	total, current, err := next.Store.MessageVectorCounts(next.MessageSpace())
	if err != nil || total != seededEligible || current != 2 || next.Index().MsgVecs.Rows() != 2 || next.Index().MsgVecs.Dims != 1536 {
		t.Fatalf("partial rebuild mixed spaces: total=%d current=%d err=%v", total, current, err)
	}
	res, err = next.Search(ctx, req)
	md, ok := res.Trace["message_dense"].(map[string]any)
	if err != nil || !ok || md["messages_scored"].(int) != 2 {
		t.Fatalf("partial search: trace=%v err=%v", md, err)
	}
	if _, err = next.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	assertMessagePending(t, next, 0)
	total, current, err = next.Store.MessageVectorCounts(next.MessageSpace())
	if err != nil || total != seededEligible || current != seededEligible || next.Index().MsgVecs.Dims != 1536 {
		t.Fatalf("observation rows were not replaced with full-width vectors: total=%d current=%d err=%v", total, current, err)
	}
}

func TestMessageVectorNonMRLActualLeafWidthChanges(t *testing.T) {
	e, width, _ := messageWidthEngine(t, 1536)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	for _, dims := range []int{3072, 768} {
		width.Store(int32(dims))
		// Simulate the installation's leaf rebuild after a provider-width change. The leaf space
		// itself is unchanged; its actual metadata must override the still-loaded old snapshot.
		v := make([]float32, dims)
		for i := range v {
			v[i] = 1
		}
		search.Normalize(v)
		if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE embed_space=?`, search.EncodeFloat16(v), e.EmbedSpace()); err != nil {
			t.Fatal(err)
		}
		if e.MessageSpace() != "gemini-embedding-2:"+itoa(dims)+":l2" {
			t.Fatal("actual width did not change the message space")
		}
		assertMessagePending(t, e, seededEligible)
		if _, err := e.Reload(); err != nil || e.Index().HasMessageVectors() {
			t.Fatalf("old rows must be excluded: %v", err)
		}
		if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
			t.Fatal(err)
		}
		assertMessagePending(t, e, 0)
		if e.Index().MsgVecs.Dims != dims {
			t.Fatal("message width did not follow leaves")
		}
	}
}

func TestMessageVectorNonMRLExplicitLeafWidthsAreSearchable(t *testing.T) {
	for _, dims := range []int{256, 1024, 4096} {
		t.Run(itoa(dims), func(t *testing.T) {
			e, _, _ := messageWidthEngine(t, 6144)
			e.Models.EmbedDims = dims
			want := dims
			if dims == 4096 {
				// Application preserves the old 4096 -> 0 full-width leaf-space mapping.
				e.Models.EmbedDims, want = 0, 6144
			}
			seed(t, e)
			if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
				t.Fatal(err)
			}
			if e.Index().LeafVecs.Dims != want || e.Index().MsgVecs.Dims != want {
				t.Fatal("question and messages must follow the configured leaf width")
			}
			res, err := e.Search(context.Background(), SearchRequest{Query: "synthetic query", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid, Intent: search.Intent{Kind: "other", Source: "none"}})
			md, ok := res.Trace["message_dense"].(map[string]any)
			if err != nil || !ok || md["messages_scored"].(int) == 0 {
				t.Fatalf("message candidates missing: err=%v", err)
			}
		})
	}
}

func TestMessageVectorNonMRLWaitsForLeafWidth(t *testing.T) {
	e, _, calls := messageWidthEngine(t, 1536)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{DryRun: true}, nil); err != nil {
		t.Fatal(err)
	}
	if e.MessageSpace() != "" || e.MessageVectorDims() != 0 {
		t.Fatal("unknown width must not get a numeric-zero space")
	}
	items, err := e.Store.PendingMessages(nil, "", 0, e.Settings.Recipe.Version())
	if err != nil || len(items) == 0 {
		t.Fatal("closed leaves without vectors can have pending messages")
	}
	done, stop := e.embedMessages(context.Background(), items, "", &budget{}, func(string, int, int) {})
	if done != 0 || stop != "" || calls.Load() != 0 {
		t.Fatal("messages must wait without embedding")
	}
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if e.Index().MsgVecs.Dims != 1536 {
		t.Fatal("the first leaf pass must establish a real message space")
	}
}

func TestMessageVectorMixedLeafWidthsWaitAndConverge(t *testing.T) {
	for _, configured := range []int{0, 1024} {
		t.Run(itoa(configured), func(t *testing.T) {
			e, width, calls := messageWidthEngine(t, 1024)
			e.Models.EmbedDims = configured
			seed(t, e)
			ctx := context.Background()
			if _, err := e.IndexRooms(ctx, IndexOptions{}, nil); err != nil {
				t.Fatal(err)
			}
			original := e.Index().LeafVecs.Dims
			// Old cached vectors must not undo width repair (including caches keyed by the leaf space).
			cached := map[string][]byte{}
			for _, leaf := range e.Index().Leaves {
				cached[store.CacheKey(e.EmbedSpace(), "document", leaf.Body)] = search.EncodeFloat16(make([]float32, 1024))
			}
			if err := e.Store.PutCachedVectors(cached); err != nil {
				t.Fatal(err)
			}
			// The lexically later room ends earlier: the old end_time resolver picked this row,
			// whereas the loader's first Start belongs to the earlier room.
			if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=?, end_time='2000-01-01' WHERE room='discord:1:99'`, search.EncodeFloat16(make([]float32, 768))); err != nil {
				t.Fatal(err)
			}
			// A fresh engine repairs only the minority width until a leaf result establishes the target.
			next := New(e.Store, e.LLM, e.Models, e.Settings)
			t.Cleanup(next.Close)
			before := calls.Load()
			if next.MessageSpace() != "" || next.MessageVectorDims() != 0 {
				t.Fatal("mixed widths must leave the message space unresolved")
			}
			if info, err := next.Reload(); err != nil || info["message_vectors"] != 0 || next.Index().LeafVecs.Dims != original {
				t.Fatalf("leaf loader must retain its first-Start width while messages wait: %v %v", info, err)
			}
			items, err := next.Store.PendingMessages(nil, "", 0, next.Settings.Recipe.Version())
			if err != nil {
				t.Fatal(err)
			}
			if n, stop := next.embedMessages(ctx, items, "", &budget{}, func(string, int, int) {}); n != 0 || stop != "message_width_wait" || calls.Load() != before {
				t.Fatal("mixed widths must not call the message provider")
			}
			dry, err := next.IndexRooms(ctx, IndexOptions{DryRun: true}, nil)
			if err != nil || dry.Pending != 1 || dry.Estimate.LeafEmbeds != 1 || calls.Load() != before {
				t.Fatalf("mixed leaves were not queued for fresh embedding: %+v %v", dry, err)
			}
			width.Store(768)
			rep, err := next.IndexRooms(ctx, IndexOptions{}, nil)
			if err != nil || rep.LeafVectors != 1 || rep.FromCache != 0 || rep.MessageVectors != 0 || rep.StoppedBy != "message_width_wait" {
				t.Fatalf("mixed widths must keep the pass queued: %+v %v", rep, err)
			}
			rep, err = next.IndexRooms(ctx, IndexOptions{}, nil)
			if err != nil || rep.LeafVectors != len(e.Index().Leaves)-1 || rep.FromCache != 0 || rep.MessageVectors != seededEligible || next.Index().LeafVecs.Dims != 768 || next.Index().MsgVecs.Dims != 768 {
				t.Fatalf("mixed widths did not converge: %+v %v", rep, err)
			}
			assertMessagePending(t, next, 0)
			before = calls.Load()
			rep, err = next.IndexRooms(ctx, IndexOptions{}, nil)
			if err != nil || rep.Pending != 0 || calls.Load() != before {
				t.Fatal("a converged installation must keep its normal free pending pass")
			}
		})
	}
}

func TestMessageVectorMajorityWidthRepairSurvivesRestart(t *testing.T) {
	e, _, calls := messageWidthEngine(t, 1024)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE room='discord:1:99'`, search.EncodeFloat16(make([]float32, 768))); err != nil {
		t.Fatal(err)
	}
	next := New(e.Store, e.LLM, e.Models, e.Settings)
	t.Cleanup(next.Close)
	before := calls.Load()
	rep, err := next.IndexRooms(ctx, IndexOptions{}, nil)
	if err != nil || rep.LeafVectors != 1 || rep.MessageVectors != 0 || rep.StoppedBy != "" || calls.Load()-before != 1 {
		t.Fatalf("restart must embed only the minority leaf: %+v %v calls=%d", rep, err, calls.Load()-before)
	}
	if next.MessageVectorDims() != 1024 {
		t.Fatal("majority width did not converge")
	}
	fresh := New(e.Store, e.LLM, e.Models, e.Settings)
	t.Cleanup(fresh.Close)
	before = calls.Load()
	if rep, err = fresh.IndexRooms(ctx, IndexOptions{}, nil); err != nil || rep.Pending != 0 || calls.Load() != before {
		t.Fatalf("converged restart must not call the provider: %+v %v", rep, err)
	}
}

func TestMessageVectorDisabledStillKeepsMixedLeafRepairPending(t *testing.T) {
	e, width, _ := messageWidthEngine(t, 1024)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE room='discord:1:99'`, search.EncodeFloat16(make([]float32, 768))); err != nil {
		t.Fatal(err)
	}
	settings := e.Settings
	settings.MessageVectors = false
	next := New(e.Store, e.LLM, e.Models, settings)
	t.Cleanup(next.Close)
	width.Store(768)
	rep, err := next.IndexRooms(ctx, IndexOptions{}, nil)
	if err != nil || rep.LeafVectors != 1 || rep.StoppedBy != "leaf_width_mixed" {
		t.Fatalf("mixed leaf repair must remain queued with messages off: %+v %v", rep, err)
	}
}

func TestMessageVectorShorterThanExplicitDimsUsesStoredLeafWidth(t *testing.T) {
	e, _, calls := messageWidthEngine(t, 768)
	e.Models.EmbedDims = 1024
	seed(t, e)
	rep, err := e.IndexRooms(context.Background(), IndexOptions{}, nil)
	if err != nil || rep.StoppedBy != "" || rep.MessageVectors != seededEligible || e.MessageSpace() != "gemini-embedding-2:768:l2" || e.Index().LeafVecs.Dims != 768 || e.Index().MsgVecs.Dims != 768 {
		t.Fatalf("stored width must win over the explicit 1024: %+v %v", rep, err)
	}
	res, err := e.Search(context.Background(), SearchRequest{Query: "synthetic short-width query", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid, Intent: search.Intent{Kind: "other", Source: "none"}})
	md, ok := res.Trace["message_dense"].(map[string]any)
	if err != nil || !ok || md["messages_scored"].(int) == 0 {
		t.Fatal("shorter fitted leaf and message vectors must produce message candidates")
	}
	before := calls.Load()
	if rep, err = e.IndexRooms(context.Background(), IndexOptions{}, nil); err != nil || rep.Pending != 0 || calls.Load() != before {
		t.Fatal("shorter-than-configured successful vectors must not be embedded repeatedly")
	}
}

func TestMessageVectorShortProviderWaitsUntilLeafWidthChanges(t *testing.T) {
	e, width, calls := messageWidthEngine(t, 1024)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.DB.Exec(`DELETE FROM memory_message_vectors`); err != nil {
		t.Fatal(err)
	}
	width.Store(768)
	rep, err := e.IndexRooms(ctx, IndexOptions{}, nil)
	if err != nil || rep.StoppedBy != "message_width_wait" || rep.MessageVectors != 0 || e.Index().HasMessageVectors() {
		t.Fatalf("short response must wait and exclude old message snapshot: %+v %v", rep, err)
	}
	if reason, err := e.MessageVectorWaitReason(); err != nil || reason != "provider_width_short:768/1024" {
		t.Fatalf("missing width wait diagnostics: %q %v", reason, err)
	}
	items, err := e.Store.PendingMessages(nil, "", 0, e.Settings.Recipe.Version())
	if err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for i := 0; i < 2; i++ {
		if n, _ := e.embedMessages(ctx, items, "", &budget{}, func(string, int, int) {}); n != 0 || calls.Load() != before {
			t.Fatal("an unchanged leaf space must not repeat a short message call")
		}
	}
	// Short messages never trigger leaf work while the stored leaf width remains uniform.
	width.Store(1024)
	if rep, err = e.IndexRooms(ctx, IndexOptions{}, nil); err != nil || rep.LeafVectors != 0 || rep.MessageVectors != 0 || calls.Load() != before {
		t.Fatalf("message wait must not re-embed uniform leaves: %+v %v", rep, err)
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if _, err = e.IndexRooms(ctx, IndexOptions{}, nil); err != nil || calls.Load() != before {
			t.Fatal("failed-to-converge space must wait without repeated provider calls")
		}
	}
	// A fresh engine may retry one message batch, but must not pay for any uniform leaf again.
	width.Store(768)
	next := New(e.Store, e.LLM, e.Models, e.Settings)
	t.Cleanup(next.Close)
	before = calls.Load()
	if rep, err = next.IndexRooms(ctx, IndexOptions{}, nil); err != nil || rep.LeafVectors != 0 || rep.MessageVectors != 0 || rep.StoppedBy != "message_width_wait" || calls.Load()-before != 1 {
		t.Fatalf("restart must retry only one message batch: %+v %v calls=%d", rep, err, calls.Load()-before)
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if rep, err = next.IndexRooms(ctx, IndexOptions{}, nil); err != nil || rep.LeafVectors != 0 || rep.StoppedBy != "message_width_wait" || calls.Load() != before {
			t.Fatalf("restarted wait must remain free: %+v %v", rep, err)
		}
	}
	e = next
	// A genuinely changed stored width releases the wait and re-embeds messages once.
	width.Store(768)
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE embed_space=?`, search.EncodeFloat16(make([]float32, 768)), e.EmbedSpace()); err != nil {
		t.Fatal(err)
	}
	// Reads at a different width cannot permanently release the original wait.
	if e.MessageSpace() == "" || e.MessageVectorDims() != 768 {
		t.Fatal("read must report the changed stored width")
	}
	if _, err := e.MessageVectorWaitReason(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EstimateMessageVectors(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE embed_space=?`, search.EncodeFloat16(make([]float32, 1024)), e.EmbedSpace()); err != nil {
		t.Fatal(err)
	}
	if reason, err := e.MessageVectorWaitReason(); err != nil || reason != "provider_width_short:768/1024" {
		t.Fatalf("reads released the wait: %q %v", reason, err)
	}
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE embed_space=?`, search.EncodeFloat16(make([]float32, 768)), e.EmbedSpace()); err != nil {
		t.Fatal(err)
	}
	if rep, err = e.IndexRooms(ctx, IndexOptions{}, nil); err != nil || rep.MessageVectors != seededEligible || e.Index().MsgVecs.Dims != 768 {
		t.Fatalf("changed leaf width did not release the message wait: %+v %v", rep, err)
	}
	width.Store(1024)
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE embed_space=?`, search.EncodeFloat16(make([]float32, 1024)), e.EmbedSpace()); err != nil {
		t.Fatal(err)
	}
	if rep, err = e.IndexRooms(ctx, IndexOptions{}, nil); err != nil || rep.MessageVectors != seededEligible || e.Index().MsgVecs.Dims != 1024 {
		t.Fatalf("returning to a previously rejected width must not resurrect an old wait: %+v %v", rep, err)
	}
}

func TestMessageVectorMRLDoesNotRepairLeafWidths(t *testing.T) {
	e, _, calls := messageWidthEngine(t, 1024)
	e.Models.EmbedTruncatable = true
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE room='discord:1:99'`, search.EncodeFloat16(make([]float32, 768))); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	rep, err := e.IndexRooms(context.Background(), IndexOptions{}, nil)
	if err != nil || rep.Pending != 0 || calls.Load() != before || e.MessageVectorDims() != 512 {
		t.Fatalf("MRL pending and 512 rule changed: %+v %v", rep, err)
	}
}

func TestMessageVectorLeafMetadataErrorFailsIndexRooms(t *testing.T) {
	e, _, calls := messageWidthEngine(t, 1024)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	// Only the reader is closed: leaf sync and pending queries still use the healthy writer.
	if err := e.Store.DB.Reader().Close(); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if _, err := e.IndexRooms(context.Background(), IndexOptions{}, nil); err == nil || calls.Load() != before {
		t.Fatal("leaf metadata errors must fail the pass before any provider request")
	}
	if _, err := e.loadMessageVectors(nil, nil, nil); err == nil {
		t.Fatal("leaf metadata errors must also fail message loading")
	}
}

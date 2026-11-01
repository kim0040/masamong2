package engine

import (
	"context"
	"math"
	"testing"
	"time"

	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

// bestDot is the highest cosine of a (unit) vector with any row of the message matrix.
func bestDot(m search.Matrix, v []float32) float64 {
	best := -2.0
	for i := 0; i < m.Rows(); i++ {
		best = math.Max(best, m.Dot(i, v))
	}
	return best
}

// leavesOnly builds the leaves of the seeded rooms without message vectors, so the messages are pending.
func leavesOnly(t *testing.T, e *Engine) {
	t.Helper()
	e.Settings.MessageVectors = false
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	e.Settings.MessageVectors = true
}

// A stored vector is used only while its message reads as the text it was made from. The check reads the
// live message, so an engine that already holds the old message list drops the vector too.
func TestReloadDropsMessageVectorWithChangedText(t *testing.T) {
	e, _ := messageEngine(t)
	seed(t, e)
	if rep, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil || rep.MessageVectors != seededEligible {
		t.Fatalf("%+v %v", rep, err)
	}
	// the speaker's stored label changes without going through an edit (no invalidation ran)
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_observations SET actor_label='renamed' WHERE observation_id='discord:1:10#0'`); err != nil {
		t.Fatal(err)
	}
	fresh := New(e.Store, e.LLM, e.Models, e.Settings)
	info, err := fresh.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if info["message_vectors"] != seededEligible-1 || info["message_stale"] != 1 {
		t.Fatalf("reload %+v: the renamed message's vector must be left out", info)
	}
	if fresh.Index().MsgVecs.Rows() != seededEligible-1 {
		t.Fatalf("rows %d", fresh.Index().MsgVecs.Rows())
	}
	// the running engine, which still holds the old message list in its snapshot, drops it as well
	if info, err := e.Reload(); err != nil || info["message_vectors"] != seededEligible-1 || info["message_stale"] != 1 {
		t.Fatalf("running engine: %+v %v", info, err)
	}
}

// The same engine sees a speaker renamed behind it: a reload before the next pass leaves the outdated
// vector out, the pass embeds the message again with the new label, and the reload after it uses the new
// vector, not the old one.
func TestLabelChangeInRunningEngineIsReindexedAndReloaded(t *testing.T) {
	e, _ := messageEngine(t)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	var content, oldLabel string
	if err := e.Store.DB.DB.QueryRow(`SELECT content, actor_label FROM memory_observations WHERE observation_id='discord:1:10#0'`).Scan(&content, &oldLabel); err != nil {
		t.Fatal(err)
	}
	oldVec := e.fitMessageVec(vecFor(store.MessageText(oldLabel, content)))
	newVec := e.fitMessageVec(vecFor(store.MessageText("renamed", content)))
	if bestDot(e.Index().MsgVecs, oldVec) < 0.99 {
		t.Fatal("the old vector is not in the loaded matrix to begin with")
	}
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_observations SET actor_label='renamed' WHERE observation_id='discord:1:10#0'`); err != nil {
		t.Fatal(err)
	}
	if info, err := e.Reload(); err != nil || info["message_stale"] != 1 || bestDot(e.Index().MsgVecs, oldVec) > 0.99 {
		t.Fatalf("outdated vector kept: %+v %v", info, err)
	}
	rep, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep.MessageVectors != 1 || rep.Reload["message_stale"] != 0 || rep.Reload["message_vectors"] != seededEligible {
		t.Fatalf("%+v %v", rep, err)
	}
	m := e.Index().MsgVecs
	if m.Rows() != seededEligible || bestDot(m, newVec) < 0.99 || bestDot(m, oldVec) > 0.99 {
		t.Fatalf("the reloaded matrix does not hold the new vector (new %.3f, old %.3f)", bestDot(m, newVec), bestDot(m, oldVec))
	}
}

// A batch that has not started must not run once an earlier one has used the budget up: the check is made
// again after the worker slot is held, which is when the earlier batch's cost is known.
func TestUnstartedMessageBatchDoesNotRunAfterBudgetIsSpent(t *testing.T) {
	e, fp := messageEngine(t)
	e.Settings.EmbedBatch, e.Settings.Workers = 2, 1
	seed(t, e)
	leavesOnly(t, e)
	fp.embedCalls.Store(0)
	pend, err := e.Store.PendingMessages(nil, e.MessageSpace(), 0, e.Settings.Recipe.Version())
	if err != nil || len(pend) != seededEligible {
		t.Fatalf("%d %v", len(pend), err)
	}
	fp.embedDelay.Store(int64(200 * time.Millisecond)) // the first batch is still running when the second is queued
	b := &budget{limit: 0.0000005}                     // the first batch (0.000001) exceeds it
	done, stop := e.embedMessages(context.Background(), pend, e.MessageSpace(), b, func(string, int, int) {})
	if stop != "budget" || done != 2 || fp.embedCalls.Load() != 1 {
		t.Fatalf("done %d, stopped by %q, %d embedding calls: only the first of four batches may run", done, stop, fp.embedCalls.Load())
	}
}

func TestUnstartedMessageBatchDoesNotRunAfterCancel(t *testing.T) {
	e, fp := messageEngine(t)
	e.Settings.EmbedBatch, e.Settings.Workers = 2, 1
	seed(t, e)
	leavesOnly(t, e)
	fp.embedCalls.Store(0)
	pend, _ := e.Store.PendingMessages(nil, e.MessageSpace(), 0, e.Settings.Recipe.Version())
	fp.embedDelay.Store(int64(200 * time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, stop := e.embedMessages(ctx, pend, e.MessageSpace(), &budget{}, func(string, int, int) {})
	if stop != "cancelled" && stop != "provider_error" {
		t.Fatalf("stopped by %q", stop)
	}
	if n := fp.embedCalls.Load(); n != 1 {
		t.Fatalf("%d embedding calls after the cancel; only the one in flight may have been made", n)
	}
}

// Overlapping leaves hold some messages twice. Each message is embedded once and projected onto every
// leaf that holds it, so the search compares more (message, leaf) rows than there are vectors.
func TestOverlappingLeavesProjectOneVectorOntoEveryLeaf(t *testing.T) {
	e, _ := messageEngine(t)
	e.Settings.Recipe = leaves.Recipe{Size: 4, Overlap: 2, Split: leaves.SplitFixed, WithPrefix: true, MinSize: 2}
	seed(t, e)
	ctx := context.Background()
	rep, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep.StoppedBy != "" {
		t.Fatalf("%+v %v", rep, err)
	}
	// one vector per message of a closed leaf, however many leaves hold it (the last lines sit in an open tail)
	rows := e.Index().MsgVecs.Rows()
	if rep.MessageVectors < seededEligible-2 || rep.MessageVectors > seededEligible || rows != rep.MessageVectors {
		t.Fatalf("%d message vectors embedded, %d rows, want one per eligible message (at most %d)", rep.MessageVectors, rows, seededEligible)
	}
	res, err := e.Search(ctx, SearchRequest{Query: "캠핑 어디로 가", Rooms: []string{"discord:1:10", "discord:1:99"}, Ranking: retrieval.Hybrid,
		Intent: search.Intent{Kind: "other", Source: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	md, ok := res.Trace["message_dense"].(map[string]any)
	if !ok {
		t.Fatalf("trace %v", res.Trace["message_dense"])
	}
	if scored := md["messages_scored"].(int); scored <= rows {
		t.Fatalf("%d (message, leaf) rows compared for %d vectors: overlapping messages must count once per leaf", scored, rows)
	}
}

// The estimate says so when it cannot see the cost: text that is not in a closed leaf yet (never split,
// another recipe, the open tail) is reported as an upper bound instead of being shown as zero.
func TestEstimateReportsMessagesNotYetInLeaves(t *testing.T) {
	e, _ := newEngine(t, false)
	seed(t, e)
	est, err := e.EstimateMessageVectors(nil)
	if err != nil {
		t.Fatal(err)
	}
	if est.Messages != 0 || est.Unprepared != seededEligible {
		t.Fatalf("before any leaf exists: %+v (want 0 estimated and %d unprepared)", est, seededEligible)
	}
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	est, err = e.EstimateMessageVectors(nil)
	if err != nil || est.Messages != seededEligible || est.Unprepared != 0 {
		t.Fatalf("after the leaves are built: %+v %v", est, err)
	}
	// a recipe that changed: the stored leaves belong to the old one, so the messages are not placed again yet
	other := e.Settings.Recipe
	other.Size += 3
	e.Settings.Recipe = other
	if est, _ = e.EstimateMessageVectors(nil); est.Messages != 0 || est.Unprepared != seededEligible {
		t.Fatalf("recipe mismatch: %+v", est)
	}
}

// The vector loaded from the table is the row that was quantised when it was saved: the loaded matrix
// holds exactly the int8 row and scale that AppendQuantized gives the same vector, and a float16 row left
// by the first version of the table loads as that vector quantised.
func TestStoredMessageVectorsLoadAsTheInMemoryQuantisation(t *testing.T) {
	e, _ := messageEngine(t)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	var content, label string
	const id = "discord:1:10#0"
	if err := e.Store.DB.DB.QueryRow(`SELECT content, actor_label FROM memory_observations WHERE observation_id=?`, id).Scan(&content, &label); err != nil {
		t.Fatal(err)
	}
	vec := e.fitMessageVec(vecFor(store.MessageText(label, content)))
	want := search.NewQuantized(len(vec), 1)
	want.AppendQuantized(vec)
	wq, ws := want.RowQ(0)
	find := func(m search.Matrix) bool {
		for i := 0; i < m.Rows(); i++ {
			q, s := m.RowQ(i)
			same := s == ws && len(q) == len(wq)
			for j := 0; same && j < len(q); j++ {
				same = q[j] == wq[j]
			}
			if same {
				return true
			}
		}
		return false
	}
	if !find(e.Index().MsgVecs) {
		t.Fatal("the loaded matrix does not hold the stored row unchanged")
	}
	// the same message as the first version stored it: float16, format 0
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_message_vectors SET fmt=0, vec=? WHERE observation_id=?`, search.EncodeFloat16(vec), id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	m := e.Index().MsgVecs
	if m.Rows() != seededEligible || bestDot(m, vec) < 0.99 {
		t.Fatalf("the float16 row did not load (rows %d, best %.3f)", m.Rows(), bestDot(m, vec))
	}
	// and nothing about messages ever sits in the embedding cache
	var n int
	if err := e.Store.DB.DB.QueryRow(`SELECT COUNT(*) FROM memory_embed_cache WHERE kind='message'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d message rows in the embedding cache (%v)", n, err)
	}
	if err := e.Store.DB.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE holder_kind='cache' AND holder_id IN (SELECT cache_key FROM memory_embed_cache WHERE kind='message')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d cache evidence refs (%v)", n, err)
	}
}

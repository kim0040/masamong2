package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

func count(t *testing.T, e *Engine, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.Store.DB.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// successor is the engine a config change builds: same store and provider, new models.
func successor(e *Engine, m Models) *Engine {
	n := New(e.Store, e.LLM, m, e.Settings)
	e.Retire(n)
	return n
}

// Leaf and summary vectors live on the leaf only; the embedding cache no longer keeps a second copy.
func TestVectorsStoredOnce(t *testing.T) {
	e, _ := newEngine(t, true)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	leaves := count(t, e, `SELECT COUNT(*) FROM memory_leaves WHERE leaf_vec IS NOT NULL`)
	sums := count(t, e, `SELECT COUNT(*) FROM memory_leaves WHERE summary_vec IS NOT NULL`)
	if leaves == 0 || sums == 0 {
		t.Fatalf("leaves %d, summaries %d", leaves, sums)
	}
	// what is left in the cache is summary text (a few hundred bytes), never a vector
	if n := count(t, e, `SELECT COUNT(*) FROM memory_embed_cache WHERE kind='' AND length(vec) >= 32`); n != 0 {
		t.Fatalf("%d vectors duplicated into the cache", n)
	}
}

// A stored room zone that is not valid ("KST") must not stop the room from being indexed: it is read in
// the instance's zone and the report carries a warning.
func TestIndexKeepsIndexingARoomWithAnUnknownTimezoneAndWarns(t *testing.T) {
	e, _ := newEngine(t, true)
	seed(t, e)
	const room = "discord:1:10"
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_rooms SET naive_timezone=? WHERE room=?`, "Mars/Olympus", room); err != nil {
		t.Fatal(err)
	}
	report, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Warnings[room]; got == "" || !strings.Contains(got, "unknown room timezone") {
		t.Fatalf("unknown zone was not reported for %s: %+v", room, report)
	}
	if len(report.FailedRooms) != 0 {
		t.Fatalf("the room must still be indexed: %+v", report.FailedRooms)
	}
	indexed := false
	for _, r := range report.Rooms {
		indexed = indexed || r.Room == room
	}
	if !indexed || report.LeafVectors == 0 {
		t.Fatalf("room %s was skipped: %+v", room, report)
	}
}

// Lowering models.embed.dimensions truncates the stored vectors (Matryoshka) instead of paying to embed
// the history again, and the smaller index answers searches.
func TestLowerDimensionsReusesStoredVectors(t *testing.T) {
	e, fp := newEngine(t, true)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	m := e.Models
	m.EmbedDims = 8 // the fake model's native size is 16
	n := successor(e, m)
	calls := fp.embedCalls.Load()
	if _, err := n.Reload(); err != nil {
		t.Fatal(err)
	}
	if ix := n.Index(); ix == nil || ix.LeafVecs.Dims != 8 || ix.LeafVecs.Rows() == 0 || ix.SummaryVecs.Dims != 8 {
		t.Fatalf("index after shrink: %+v", ix)
	}
	rep, err := n.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep.Pending != 0 || fp.embedCalls.Load() != calls {
		t.Fatalf("shrinking must not re-embed: %v %+v calls %d→%d", err, rep, calls, fp.embedCalls.Load())
	}
	if c := count(t, n, `SELECT COUNT(*) FROM memory_leaves WHERE leaf_vec IS NOT NULL AND (embed_space<>? OR length(leaf_vec)<>16)`, n.EmbedSpace()); c != 0 {
		t.Fatalf("%d leaves not moved to 8 dims", c)
	}
	res, err := n.Search(ctx, SearchRequest{Query: "캠핑 어디로 가", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid,
		Intent: search.Intent{Kind: "other", Source: "none"}})
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("%v %+v", err, res)
	}
}

// Switching the embedding model keeps the old vectors in the cache (leaf and summary), so switching back
// is free, and a leaf never pairs a vector of one space with a summary vector of another.
func TestModelSwitchBackIsFree(t *testing.T) {
	e, fp := newEngine(t, true)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	orig := e.Models
	other := orig
	other.Embed = "fake-embed-2"
	b := successor(e, other)
	if _, err := b.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if c := count(t, b, `SELECT COUNT(*) FROM memory_leaves WHERE leaf_vec IS NOT NULL AND embed_space<>?`, b.EmbedSpace()); c != 0 {
		t.Fatalf("%d leaves left in the old space", c)
	}
	calls := fp.embedCalls.Load()
	a := successor(b, orig)
	rep, err := a.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep.LeafVectors != 0 || rep.FromCache == 0 || fp.embedCalls.Load() != calls {
		t.Fatalf("switching back must reuse kept vectors: %v %+v calls %d→%d", err, rep, calls, fp.embedCalls.Load())
	}
	if ix := a.Index(); ix == nil || ix.SummaryVecs.Rows() == 0 {
		t.Fatalf("summaries lost after switching back: %+v", ix)
	}
}

// A database written by an earlier version holds a second copy of each vector in the cache and may
// hold summary vectors of an earlier dimensionality; both are handled.
func TestLegacyDuplicatesAndStaleSummaries(t *testing.T) {
	e, _ := newEngine(t, true)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := e.Store.DB.DB.Query(`SELECT body, summary, leaf_vec, summary_vec FROM memory_leaves WHERE leaf_vec IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string][]byte{}
	for rows.Next() {
		var body, summary string
		var lv, sv []byte
		if err := rows.Scan(&body, &summary, &lv, &sv); err != nil {
			t.Fatal(err)
		}
		legacy[store.CacheKey(e.Models.Embed, "document", body)] = lv
		legacy[store.CacheKey(e.Models.Embed, "document", summary)] = sv
	}
	rows.Close()
	legacy[store.CacheKey(e.Models.Embed, "document", "text of a leaf that is gone")] = make([]byte, 32)
	if err := e.Store.PutCachedVectors(legacy); err != nil {
		t.Fatal(err)
	}
	before := count(t, e, `SELECT COUNT(*) FROM memory_embed_cache`)
	removed, err := e.Store.DropCachedDuplicates(SpaceModel, func() error { return nil })
	if err != nil || removed != int64(len(legacy)-1) {
		t.Fatalf("removed %d of %d duplicates: %v", removed, len(legacy)-1, err)
	}
	if after := count(t, e, `SELECT COUNT(*) FROM memory_embed_cache`); after != before-int(removed) {
		t.Fatalf("cache %d → %d", before, after)
	}

	// a summary vector of another dimensionality is left out of the index and embedded again
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET summary_vec=x'0000' WHERE summary_vec IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	if ix := e.Index(); ix.SummaryVecs.Rows() != 0 {
		t.Fatalf("mismatched summary vectors loaded: %d", ix.SummaryVecs.Rows())
	}
	rep, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep.SummaryVectors == 0 || e.Index().SummaryVecs.Rows() == 0 {
		t.Fatalf("stale summary vectors must be re-embedded: %v %+v", err, rep)
	}
}

func TestSpaceParsing(t *testing.T) {
	for space, want := range map[string]string{"qwen/qwen3-embedding-8b:4096:l2": "qwen/qwen3-embedding-8b",
		"nomic-embed-text:latest:0:l2": "nomic-embed-text:latest", "odd": "odd"} {
		if got := SpaceModel(space); got != want {
			t.Errorf("%s: %s", space, got)
		}
	}
}

// A rebuild copies the rows of unchanged leaves from the loaded index and reads only new or changed
// leaves' vectors; the result is the same index a full load builds.
func TestRebuildReusesUnchangedRows(t *testing.T) {
	e, _ := newEngine(t, true)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	full := e.Index()
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	again := e.Index()
	if e.LastReused() != again.LeafVecs.Rows() || again.LeafVecs.Rows() == 0 {
		t.Fatalf("reused %d of %d rows", e.LastReused(), again.LeafVecs.Rows())
	}
	same := func(a, b *search.Index) {
		t.Helper()
		if a.LeafVecs.Rows() != b.LeafVecs.Rows() || a.SummaryVecs.Rows() != b.SummaryVecs.Rows() {
			t.Fatalf("rows %d/%d vs %d/%d", a.LeafVecs.Rows(), a.SummaryVecs.Rows(), b.LeafVecs.Rows(), b.SummaryVecs.Rows())
		}
		for _, l := range a.Leaves {
			qa, sa, _ := a.QuantizedLeaf(l.DocID)
			qb, sb, ok := b.QuantizedLeaf(l.DocID)
			if !ok || sa != sb || string(int8Bytes(qa)) != string(int8Bytes(qb)) {
				t.Fatalf("leaf %s differs", l.DocID)
			}
			_, _, ha := a.QuantizedSummary(l.DocID)
			_, _, hb := b.QuantizedSummary(l.DocID)
			if ha != hb {
				t.Fatalf("summary of %s: %v vs %v", l.DocID, ha, hb)
			}
		}
	}
	same(full, again)

	// a changed summary vector is read again, the rest reused
	var id string
	if err := e.Store.DB.DB.QueryRow(`SELECT leaf_id FROM memory_leaves WHERE summary_vec IS NOT NULL LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SaveSummary(id, "다른 요약", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	ix := e.Index()
	if _, _, ok := ix.QuantizedSummary(id); ok {
		t.Fatal("a dropped summary vector is still in the index")
	}
	if e.LastReused() != ix.LeafVecs.Rows()-1 {
		t.Fatalf("reused %d of %d", e.LastReused(), ix.LeafVecs.Rows())
	}
	// and a fresh engine (nothing loaded) builds the same index from the database alone
	fresh := New(e.Store, e.LLM, e.Models, e.Settings)
	if _, err := fresh.Reload(); err != nil {
		t.Fatal(err)
	}
	if fresh.LastReused() != 0 {
		t.Fatalf("fresh engine reused %d", fresh.LastReused())
	}
	same(ix, fresh.Index())
}

func int8Bytes(q []int8) []byte {
	b := make([]byte, len(q))
	for i, v := range q {
		b[i] = byte(v)
	}
	return b
}

// A rebuild takes unchanged rooms' messages from the loaded index and reads only rooms with new
// messages; the messages are exactly those a full load reads.
func TestRebuildReadsOnlyChangedRooms(t *testing.T) {
	e, _ := newEngine(t, false)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	// one new message in one room, then the same message count check against a fresh load
	if _, err := e.Store.InsertObservations([]corpus.Observation{obs("discord:1:99", 500, "민수", "새 메시지", time.Now().Add(-time.Hour))}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	fresh := New(e.Store, e.LLM, e.Models, e.Settings)
	if _, err := fresh.Reload(); err != nil {
		t.Fatal(err)
	}
	a, b := e.Index().Messages, fresh.Index().Messages
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("messages %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("message %d: %+v vs %+v", i, a[i], b[i])
		}
	}
	found := false
	for _, m := range a {
		found = found || m.Content == "새 메시지"
	}
	if !found {
		t.Fatal("the new message is not in the rebuilt index")
	}
	// the unchanged room's messages come from the loaded index, not a new read
	before := e.Index().RoomMessages()["discord:1:10"]
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	after := e.Index().RoomMessages()["discord:1:10"]
	if len(before) == 0 || &before[0].Content == &after[0].Content {
		t.Fatal("unexpected aliasing between index message slices")
	}
	if before[0].Content != after[0].Content {
		t.Fatal("room messages changed without new messages")
	}
	// deleting a room drops its messages
	if _, err := e.Store.DeleteRoom("discord:1:99"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	for _, m := range e.Index().Messages {
		if m.Room == "discord:1:99" {
			t.Fatal("deleted room still in the index")
		}
	}
}

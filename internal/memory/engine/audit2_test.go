package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

// Port of A2C-01: pin the legacy room clock so the regression is independent of
// machine speed, and exercise successive edits with the same event timestamp.
func TestSameSecondEditsNeverRestoreOldEvidence(t *testing.T) {
	e, _ := newEngine(t, false)
	defer e.Close()
	at := time.Now().UTC().Truncate(time.Second)
	const room = "discord:g:c"
	if _, err := e.Capture(LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m", AuthorID: "alice", AuthorName: "Alice", Content: "old private canary", At: at.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	install := func() {
		t.Helper()
		if _, err := e.Store.DB.DB.Exec(`UPDATE memory_rooms SET updated_at=? WHERE room=?`, at.Format(time.RFC3339), room); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Store.SyncLeaves(room, e.Settings.Recipe, at, time.UTC); err != nil {
			t.Fatal(err)
		}
		var id, body string
		if err := e.Store.DB.DB.QueryRow(`SELECT leaf_id,body FROM memory_leaves WHERE room=? AND recipe=?`, room, e.Settings.Recipe.Version()).Scan(&id, &body); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.SaveLeafVector(id, e.EmbedSpace(), search.EncodeFloat16(vecFor(body)), body); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	install()
	before := e.Index().RoomSigs[room]
	for i, text := range []string{"replacement public text", "final corrected text"} {
		var ok bool
		var err error
		if i == 0 {
			ok, err = e.Edit("m", text, at)
		} else {
			// A durable write is protected even before its engine callback.
			_, ok, err = e.Store.EditMessage("discord", "m", text, at.Format(time.RFC3339))
		}
		if err != nil || !ok {
			t.Fatalf("edit: %v %v", ok, err)
		}
		stale, err := e.Search(context.Background(), SearchRequest{Query: "old private canary", Rooms: []string{room}, Ranking: retrieval.Hybrid})
		if err != nil || len(stale.Hits) != 0 {
			t.Fatalf("search before reload quoted superseded evidence: %+v %v", stale, err)
		}
		install()
		if got := e.Index().Messages[0].Content; got != text {
			t.Fatalf("reloaded evidence = %q, want %q", got, text)
		}
		if after := e.Index().RoomSigs[room]; before == after {
			t.Fatal("room identity did not advance after an edit")
		} else {
			before = after
		}
		r, err := e.Search(context.Background(), SearchRequest{Query: text, Rooms: []string{room}, Ranking: retrieval.Hybrid, RemainingUSD: 1})
		if err != nil || len(r.Hits) == 0 || !strings.Contains(r.Hits[0].Excerpt, text) || strings.Contains(r.Hits[0].Excerpt, "old private canary") {
			t.Fatalf("search after edit: %+v %v", r, err)
		}
	}
}

// Port of A2C-02: changing recipes in either direction must replace membership.
func TestRecipeChangesOnlyLoadTheSelectedLeaves(t *testing.T) {
	e, _ := newEngine(t, false)
	defer e.Close()
	seed(t, e)
	for _, recipe := range []leaves.Recipe{leaves.DefaultRecipe(), {Size: 8, Split: leaves.SplitFixed, WithPrefix: true}, leaves.DefaultRecipe()} {
		e.Settings.Recipe = recipe
		if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
			t.Fatal(err)
		}
		if len(e.Index().Leaves) == 0 {
			t.Fatal("selected recipe has no searchable leaves")
		}
		for _, l := range e.Index().Leaves {
			var got string
			if err := e.Store.DB.DB.QueryRow(`SELECT recipe FROM memory_leaves WHERE leaf_id=?`, l.DocID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != recipe.Version() {
				t.Fatalf("loaded recipe %q, want %q", got, recipe.Version())
			}
		}
	}
}

func TestSearchHoldingAnOldIndexStaysGuardedAfterReload(t *testing.T) {
	e, _ := newEngine(t, false)
	defer e.Close()
	at := time.Now().UTC().Truncate(time.Second)
	const room = "discord:g:c"
	if _, err := e.Capture(LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m", AuthorName: "Alice", Content: "old private canary", At: at.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	install := func() {
		t.Helper()
		if _, err := e.Store.SyncLeaves(room, e.Settings.Recipe, at, time.UTC); err != nil {
			t.Fatal(err)
		}
		var id, body string
		if err := e.Store.DB.DB.QueryRow(`SELECT leaf_id,body FROM memory_leaves WHERE room=? AND recipe=?`, room, e.Settings.Recipe.Version()).Scan(&id, &body); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.SaveLeafVector(id, e.EmbedSpace(), search.EncodeFloat16(vecFor(body)), body); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	install()
	old := e.Index()
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": vecFor("old private canary")}}})
	}))
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	e.LLM.BaseURL = srv.URL
	type result struct {
		res search.Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := e.Search(context.Background(), SearchRequest{Query: "old private canary", Rooms: []string{room}, Ranking: retrieval.Hybrid})
		done <- result{res, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("search did not reach the blocking provider")
	}
	if ok, err := e.Edit("m", "replacement public text", at); err != nil || !ok {
		t.Fatalf("edit: %v %v", ok, err)
	}
	install()
	// Deliver an older callback after the newer one: the guard cannot regress.
	e.revisedChanged([]store.Revision{{Room: room, RoomRevision: old.RoomRevisions[room]}})
	if got := e.dropRevised([]search.Hit{{Room: room}}, old); len(got) != 0 {
		t.Fatal("reload cleared the guard against an older snapshot")
	}
	close(release)
	select {
	case got := <-done:
		if got.err != nil || len(got.res.Hits) != 0 {
			t.Fatalf("old in-flight evidence escaped: %+v %v", got.res, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search did not finish")
	}
}

func TestRecipeChangeDoesNotInheritAnObsoleteIndex(t *testing.T) {
	e, _ := newEngine(t, false)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	s := e.Settings
	s.Recipe = leaves.Recipe{Size: 8, Split: leaves.SplitFixed, WithPrefix: true}
	next := New(e.Store, e.LLM, e.Models, s)
	next.Inherit(e)
	e.Retire(next)
	defer next.Close()
	if next.Index() != nil {
		t.Fatal("successor inherited an index with the previous recipe")
	}
	if got := next.QueuedRooms(); len(got) != 2 {
		t.Fatalf("recipe change did not queue idle rooms: %v", got)
	}
	if _, err := next.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(next.Index().Leaves) != 0 {
		t.Fatal("reload loaded the obsolete recipe before recutting")
	}
	if _, err := next.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if len(next.Index().Leaves) == 0 {
		t.Fatal("replacement recipe was not rebuilt")
	}
}

func TestNewCaptureKeepsExistingEvidenceSearchable(t *testing.T) {
	e, _ := newEngine(t, false)
	defer e.Close()
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Capture(LiveMessage{GuildID: "1", ChannelID: "10", MessageID: "new", AuthorName: "Alice", Content: "new live tail", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	find := func() {
		t.Helper()
		res, err := e.Search(context.Background(), SearchRequest{Query: "캠핑 어디로 가", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid})
		if err != nil || len(res.Hits) == 0 {
			t.Fatalf("new capture suppressed existing evidence: %+v %v", res, err)
		}
	}
	find()
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	find()
}

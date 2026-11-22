package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

// A rebuild had no stop between its start and its publish, so a long one held the owner's Close until
// it finished. StopReload now ends it at the next stage: nothing is published, a built index is
// closed, the loaded one keeps serving searches, and no in-memory database is left open.
func TestStopReloadEndsARebuildAtTheNextStage(t *testing.T) {
	e, _ := newEngine(t, true)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	base := openLexicalDBs()
	for _, stage := range []string{"start", "messages", "vectors", "read", "build", "publish"} {
		t.Run(stage, func(t *testing.T) {
			eng := New(e.Store, e.LLM, e.Models, e.Settings)
			eng.Inherit(e)
			loaded := eng.Index()
			seen := map[string]bool{}
			hook := func(h *Engine, at string) {
				if h == eng {
					seen[at] = true
					if at == stage {
						h.StopReload()
					}
				}
			}
			reloadStageHook.Store(&hook)
			defer reloadStageHook.Store(nil)
			_, err := eng.Reload()
			switch {
			case stage == "publish" && err != nil:
				t.Fatalf("a build stopped at publish: %v", err)
			case stage != "publish" && !errors.Is(err, errRetired):
				t.Fatalf("not stopped at %s: %v", stage, err)
			}
			if !seen[stage] {
				t.Fatalf("stage %s never checked: %v", stage, seen)
			}
			if eng.Index() != loaded {
				t.Fatal("a stopped rebuild published its index")
			}
			// a rebuild stopped before it began measures nothing (as before)
			if m, ok := eng.ResidencyMeasurement(); stage != "start" && (!ok || m.Outcome != ReloadDiscarded) {
				t.Fatalf("a stopped rebuild was measured as %s", m.Outcome)
			}
			res, err := eng.Search(context.Background(), SearchRequest{Query: "캠핑", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid,
				Intent: search.Intent{Kind: "other", Source: "none"}})
			if err != nil || len(res.Hits) == 0 {
				t.Fatalf("the loaded index must keep serving: %v %+v", err, res)
			}
		})
	}
	reloadStageHook.Store(nil)
	deadline := time.Now().Add(2 * time.Second)
	for openLexicalDBs() > base && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := openLexicalDBs(); got > base {
		t.Fatalf("in-memory index databases: %d before, %d after stopped rebuilds", base, got)
	}
	// the engine that was not stopped still rebuilds
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
}

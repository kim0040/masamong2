package engine

import (
	"context"
	"testing"

	"masamong/internal/memory/leaves"
)

// An operator's history cost real money to summarize and embed, and a release must never spend it again on its own.
// The recipe's label is stored with every leaf: changing it leaves the whole history waiting to be cut and embedded
// again. A change here needs the operator's decision and an entry in the release notes, not a new expected value.
func TestTheDefaultRecipeKeepsItsLabel(t *testing.T) {
	if got, want := leaves.DefaultRecipe().Version(), "leaf-v2:time_gap:8:gap30:min2"; got != want {
		t.Fatalf("the recipe label moved from %q to %q: every stored leaf would be embedded again", want, got)
	}
}

// Starting on a store that is already indexed (what an upgrade does) pays for nothing and rewrites nothing.
func TestARestartOnAnIndexedStoreReprocessesNothing(t *testing.T) {
	e, fp := newEngine(t, true)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	embeds, chats := fp.embedCalls.Load(), fp.chatCalls.Load()
	leafRows := count(t, e, `SELECT COUNT(*) FROM memory_leaves`)
	if embeds == 0 || leafRows == 0 {
		t.Fatalf("nothing was indexed to begin with: embeds=%d leaves=%d", embeds, leafRows)
	}
	n := successor(e, e.Models)
	if _, err := n.Reload(); err != nil {
		t.Fatal(err)
	}
	rep, err := n.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pending != 0 || fp.embedCalls.Load() != embeds || fp.chatCalls.Load() != chats {
		t.Fatalf("a restart spent again: %+v embeds %d→%d chats %d→%d", rep, embeds, fp.embedCalls.Load(), chats, fp.chatCalls.Load())
	}
	if got := count(t, n, `SELECT COUNT(*) FROM memory_leaves`); got != leafRows {
		t.Fatalf("a restart changed the leaves: %d→%d", leafRows, got)
	}
}

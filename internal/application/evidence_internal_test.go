package application

import (
	"testing"

	"masamong/internal/memory/search"
)

// A hit whose own messages an earlier hit already showed says where they are; a hit that shows its own
// messages says nothing.
func TestEvidenceSaysWhereAHitsOwnMessagesAre(t *testing.T) {
	hits := []search.Hit{
		{Rank: 1, LeafObservationIDs: []string{"a", "b"}, VisibleObservationIDs: []string{"x", "a", "b", "c", "d"}},
		{Rank: 2, LeafObservationIDs: []string{"p"}, VisibleObservationIDs: []string{"p", "q"}},
		{Rank: 3, LeafObservationIDs: []string{"c", "d"}, VisibleObservationIDs: []string{"e", "f"}},
	}
	b := hitBrief(hits)
	if _, ok := b[0]["leaf_shown_in"]; ok {
		t.Errorf("hit 1 shows its own messages: %v", b[0])
	}
	if _, ok := b[1]["leaf_shown_in"]; ok {
		t.Errorf("hit 2 shows its own messages: %v", b[1])
	}
	if b[2]["leaf_shown_in"] != 1 {
		t.Errorf("hit 3's messages are in #1: %v", b[2])
	}
}

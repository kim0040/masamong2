package application

import (
	"context"
	"errors"
	"testing"

	"masamong/internal/memory/blocks"
)

// A run that lost its job by the time indexing ends (a retry's run took it) writes no block status:
// the newer run's state stays, whatever the cancel watcher saw.
func TestPrepareRunThatLostItsJobLeavesTheBlockAlone(t *testing.T) {
	s, err := OpenPractice(context.Background(), t.TempDir(), "ko", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	const room = "kakao:practice"
	cand, ok := s.blockBySource(room)
	if !ok {
		t.Fatal("practice block missing")
	}
	catalog := func() blocks.Block {
		for _, b := range s.loadPreparedBlocks() {
			if b.SourceRoom == room {
				return b
			}
		}
		t.Fatal("no catalog entry")
		return blocks.Block{}
	}
	newer := func() bool {
		b := catalog()
		b.Status, b.Error = "preparing", "newer run"
		if err := s.savePreparedBlock(b); err != nil {
			t.Fatal(err)
		}
		return false
	}
	if _, _, err := s.prepareRoom(context.Background(), cand, preparePayload{SourceRoom: room, BudgetUSD: 1}, nil, newer); !errors.Is(err, errJobTaken) {
		t.Fatalf("err %v, want errJobTaken", err)
	}
	if b := catalog(); b.Error != "newer run" {
		t.Fatalf("the lost run wrote the block: %s %q", b.Status, b.Error)
	}
	// the owner records its outcome as before
	if _, _, err := s.prepareRoom(context.Background(), cand, preparePayload{SourceRoom: room, BudgetUSD: 1}, nil, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if b := catalog(); b.Status != "ready" || b.Error != "" {
		t.Fatalf("owner's block %s %q", b.Status, b.Error)
	}
}

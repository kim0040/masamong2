package application

import (
	"context"
	"testing"
	"time"
)

func TestQueuedTurnKeepsItsSlotPool(t *testing.T) {
	var gate turnGate
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, err := gate.enter(ctx, "a", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer a()
	waiter := make(chan func(), 1)
	go func() {
		release, _ := gate.enter(ctx, "a", 3)
		waiter <- release
	}()
	for {
		gate.mu.Lock()
		queued := gate.lanes["a"].users == 2
		gate.mu.Unlock()
		if queued {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("old-limit turn never queued")
		case <-time.After(time.Millisecond):
		}
	}
	b, err := gate.enter(ctx, "b", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer b()
	a()
	old := <-waiter
	if old == nil {
		t.Fatal("old-limit turn did not run in its original pool")
	}
	defer old()
	wait, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if release, err := gate.enter(wait, "c", 1); err == nil {
		release()
		t.Fatal("new-limit turn bypassed the occupied slot")
	}
}

func TestMoreDoesNotWaitForAlreadyAdmittedTurn(t *testing.T) {
	var gate turnGate
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	chat, err := gate.enter(ctx, "room", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer chat()
	more, err := gate.enter(ctx, "room", 0)
	if err != nil {
		t.Fatalf("press waited behind an unrelated running model turn: %v", err)
	}
	defer more()
	chat()
	wait, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if reply, err := gate.enter(wait, "room", 1); err == nil {
		reply()
		t.Fatal("a new reply passed the unregistered continuation")
	}
}

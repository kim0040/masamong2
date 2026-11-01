package discord

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReconciliationQueuedEditsDoNotWakeFailedDrain(t *testing.T) {
	g, _ := reconciliationTestGateway(t)
	g.reconciliationWake = make(chan struct{}, 1)
	g.reconciliationWait = nil
	g.messageEventApply = func(messageReconciliation) error { return errors.New("store unavailable") }
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", MessageIDs: []string{"first"}}); err != nil {
		t.Fatal(err)
	}
	if len(g.reconciliationWake) != 1 {
		t.Fatal("first edit deferral did not wake an idle drain")
	}
	// The retry hook pauses before the timer/select, so wake accumulation is observed without sleeps.
	waiting := make(chan struct{})
	resume := make(chan struct{})
	g.retryJitter = func(time.Duration) time.Duration { close(waiting); <-resume; return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.runReconciliationDrain(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		close(resume)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("drain failed to stop")
		}
	})
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not enter backoff")
	}
	for i := 0; i < 20; i++ {
		if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", MessageIDs: []string{fmt.Sprint(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(g.reconciliationWake) != 0 {
		t.Fatal("edits defeated the store outage backoff")
	}
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "delete", MessageIDs: []string{"urgent"}}); err != nil {
		t.Fatal(err)
	}
	if len(g.reconciliationWake) != 1 {
		t.Fatal("delete did not wake the backoff")
	}
}

func TestReconciliationRecoveryDoesNotStrandEditAfterSuccessfulPass(t *testing.T) {
	g, _ := reconciliationTestGateway(t)
	g.reconciliationWake = make(chan struct{}, 1)
	g.reconciliationBackingOff.Store(true)
	// Model an edit arriving just after a recovering pass settled the queue, before it becomes idle.
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", MessageIDs: []string{"late"}}); err != nil {
		t.Fatal(err)
	}
	if len(g.reconciliationWake) != 0 {
		t.Fatal("edit woke failure backoff")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resumed := make(chan bool, 1)
	go func() { resumed <- g.waitReconciliationIdle(ctx) }()
	select {
	case ok := <-resumed:
		if !ok {
			t.Fatal("successful drain stopped instead of handling pending work")
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-resumed
		t.Fatal("edit queued without a wake was stranded after recovery")
	}
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", MessageIDs: []string{"next"}}); err != nil {
		t.Fatal(err)
	}
	if len(g.reconciliationWake) != 1 {
		t.Fatal("idle edit stopped waking the drain")
	}
}

package application

import (
	"context"
	"testing"

	"masamong/internal/chat"
)

func TestNoteFirstSendMarksOnlyTheFirstSend(t *testing.T) {
	t.Parallel()
	NoteFirstSend(context.Background()) // a context without the marker is ignored
	ctx, mark := withFirstSend(context.Background())
	NoteFirstSend(ctx)
	if mark.at.IsZero() {
		t.Fatal("the first send was not noted")
	}
	first := mark.at
	NoteFirstSend(ctx)
	if !mark.at.Equal(first) {
		t.Fatal("a later send moved the first-send mark")
	}
	_, other := withFirstSend(context.Background())
	if !other.at.IsZero() {
		t.Fatal("a marker leaked into another context")
	}
}

func TestDeliverTimedReportsFirstSendAndDeliverEnd(t *testing.T) {
	t.Parallel()
	in := ChatInput{Deliver: func(ctx context.Context, res chat.Result) string {
		NoteFirstSend(ctx)
		return "said text"
	}}
	said, firstSend, deliverEnd := deliverTimed(context.Background(), in, chat.Result{})
	if said != "said text" {
		t.Fatalf("delivered text %q", said)
	}
	if firstSend.IsZero() {
		t.Fatal("the connector noted no first send")
	}
	if deliverEnd.Before(firstSend) {
		t.Fatal("deliverEnd precedes the first send it wraps")
	}
	// A connector that never notes a first send still ends the measurement.
	said, firstSend, deliverEnd = deliverTimed(context.Background(), ChatInput{Deliver: func(context.Context, chat.Result) string {
		return ""
	}}, chat.Result{})
	if said != "" || !firstSend.IsZero() || deliverEnd.IsZero() {
		t.Fatal("a quiet delivery must end the measurement without a first-send mark")
	}
}

func TestNoteTurnTiming(t *testing.T) {
	t.Parallel()
	res := &chat.Result{}
	noteTurnTiming(res, "queue_wait", 10) // no timing map: nothing to record into
	res = &chat.Result{Timing: map[string]int{}}
	noteTurnTiming(res, "queue_wait", -1)
	if _, ok := res.Timing["queue_wait"]; ok {
		t.Fatal("an unmeasured step was recorded")
	}
	noteTurnTiming(res, "queue_wait", 42)
	if res.Timing["queue_wait"] != 42 {
		t.Fatalf("queue_wait %v", res.Timing["queue_wait"])
	}
}

func TestAddTurnSpans(t *testing.T) {
	t.Parallel()
	body := map[string]any{}
	addTurnSpans(body, chat.Result{})
	if len(body) != 0 {
		t.Fatalf("an empty result added keys: %v", body)
	}
	addTurnSpans(body, chat.Result{Spans: []chat.Span{{Name: "planner", StartMS: 0, EndMS: 3}}, WaitedOn: "loop"})
	if got, ok := body["spans"].([]chat.Span); !ok || len(got) != 1 || got[0].Name != "planner" {
		t.Fatalf("spans %v", body["spans"])
	}
	if body["waited_on"] != "loop" {
		t.Fatalf("waited_on %v", body["waited_on"])
	}
}

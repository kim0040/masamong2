package application_test

import (
	"context"
	"testing"

	"masamong/internal/config"
)

// memory.warm_query: off keeps the message from the embedding provider until a search asks for it.
func TestWarmQueryOffEmbedsOnlyWhenThePlanSearches(t *testing.T) {
	svc, f := warmSvc(t, warmNone)
	applyDoc(t, svc, func(d *config.Document) { d.Memory.WarmQuery = "off" })
	warmTurn(t, svc, f, "미리 보내지 않을 질문", false)
	if n := f.asked.Load(); n != 0 {
		t.Fatalf("a turn with warm_query off sent the message to the embedding provider %d times before the plan", n)
	}
	searches := warmSearch
	f.plan.Store(&searches)
	warmTurn(t, svc, f, "미리 보내지 않을 질문", false)
	if n := f.asked.Load(); n != 1 {
		t.Fatalf("a plan that searches embedded the message %d times, want 1", n)
	}
}

// The switch is read from the document of each turn: turning it back on warms the next turn, no restart.
func TestWarmQueryOnAgainWarmsTheNextTurn(t *testing.T) {
	svc, f := warmSvc(t, warmNone)
	applyDoc(t, svc, func(d *config.Document) { d.Memory.WarmQuery = "off" })
	warmTurn(t, svc, f, "다시 켠 질문", false)
	if n := f.asked.Load(); n != 0 {
		t.Fatalf("embedded %d times with the switch off", n)
	}
	applyDoc(t, svc, func(d *config.Document) { d.Memory.WarmQuery = "on" })
	f.holdPlan.Store(true) // the planner waits for an embedding of the message: only one started ahead of the plan releases it
	warmTurn(t, svc, f, "다시 켠 질문", false)
	if f.asked.Load() == 0 {
		t.Fatal("the switch is on again and the message was not embedded ahead of the plan")
	}
}

// Changing the switch only changes when a turn calls the provider. It rebuilds nothing: no summary, no
// embedding of the indexed rooms, no new index pass work, whichever way it is set.
func TestWarmQueryChangeDoesNotReindexOrResummarize(t *testing.T) {
	svc, f := warmSvc(t, warmNone)
	svc.MemoryPassForTest(context.Background()) // settle: everything captured is indexed
	room := embedded(t, svc, "c")
	embeds, chats := f.embeds.Load(), f.chats.Load()
	for _, v := range []string{"off", "on", "off", ""} {
		applyDoc(t, svc, func(d *config.Document) { d.Memory.WarmQuery = v })
		svc.MemoryPassForTest(context.Background())
	}
	if got := f.embeds.Load(); got != embeds {
		t.Fatalf("changing memory.warm_query made %d embedding requests", got-embeds)
	}
	if got := f.chats.Load(); got != chats {
		t.Fatalf("changing memory.warm_query made %d model calls (summaries or others)", got-chats)
	}
	if got := embedded(t, svc, "c"); got != room {
		t.Fatalf("indexed vectors of the room changed from %d to %d", room, got)
	}
}

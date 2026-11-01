package discord

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
)

func TestLeaseAndForeignButton(t *testing.T) {
	dir := t.TempDir()
	svc, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	raw, _ := json.Marshal(map[string]any{"instance": "t"})
	if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: raw}); !res.OK {
		t.Fatalf("setup %+v", res)
	}
	withoutWorldWeather(t, svc)
	sim := New(svc, "lease-1")
	now := time.Now()
	r := sim.Handle(context.Background(), Event{Kind: "prefix", Command: "날씨"}, now, "lease-other")
	if r.Error != "sender_lease_lost" {
		t.Fatalf("%+v", r)
	}
	tok := sim.IssueButton("alice", now, 1)
	r = sim.Handle(context.Background(), Event{Kind: "button", Token: tok, Actor: "bob", Action: "weather.query"}, now, "lease-1")
	if r.Error != "stale_or_foreign_token" {
		t.Fatalf("foreign button %+v", r)
	}
	r = sim.Handle(context.Background(), Event{Kind: "prefix", Command: "날씨"}, now, "lease-1")
	if !r.Acked || !r.Result.OK {
		t.Fatalf("weather %+v", r)
	}
}

func TestDeferredMemoryHasNoSuccess(t *testing.T) {
	dir := t.TempDir()
	svc, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	raw, _ := json.Marshal(map[string]any{"instance": "t"})
	_ = svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: raw})
	sim := New(svc, "l")
	r := sim.Handle(context.Background(), Event{Kind: "slash", Action: "memory.search"}, time.Now(), "l")
	if r.Result.OK {
		t.Fatal("memory.search must not succeed")
	}
}

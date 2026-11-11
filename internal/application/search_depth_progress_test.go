package application_test

import (
	"context"
	"sync/atomic"
	"testing"

	"masamong/internal/application"
	"masamong/internal/chat"
)

// ChatInput.OnProgress is handed to the chat runtime and is optional: a turn with it and a turn without it
// (every caller but the Discord gateway) both run, lab and dry-run turns included. The runtime reports the
// stages; this only checks that the plumbing neither requires nor trips over the callback.
func TestChatTurnOnProgressIsOptional(t *testing.T) {
	var planCalls, respCalls atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &planCalls)
	respURL := chatServer(t, "응답", "stop", &respCalls)
	svc := routedSvc(t, planURL, respURL)
	var heard atomic.Int32
	for _, tc := range []struct {
		name     string
		progress func(chat.Progress)
		dry      bool
	}{
		{"nil", nil, false},
		{"set", func(chat.Progress) { heard.Add(1) }, false},
		{"nil dry run", nil, true},
		{"set dry run", func(chat.Progress) { heard.Add(1) }, true},
	} {
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "u", SpeakerID: "u1",
			DryRun: tc.dry, OnProgress: tc.progress})
		if err != nil || res.Answer != "응답" {
			t.Fatalf("%s: answer %q, err %v", tc.name, res.Answer, err)
		}
	}
	// a turn that plans no search reports no progress
	if heard.Load() != 0 {
		t.Fatalf("a turn without a search reported %d stages", heard.Load())
	}
}

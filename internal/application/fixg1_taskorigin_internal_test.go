package application

import (
	"context"
	"testing"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/lang"
)

// A task's tool call (viaTask) passes the checks a chat call does: a blocked owner's call is refused (the
// Discord gate refuses a chat call's), a paused channel refuses it, and a chat tool is allowed on its
// surface.
func TestTaskToolCallsKeepTheChatChecks(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	p, _ := lang.Get("ko")
	in := ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "777777"}
	call := func() chat.ToolResult {
		return s.turnTool(in, chat.Voice{Pack: p}, viaTask)(context.Background(), "finance.fx", map[string]any{"base": "USD", "quote": "KRW"})
	}
	if r := call(); r.Status == "PERMISSION_DENIED" || r.Status == "FEATURE_DISABLED" {
		t.Fatalf("an open channel refused the task's call: %+v", r)
	}
	if r := s.Execute(context.Background(), action.Request{Action: "access.policy", Input: []byte(`{"blocked_users":["777777"]}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := call(); r.OK || r.Status != "PERMISSION_DENIED" {
		t.Fatalf("a blocked owner's task call: %+v", r)
	}
	if r := s.Execute(context.Background(), action.Request{Action: "access.policy", Input: []byte(`{"blocked_users":[]}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := s.Execute(context.Background(), action.Request{Action: "runtime.pause", Input: []byte(`{"scope":"channel","guild_id":"g","channel_id":"c"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := call(); r.OK || r.Status != "FEATURE_DISABLED" {
		t.Fatalf("a paused channel's task call: %+v", r)
	}
}

package application_test

import (
	"testing"

	"masamong/internal/action"
)

// A command refused for who asks (a server waiting for approval, a blocked member) uses none of the
// tool's shared daily allowance: the Discord gate runs before the tool's limits are counted.
func TestRefusedCommandsSpendNoToolQuota(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	if r := exec(svc, "tools.configure", map[string]any{"id": "help.version", "per_day": 2}); !r.OK {
		t.Fatal(r.Error)
	}
	// a server waiting for approval (approval is on by default): any member's command
	pending := &action.Invoker{Via: "command", GuildID: "999000000000000001", ChannelID: "c9", UserID: "500000000000000001"}
	for i := 0; i < 3; i++ {
		if r := as(svc, "help.version", nil, pending); r.OK || r.Error.MessageKey != "access.server_pending" {
			t.Fatalf("pending-server call %d: %+v", i, r)
		}
	}
	serveAnyServer(t, svc)
	if r := exec(svc, "access.policy", map[string]any{"blocked_users": []string{"500000000000000003"}}); !r.OK {
		t.Fatalf("policy: %+v", r)
	}
	blocked := &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: "500000000000000003"}
	for i := 0; i < 3; i++ {
		if r := as(svc, "help.version", nil, blocked); r.OK || r.Error.MessageKey != "access.blocked" {
			t.Fatalf("blocked call %d: %+v", i, r)
		}
	}
	// the allowance is whole for the members it is meant for
	ok := &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: "500000000000000002"}
	for i := 0; i < 2; i++ {
		if r := as(svc, "help.version", nil, ok); !r.OK {
			t.Fatalf("legit call %d after refused ones: %+v", i, r.Error)
		}
	}
	if r := as(svc, "help.version", nil, ok); r.OK || r.Error.MessageKey != "tool.limited" {
		t.Fatalf("the third call is over the limit: %+v", r)
	}
}

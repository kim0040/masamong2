package discord

import (
	"encoding/json"
	"testing"
)

// /mymemory and /forgetme are ephemeral Discord commands, and the confirm button of /forgetme acts for
// the person pressing it with the preview's token.
func TestMemoryCommandsAndForgetButton(t *testing.T) {
	want := map[string]string{"mymemory": "memory.mine.show", "forgetme": "memory.mine.forget"}
	for _, c := range Commands() {
		if act, ok := want[c.Name]; ok {
			if c.Action != act || !c.Ephemeral || !c.DiscordOnly {
				t.Fatalf("%s: %+v", c.Name, c)
			}
			delete(want, c.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing commands: %v", want)
	}
	req, ok := featureButton([]string{"mem", "forget", "p", "abc123"}, InvokeContext{GuildID: "g", ChannelID: "c", UserID: "u9"}, "r1")
	if !ok || req.Action != "memory.mine.forget" || req.Invoker == nil || req.Invoker.UserID != "u9" || req.Invoker.GuildID != "g" || req.Invoker.Via != "command" {
		t.Fatalf("button: %+v", req)
	}
	var in map[string]any
	_ = json.Unmarshal(req.Input, &in)
	if in["scope"] != "pending" || in["confirm"] != "abc123" {
		t.Fatalf("button input: %v", in)
	}
	for _, bad := range [][]string{{"mem", "forget", "x", "abc"}, {"mem", "forget", "a", ""}, {"mem", "show", "a", "abc"}, {"mem", "forget"}} {
		if _, ok := featureButton(bad, InvokeContext{GuildID: "g", UserID: "u9"}, "r"); ok {
			t.Fatalf("%v must not parse", bad)
		}
	}
}

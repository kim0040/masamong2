package config

import (
	"strings"
	"testing"
)

const minimalDoc = "schema_version: 1\ninstance:\n  id: x\n"

// Memory is on by default: a file that never mentions it gets every switch on, for new installs
// (Default) and for configs that omit the keys (Parse and ParseCommitted alike).
func TestMemoryDefaultsOn(t *testing.T) {
	d := Default("x", "/tmp/x")
	if !d.Memory.Capture || !d.Memory.Embed || !d.Memory.Summaries || d.Memory.Retrieve != "auto" {
		t.Fatalf("Default memory: %+v", d.Memory)
	}
	for _, parse := range []func([]byte) (Document, error){
		Parse,
		func(raw []byte) (Document, error) { d, _, err := ParseCommitted(raw); return d, err },
	} {
		got, err := parse([]byte(minimalDoc))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Memory.Capture || !got.Memory.Embed || !got.Memory.Summaries {
			t.Fatalf("omitted memory switches must default to on: %+v", got.Memory)
		}
		if got.Memory.Retrieve != "auto" {
			t.Fatalf("a missing memory.retrieve must read as auto, got %q", got.Memory.Retrieve)
		}
	}
}

// A partial memory section defaults only the keys the file leaves out.
func TestMemoryPartialDefaults(t *testing.T) {
	got, err := Parse([]byte(minimalDoc + "memory:\n  capture: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory.Capture || !got.Memory.Embed || !got.Memory.Summaries || got.Memory.Retrieve != "auto" {
		t.Fatalf("memory: %+v", got.Memory)
	}
}

// Turning memory off stays an explicit setting: an off written in the file is kept, and it keeps
// coming back off after a marshal/parse round trip (a written `capture: false` is not dropped).
func TestMemoryExplicitOffKept(t *testing.T) {
	got, err := Parse([]byte(minimalDoc + "memory:\n  capture: false\n  embed: false\n  summaries: false\n  retrieve: off\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory.Capture || got.Memory.Embed || got.Memory.Summaries || got.Memory.Retrieve != "off" {
		t.Fatalf("explicit off must be kept: %+v", got.Memory)
	}
	b, err := Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"capture: false", "embed: false", "summaries: false"} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("an explicit off was not written: %q missing in\n%s", key, b)
		}
	}
	again, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if again.Memory.Capture || again.Memory.Embed || again.Memory.Summaries {
		t.Fatalf("explicit off did not survive a round trip: %+v", again.Memory)
	}
}

// A channel the file never configures captures and searches memory; a channel that says off stays
// off. The check runs through Parse so the written file is what decides.
func TestChannelMemoryDefaultsOn(t *testing.T) {
	raw := minimalDoc + `guilds:
  g1:
    channels:
      c1: {respond: always}
      c2: {respond: always, capture: false}
      c3: {respond: always, memory_use: off}
`
	got, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	ch := got.Guilds["g1"].Channels
	if !ch["c1"].Capture || ch["c1"].MemoryUse != "on" {
		t.Fatalf("an unconfigured channel must capture and use memory: %+v", ch["c1"])
	}
	if ch["c2"].Capture {
		t.Fatal("an explicit capture: false must be kept")
	}
	if ch["c3"].MemoryUse != "off" {
		t.Fatalf("an explicit memory_use: off must be kept, got %q", ch["c3"].MemoryUse)
	}
	// a policy assembled in code (no file behind it) is an unconfigured channel, not an off
	if p := NormalizeChannelPolicy(ChannelPolicy{}); !p.Capture || p.MemoryUse != "on" {
		t.Fatalf("a code-built policy must default to capturing: %+v", p)
	}
}

// A written reply_only is read as off from a committed document and rejected from a candidate;
// reply_only is no longer a choice the console offers.
func TestReplyOnlyReadsAsOff(t *testing.T) {
	raw := minimalDoc + `guilds:
  g1:
    channels:
      c1: {respond: always, conversation: {follow: reply_only}}
`
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "conversation.follow") {
		t.Fatalf("a candidate with reply_only must be rejected naming the key: %v", err)
	}
	got, problems, err := ParseCommitted([]byte(raw))
	if err != nil {
		t.Fatalf("a committed document with reply_only must load: %v", err)
	}
	if f := got.Guilds["g1"].Channels["c1"].Conversation.Follow; f != "off" {
		t.Fatalf("a stored reply_only must read as off, got %q", f)
	}
	if len(problems) == 0 || !strings.Contains(problems[0].Path, "conversation.follow") {
		t.Fatalf("the read-as-off must be reported: %v", problems)
	}
	for _, m := range FollowModes {
		if m == "reply_only" {
			t.Fatal("reply_only is still a choice")
		}
	}
}

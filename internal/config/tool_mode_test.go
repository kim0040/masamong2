package config

import (
	"strings"
	"testing"
)

func TestChatToolModeValidation(t *testing.T) {
	for _, mode := range []string{"", "planner", "shadow", "loop"} {
		d := Default("a", "/tmp/x")
		d.Chat.ToolMode = mode
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(b)
		if err != nil || got.Chat.ToolMode != mode {
			t.Fatalf("%q: %v %q", mode, err, got.Chat.ToolMode)
		}
		if mode == "" && strings.Contains(string(b), "chat:") {
			t.Fatalf("an unset tool mode writes a chat section:\n%s", b)
		}
	}
	if (ChatSettings{}).Mode() != "loop" {
		t.Fatal("the default tool mode is not loop")
	}
	d := Default("a", "/tmp/x")
	d.Chat.ToolMode = "auto"
	b, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b); err == nil {
		t.Fatal("chat.tool_mode auto accepted")
	}
}

func TestChatToolModeSchema(t *testing.T) {
	var node *SchemaNode
	for _, f := range Schema().Fields {
		if f.Key != "chat" {
			continue
		}
		for _, g := range f.Node.Fields {
			if g.Key == "tool_mode" {
				node = g.Node
			}
		}
	}
	if node == nil || len(node.Enum) != 3 || node.Implied != "loop" {
		t.Fatalf("chat.tool_mode schema: %+v", node)
	}
}

func TestChatToolModel(t *testing.T) {
	if (ChatSettings{}).LoopRole() != "planner" || (ChatSettings{ToolModel: "response"}).LoopRole() != "response" {
		t.Fatal("loop role")
	}
	d := Default("a", "/tmp/x")
	d.Chat.ToolModel = "huge"
	b, _ := Marshal(d)
	if _, err := Parse(b); err == nil {
		t.Fatal("an unknown chat.tool_model is refused")
	}
}

func TestChatLoopFinal(t *testing.T) {
	if (ChatSettings{}).LoopFinalMode() != "tools" || (ChatSettings{LoopFinal: "search"}).LoopFinalMode() != "search" {
		t.Fatal("chat.loop_final default")
	}
	var node *SchemaNode
	for _, f := range Schema().Fields {
		if f.Key != "chat" {
			continue
		}
		for _, g := range f.Node.Fields {
			if g.Key == "loop_final" {
				node = g.Node
			}
		}
	}
	if node == nil || len(node.Enum) != 3 || node.Implied != "tools" {
		t.Fatalf("chat.loop_final schema: %+v", node)
	}
	d := Default("a", "/tmp/x")
	d.Chat.LoopFinal = "always"
	b, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b); err == nil {
		t.Fatal("an unknown chat.loop_final is refused")
	}
}

package config

import (
	"strings"
	"testing"
)

func TestTranscriptCharsValidation(t *testing.T) {
	for _, n := range []int{0, 1000, 5000, 20000} {
		d := Default("a", "/tmp/x")
		d.Chat.TranscriptChars = n
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(b)
		if err != nil || got.Chat.TranscriptChars != n {
			t.Fatalf("%d: %v %d", n, err, got.Chat.TranscriptChars)
		}
		if n == 0 && strings.Contains(string(b), "transcript_chars") {
			t.Fatalf("an unset budget writes a transcript_chars:\n%s", b)
		}
	}
	if (ChatSettings{}).TranscriptBudget() != 5000 {
		t.Fatal("the default transcript budget is not 5000")
	}
	for _, n := range []int{-1, 1, 999, 20001} {
		d := Default("a", "/tmp/x")
		d.Chat.TranscriptChars = n
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(b); err == nil {
			t.Fatalf("chat.transcript_chars %d accepted", n)
		} else if !strings.Contains(err.Error(), "chat.transcript_chars") {
			t.Fatalf("%d: the error must name the key: %v", n, err)
		}
	}
}

func TestTranscriptCharsSchema(t *testing.T) {
	var node *SchemaNode
	for _, f := range Schema().Fields {
		if f.Key != "chat" {
			continue
		}
		for _, g := range f.Node.Fields {
			if g.Key == "transcript_chars" {
				node = g.Node
			}
		}
	}
	if node == nil || node.Implied != 5000 || node.Min == nil || node.Max == nil || *node.Min != 1000 || *node.Max != 20000 {
		t.Fatalf("transcript_chars schema: %+v", node)
	}
}

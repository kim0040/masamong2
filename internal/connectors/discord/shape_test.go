package discord

import (
	"strings"
	"testing"
)

func TestBubbles(t *testing.T) {
	parts, ok := bubbles("ㅋㅋㅋ 그거 나도 봤어\n내일 같이 갈래?")
	if !ok || len(parts) != 2 {
		t.Fatalf("%v %v", parts, ok)
	}
	parts, _ = bubbles("a\nb\nc\nd")
	if len(parts) != 3 || parts[2] != "c\nd" {
		t.Fatalf("at most three bubbles: %q", parts)
	}
	for _, s := range []string{"한 줄이야", "- 하나\n- 둘", "1. a\n2. b", "```go\nx\n```\n끝", strings.Repeat("길게 ", 100) + "\n둘"} {
		if _, ok := bubbles(s); ok {
			t.Errorf("sent whole: %q", s)
		}
	}
}

func TestTrimReply(t *testing.T) {
	short := "짧은 답"
	if h, r := trimReply(short); h != short || r != "" {
		t.Fatal("short replies are not trimmed")
	}
	p := strings.Repeat("가", 300)
	long := p + "\n\n" + p + "\n\n" + p
	h, r := trimReply(long)
	if h != p || r != p+"\n\n"+p {
		t.Fatalf("head %d rest %d", len([]rune(h)), len([]rune(r)))
	}
	if _, r := trimReply(strings.Repeat("나", 900)); r != "" {
		t.Fatal("no paragraph to cut at: sent whole")
	}
}

func TestListy(t *testing.T) {
	for s, want := range map[string]bool{"- a": true, "12. b": true, "3) c": true, "2026년에": false, "# h": true, "그냥": false} {
		if listy(s) != want {
			t.Errorf("%q", s)
		}
	}
}

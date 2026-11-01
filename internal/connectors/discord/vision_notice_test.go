package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWithLeadPutsTheNoticeBeforeTheAnswer(t *testing.T) {
	if got := withLead("notice", "answer"); got != "notice\n\nanswer" {
		t.Fatalf("delivered text = %q", got)
	}
	if got := withLead("", "answer"); got != "answer" {
		t.Fatalf("unchanged text = %q", got)
	}
	if got := withLead("notice", ""); got != "notice" {
		t.Fatalf("notice alone = %q", got)
	}
}

// sentContents are the message texts the fake Discord saw posted, in order.
func sentContents(t *testing.T, f *fakeDiscord) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.method != http.MethodPost || !strings.HasSuffix(c.path, "/messages") {
			continue
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			t.Fatalf("message body: %v", err)
		}
		out = append(out, body.Content)
	}
	return out
}

// A long answer is trimmed behind a "more" button; the partial-picture notice must be in the message
// that is shown, exactly once, and not in the part kept for the button.
func TestVisionNoticeIsInTheFirstMessageOfATrimmedReply(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	const notice = "NOTICE-LINE 1/3"
	var paras []string
	for i := 0; i < 8; i++ {
		paras = append(paras, strings.Repeat("word ", 30)+fmt.Sprint(i))
	}
	answer := strings.Join(paras, "\n\n")
	said, ids, rep := g.sendShaped(context.Background(), g.sess, "g1", "c1", nil, answer, notice, "trim")
	if rep.Sent != rep.Parts || len(ids) == 0 {
		t.Fatalf("delivery: %+v ids %v", rep, ids)
	}
	sent := sentContents(t, fake)
	if len(sent) == 0 || !strings.HasPrefix(sent[0], notice+"\n\n") {
		t.Fatalf("the first message does not open with the notice: %q", sent)
	}
	if n := strings.Count(strings.Join(sent, "\n"), notice); n != 1 {
		t.Fatalf("notice sent %d times", n)
	}
	if !strings.HasPrefix(said, notice) || strings.Count(said, notice) != 1 {
		t.Fatalf("what is recorded as said lost the notice: %q", said)
	}
	e, ok := g.more.peek(ids[len(ids)-1])
	if !ok || e.text == "" || strings.Contains(e.text, notice) {
		t.Fatalf("the part behind the button should hold the rest of the answer only: %v %q", ok, e.text)
	}
}

// A long answer with no paragraph break is sent whole; the notice does not turn it into a trim.
func TestVisionNoticeDoesNotTrimAnUnbrokenAnswer(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	answer := strings.Repeat("word ", 200)
	_, ids, rep := g.sendShaped(context.Background(), g.sess, "g1", "c1", nil, answer, "NOTICE-LINE", "trim")
	if rep.Sent != rep.Parts || len(ids) == 0 {
		t.Fatalf("delivery: %+v", rep)
	}
	if _, ok := g.more.peek(ids[len(ids)-1]); ok {
		t.Fatal("an answer without a paragraph break was trimmed behind a button")
	}
	if sent := sentContents(t, fake); len(sent) != 1 || !strings.HasPrefix(sent[0], "NOTICE-LINE\n\nword") {
		t.Fatalf("sent = %q", sent)
	}
}

func TestVisionNoticeIsItsOwnFirstBubble(t *testing.T) {
	oldMin, oldMax := bubbleGapMin, bubbleGapMax
	bubbleGapMin, bubbleGapMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { bubbleGapMin, bubbleGapMax = oldMin, oldMax })
	g, fake := gatewayWithFakeDiscord(t)
	_, _, rep := g.sendShaped(context.Background(), g.sess, "g1", "c1", nil, "one line answer", "NOTICE-LINE", "bubbles")
	if rep.Sent != 2 {
		t.Fatalf("report %+v", rep)
	}
	if sent := sentContents(t, fake); len(sent) != 2 || sent[0] != "NOTICE-LINE" || sent[1] != "one line answer" {
		t.Fatalf("bubbles = %q", sent)
	}
}

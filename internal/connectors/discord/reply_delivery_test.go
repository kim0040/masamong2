package discord

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/errclass"
)

func fastReplyBackoff(t *testing.T) {
	t.Helper()
	old := replyBackoff
	replyBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { replyBackoff = old })
}

func restErr(status, code int, body string) error {
	return &discordgo.RESTError{
		Response:     &http.Response{StatusCode: status, Status: http.StatusText(status)},
		ResponseBody: []byte(body),
		Message:      &discordgo.APIErrorMessage{Code: code},
	}
}

func TestClassifyREST(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want errclass.Class
	}{
		{"nil", nil, errclass.None},
		{"missing permissions", restErr(403, 50013, ""), errclass.Permission},
		{"missing access", restErr(403, 50001, ""), errclass.Permission},
		{"plain 403", restErr(403, 0, ""), errclass.Permission},
		{"unknown channel", restErr(404, 10003, ""), errclass.NotFound},
		{"unknown message", restErr(404, 10008, ""), errclass.NotFound},
		{"plain 404", restErr(404, 0, ""), errclass.NotFound},
		{"invalid form body", restErr(400, 50035, `{"errors":{"content":{}}}`), errclass.BadRequest},
		{"rate limited", restErr(429, 0, ""), errclass.RateLimited},
		{"rate limit error", &discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{}}, errclass.RateLimited},
		{"server error", restErr(500, 0, ""), errclass.Overloaded},
		{"bad gateway", restErr(503, 0, ""), errclass.Overloaded},
		{"discordgo gave up on 502", errors.New("Exceeded Max retries HTTP 502 Bad Gateway, x"), errclass.Overloaded},
		{"canceled", context.Canceled, errclass.Canceled},
		{"deadline", context.DeadlineExceeded, errclass.Timeout},
		{"wrapped net error", errors.Join(errors.New("post"), &net.OpError{Op: "dial", Err: errors.New("refused")}), errclass.Network},
		{"other", errors.New("boom"), errclass.Unknown},
	}
	for _, c := range cases {
		if got := classifyREST(c.err); got != c.want {
			t.Errorf("%s: classifyREST = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestReplyTargetGone(t *testing.T) {
	gone := restErr(400, 50035, `{"code":50035,"errors":{"message_reference":{"_errors":[{"code":"REPLIES_UNKNOWN_MESSAGE"}]}}}`)
	if !replyTargetGone(gone) {
		t.Fatal("a 50035 about message_reference must count as a deleted reply target")
	}
	for _, err := range []error{
		nil, errors.New("x"), restErr(400, 50035, `{"errors":{"content":{}}}`), restErr(404, 10008, `message_reference`), restErr(403, 50013, ""),
	} {
		if replyTargetGone(err) {
			t.Errorf("replyTargetGone(%v) = true", err)
		}
	}
	if !replyTargetGone(errors.Join(errors.New("wrapped"), gone)) {
		t.Error("a wrapped error must still be recognized")
	}
}

func testGateway() *Gateway {
	return &Gateway{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

type sendCall struct {
	text, nonce string
	ref         bool
}

func TestSendReplyPartsRetriesWithSameNonce(t *testing.T) {
	fastReplyBackoff(t)
	var calls []sendCall
	n := 0
	said, ids, rep := testGateway().sendReplyParts(context.Background(), []string{"one", "two"}, &discordgo.MessageReference{MessageID: "m"},
		func(_ context.Context, msg *discordgo.MessageSend, nonce string) (*discordgo.Message, error) {
			calls = append(calls, sendCall{msg.Content, nonce, msg.Reference != nil})
			n++
			if n <= 2 { // the first part fails twice with a server error, then goes through
				return nil, restErr(500, 0, "")
			}
			return &discordgo.Message{ID: "id-" + msg.Content}, nil
		}, nil)
	if said != "onetwo" || len(ids) != 2 || rep.Parts != 2 || rep.Sent != 2 || rep.Retries != 2 || rep.Class != errclass.None {
		t.Fatalf("said %q ids %v report %+v", said, ids, rep)
	}
	if len(calls) != 4 {
		t.Fatalf("sends = %d, want 4", len(calls))
	}
	if calls[0].nonce == "" || calls[0].nonce != calls[1].nonce || calls[1].nonce != calls[2].nonce {
		t.Fatalf("retries of one part must share a nonce: %+v", calls)
	}
	if calls[3].nonce == calls[2].nonce || len(calls[3].nonce) > 25 {
		t.Fatalf("each part gets its own nonce of at most 25 characters: %+v", calls)
	}
	if !calls[0].ref || !calls[2].ref || calls[3].ref {
		t.Fatalf("only the first part replies to the message: %+v", calls)
	}
}

func TestSendReplyPartsPermanentFailureStopsAndReports(t *testing.T) {
	fastReplyBackoff(t)
	sends := 0
	var noted []errclass.Class
	said, ids, rep := testGateway().sendReplyParts(context.Background(), []string{"a", "b", "c"}, nil,
		func(_ context.Context, msg *discordgo.MessageSend, _ string) (*discordgo.Message, error) {
			sends++
			if msg.Content == "b" {
				return nil, restErr(400, 50035, `{"errors":{"content":{}}}`)
			}
			return &discordgo.Message{ID: "id-" + msg.Content}, nil
		}, func(c errclass.Class) { noted = append(noted, c) })
	if said != "a" || len(ids) != 1 || sends != 2 {
		t.Fatalf("said %q ids %v sends %d", said, ids, sends)
	}
	if rep.Parts != 3 || rep.Sent != 1 || rep.Retries != 0 || rep.Class != errclass.BadRequest || len(rep.MessageIDs) != 1 {
		t.Fatalf("report %+v", rep)
	}
	if len(noted) != 1 || noted[0] != errclass.BadRequest {
		t.Fatalf("hook calls %v", noted)
	}
}

func TestSendReplyPartsGivesUpAfterTwoRetries(t *testing.T) {
	fastReplyBackoff(t)
	sends := 0
	said, _, rep := testGateway().sendReplyParts(context.Background(), []string{"a"}, nil,
		func(context.Context, *discordgo.MessageSend, string) (*discordgo.Message, error) {
			sends++
			return nil, restErr(502, 0, "")
		}, nil)
	if sends != 3 || said != "" || rep.Retries != 2 || rep.Sent != 0 || rep.Class != errclass.Overloaded {
		t.Fatalf("sends %d said %q report %+v", sends, said, rep)
	}
}

func TestSendReplyPartsRateLimitedIsRetriedOnce(t *testing.T) {
	fastReplyBackoff(t)
	sends := 0
	_, _, rep := testGateway().sendReplyParts(context.Background(), []string{"a"}, nil,
		func(context.Context, *discordgo.MessageSend, string) (*discordgo.Message, error) {
			sends++
			return nil, restErr(429, 0, "")
		}, nil)
	if sends != 2 || rep.Retries != 1 || rep.Class != errclass.RateLimited {
		t.Fatalf("sends %d report %+v", sends, rep)
	}
}

func TestSendReplyPartsDropsReferenceWhenTargetIsGone(t *testing.T) {
	fastReplyBackoff(t)
	var calls []sendCall
	said, ids, rep := testGateway().sendReplyParts(context.Background(), []string{"x", "y"}, &discordgo.MessageReference{MessageID: "deleted"},
		func(_ context.Context, msg *discordgo.MessageSend, nonce string) (*discordgo.Message, error) {
			calls = append(calls, sendCall{msg.Content, nonce, msg.Reference != nil})
			if msg.Reference != nil {
				return nil, restErr(400, 50035, `{"errors":{"message_reference":{"_errors":[{"code":"REPLIES_UNKNOWN_MESSAGE"}]}}}`)
			}
			return &discordgo.Message{ID: "id-" + msg.Content}, nil
		}, nil)
	if said != "xy" || len(ids) != 2 || rep.Sent != 2 || !rep.ReferenceDropped || rep.Retries != 1 || rep.Class != errclass.None {
		t.Fatalf("said %q ids %v report %+v", said, ids, rep)
	}
	if len(calls) != 3 || !calls[0].ref || calls[1].ref || calls[2].ref {
		t.Fatalf("calls %+v", calls)
	}
}

func TestSendReplyPartsPermissionIsNotRetriedAndCallsHook(t *testing.T) {
	fastReplyBackoff(t)
	sends := 0
	var noted []errclass.Class
	_, _, rep := testGateway().sendReplyParts(context.Background(), []string{"a", "b"}, nil,
		func(context.Context, *discordgo.MessageSend, string) (*discordgo.Message, error) {
			sends++
			return nil, restErr(403, 50013, `{"message":"Missing Permissions","code":50013}`)
		}, func(c errclass.Class) { noted = append(noted, c) })
	if sends != 1 || rep.Retries != 0 || rep.Sent != 0 || rep.Class != errclass.Permission {
		t.Fatalf("sends %d report %+v", sends, rep)
	}
	if len(noted) != 1 || noted[0] != errclass.Permission {
		t.Fatalf("hook calls %v", noted)
	}
}

func TestSendReplyPartsCancelDuringBackoff(t *testing.T) {
	old := replyBackoff
	replyBackoff = []time.Duration{time.Minute, time.Minute}
	defer func() { replyBackoff = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sends := 0
	var noted []errclass.Class
	done := make(chan struct{})
	var rep struct{ class errclass.Class }
	go func() {
		defer close(done)
		_, _, r := testGateway().sendReplyParts(ctx, []string{"a", "b"}, nil,
			func(context.Context, *discordgo.MessageSend, string) (*discordgo.Message, error) {
				sends++
				go func() { time.Sleep(20 * time.Millisecond); cancel() }()
				return nil, restErr(500, 0, "")
			}, func(c errclass.Class) { noted = append(noted, c) })
		rep.class = r.Class
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled context did not end the backoff wait")
	}
	if sends != 1 || rep.class != errclass.Canceled || len(noted) != 0 {
		t.Fatalf("sends %d class %q hook %v", sends, rep.class, noted)
	}
}

func TestSendReplyPartsStopsAtCanceledContextBetweenParts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, ids, rep := testGateway().sendReplyParts(ctx, []string{"a", "b"}, nil,
		func(context.Context, *discordgo.MessageSend, string) (*discordgo.Message, error) {
			cancel()
			return &discordgo.Message{ID: "1"}, nil
		}, nil)
	if len(ids) != 1 || rep.Sent != 1 || rep.Parts != 2 || rep.Class != errclass.Canceled {
		t.Fatalf("ids %v report %+v", ids, rep)
	}
}

// --- splitMessage ---

func checkParts(t *testing.T, name string, parts []string, limit int) {
	t.Helper()
	for i, p := range parts {
		if n := utf8.RuneCountInString(p); n > limit {
			t.Errorf("%s: part %d has %d runes (limit %d)", name, i, n, limit)
		}
		if strings.TrimSpace(p) == "" {
			t.Errorf("%s: part %d is empty", name, i)
		}
		if !utf8.ValidString(p) {
			t.Errorf("%s: part %d is not valid UTF-8", name, i)
		}
		if strings.Count(p, "```")%2 != 0 {
			t.Errorf("%s: part %d leaves a fence open:\n%s", name, i, p)
		}
	}
}

func TestSplitMessageTable(t *testing.T) {
	const limit = 100
	code := func(lang string, lines int) string {
		var b strings.Builder
		b.WriteString("Here is the code:\n\n```" + lang + "\n")
		for i := 0; i < lines; i++ {
			b.WriteString("line := compute(" + strings.Repeat("x", 20) + ")\n")
		}
		b.WriteString("```\nDone.")
		return b.String()
	}
	cases := []struct {
		name  string
		text  string
		parts func(t *testing.T, parts []string)
	}{
		{"plain long text", strings.Repeat("The quick brown fox jumps. ", 40), func(t *testing.T, p []string) {
			if len(p) < 10 {
				t.Errorf("parts = %d", len(p))
			}
			for _, part := range p[:len(p)-1] {
				if !strings.HasSuffix(part, ".") {
					t.Errorf("part does not end at a sentence: %q", part)
				}
			}
		}},
		{"paragraphs cut at blank lines", strings.Repeat(strings.Repeat("word ", 12)+"\n\n", 6), func(t *testing.T, p []string) {
			for _, part := range p[:len(p)-1] {
				if strings.HasPrefix(part, "\n") || strings.HasSuffix(part, "\n") {
					t.Errorf("part keeps the cut's line breaks: %q", part)
				}
			}
		}},
		{"code block over three parts", code("", 12), func(t *testing.T, p []string) {
			if len(p) < 3 {
				t.Errorf("parts = %d", len(p))
			}
			if !strings.HasSuffix(p[0], "\n```") && strings.Count(p[0], "```") == 2 {
				t.Errorf("first part does not close or contain its block: %q", p[0])
			}
			for _, mid := range p[1 : len(p)-1] {
				if !strings.HasPrefix(mid, "```\n") || !strings.HasSuffix(mid, "\n```") {
					t.Errorf("middle part is not a block of its own: %q", mid)
				}
			}
		}},
		{"code block with language", code("go", 12), func(t *testing.T, p []string) {
			for _, part := range p[1 : len(p)-1] {
				if !strings.HasPrefix(part, "```go\n") {
					t.Errorf("part does not reopen with the language: %q", part)
				}
			}
			if !strings.HasPrefix(p[len(p)-1], "```go\n") {
				t.Errorf("last part does not reopen the block: %q", p[len(p)-1])
			}
		}},
		{"CJK without spaces", strings.Repeat("가나다라마바사아자차카타파하", 30), func(t *testing.T, p []string) {
			if got := strings.Join(p, ""); got != strings.Repeat("가나다라마바사아자차카타파하", 30) {
				t.Errorf("text changed by the split")
			}
		}},
		{"CJK sentences", strings.Repeat("今日は天気がいいですね。散歩に行きましょう！", 12), func(t *testing.T, p []string) {
			for _, part := range p[:len(p)-1] {
				if !strings.HasSuffix(part, "。") && !strings.HasSuffix(part, "！") {
					t.Errorf("part does not end at a sentence mark: %q", part)
				}
			}
		}},
		{"emoji", strings.Repeat("😀", 250), func(t *testing.T, p []string) {
			if got := strings.Join(p, ""); got != strings.Repeat("😀", 250) {
				t.Errorf("text changed by the split")
			}
		}},
		{"emoji sequences are not torn", strings.Repeat("a👨‍👩‍👧b", 60), func(t *testing.T, p []string) {
			for _, part := range p {
				r := []rune(part)
				if r[0] == 0x200D || r[len(r)-1] == 0x200D || (r[0] >= 0xFE00 && r[0] <= 0xFE0F) {
					t.Errorf("part begins or ends inside a sequence: %q", part)
				}
			}
		}},
		{"exactly limit", strings.Repeat("a", limit), func(t *testing.T, p []string) {
			if len(p) != 1 {
				t.Errorf("parts = %d", len(p))
			}
		}},
		{"one over limit", strings.Repeat("a", limit+1), func(t *testing.T, p []string) {
			if len(p) != 2 {
				t.Errorf("parts = %d", len(p))
			}
		}},
		{"exactly two limits", strings.Repeat("ab", limit), func(t *testing.T, p []string) {
			if len(p) != 2 || len(p[0]) != limit || len(p[1]) != limit {
				t.Errorf("parts = %d", len(p))
			}
		}},
		{"short", "hello", func(t *testing.T, p []string) {
			if len(p) != 1 || p[0] != "hello" {
				t.Errorf("parts = %q", p)
			}
		}},
		{"fence marker is not torn", strings.Repeat("x", limit-2) + "\n```go\nfoo()\n```\nafter", nil},
		{"many newlines", strings.Repeat("\n", 300) + "end", nil},
	}
	for _, c := range cases {
		parts := splitMessage(c.text, limit)
		checkParts(t, c.name, parts, limit)
		if c.parts != nil {
			c.parts(t, parts)
		}
	}
}

func TestSplitMessageCodeBlockKeepsTextAndLanguage(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 40; i++ {
		body.WriteString("fmt.Println(\"line number ")
		body.WriteString(strings.Repeat("7", i%5+1))
		body.WriteString("\")\n")
	}
	text := "```go\n" + strings.TrimRight(body.String(), "\n") + "\n```"
	parts := splitMessage(text, 120)
	checkParts(t, "block", parts, 120)
	if len(parts) < 3 {
		t.Fatalf("parts = %d", len(parts))
	}
	var rebuilt strings.Builder
	for _, p := range parts {
		if !strings.HasPrefix(p, "```go\n") || !strings.HasSuffix(p, "\n```") {
			t.Fatalf("part is not a complete go block: %q", p)
		}
		rebuilt.WriteString(strings.TrimSuffix(strings.TrimPrefix(p, "```go\n"), "\n```"))
		rebuilt.WriteString("\n")
	}
	// every code line survives, in order
	want := strings.Fields(strings.NewReplacer("```go\n", "", "\n```", "").Replace(text))
	if got := strings.Fields(rebuilt.String()); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("code changed by the split:\n got %q\nwant %q", got, want)
	}
}

func TestSplitMessageEmptyInputGivesOneEmptyPart(t *testing.T) {
	if p := splitMessage("", 100); len(p) != 1 || p[0] != "" {
		t.Fatalf("parts = %q", p)
	}
}

// A prose line that starts with several tildes (a trailing-wave tone, "~~~~~ nice") is not a code fence on
// Discord (it only renders ``` blocks), so a cut in or after that line must neither repeat the line nor
// add synthetic fence lines.
func TestSplitMessageTildeProseAtBoundaryIsNotAFence(t *testing.T) {
	const limit = 60
	texts := map[string]string{
		"line after the cut":   strings.Repeat("a", limit-8) + "\n~~~~~ nice nice nice\nthe rest of the reply goes on for a while here.",
		"line ends first part": "short line\n~~~~~ nice nice\n" + strings.Repeat("the rest of the reply goes on. ", 5),
	}
	for name, text := range texts {
		parts := splitMessage(text, limit)
		checkParts(t, name, parts, limit)
		if len(parts) < 2 {
			t.Fatalf("%s: parts = %d, want a split", name, len(parts))
		}
		if n := strings.Count(strings.Join(parts, "\n"), "~~~~~"); n != 1 {
			t.Errorf("%s: the tilde line appears %d times: %q", name, n, parts)
		}
		for _, p := range parts {
			for _, line := range strings.Split(p, "\n") {
				if l := strings.TrimSpace(line); l == "~~~~~" || l == "```" {
					t.Errorf("%s: synthetic fence line in part %q", name, p)
				}
			}
		}
	}
}

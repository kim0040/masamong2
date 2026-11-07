package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
)

// The promise these tests pin (postmortem 0006). An edit is not a deletion: the message is still there,
// with other words. A reply made from the new words is an ordinary reply and stays; a reply made from the
// old words goes, with everything built on it, whenever it reaches the store. A deletion or a withdrawal
// has no new words, so nothing about what they block changes.

// promptServer answers every model call with a fixed reply, remembers the prompts, and runs during the next
// call whatever the test leaves in during (the thing that happens while a turn is under way).
type promptServer struct {
	mu      sync.Mutex
	prompts []string
	during  func()
	URL     string
}

func newPromptServer(t *testing.T) *promptServer {
	t.Helper()
	p := &promptServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.prompts = append(p.prompts, string(body))
		during := p.during
		p.during = nil
		p.mu.Unlock()
		if during != nil {
			during()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"memory_requirement":"none","tool_plan":[]}`}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
		})
	}))
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

func (p *promptServer) whileRunning(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.during = fn
}

// sawSince reports whether a prompt made after the first skip calls holds the text.
func (p *promptServer) sawSince(skip int, text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, prompt := range p.prompts[skip:] {
		if strings.Contains(prompt, text) {
			return true
		}
	}
	return false
}

func (p *promptServer) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.prompts)
}

func editSvc(t *testing.T) (*application.Service, *promptServer) {
	t.Helper()
	p := newPromptServer(t)
	return routedSvc(t, p.URL, p.URL), p
}

func changeSource(t *testing.T, svc *application.Service, change, id, author string) {
	t.Helper()
	var err error
	switch change {
	case "edit":
		err = svc.MessageEdited("g", "c", id, "corrected words", time.Now())
	case "delete":
		err = svc.MessagesDeleted("g", "c", []string{id})
	case "blank":
		err = svc.MessageEdited("g", "c", id, "   ", time.Now()) // an edit to nothing is a deletion
	case "withdraw":
		if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: author, Name: author}); !r.OK {
			t.Fatal(r.Error)
		}
	default:
		t.Fatalf("unknown change %q", change)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The bug: for the next 16 turns every reply read the corrected message, every reply was refused, and the
// bot forgot what it had just said.
func TestRepliesOnTheEditedWordsAreKept(t *testing.T) {
	svc, prompts := editSvc(t)
	source := chainTurn(t, svc, "c", "secret", "alice")
	built := chainTurn(t, svc, "c", "n0", "bob")
	changeSource(t, svc, "edit", "secret", "alice")
	if n := assistantTurns(t, svc, "c"); n != 0 {
		t.Fatalf("%d replies built on the old words are still in the chat context", n)
	}
	for _, turn := range []string{source, built} {
		if answerKept(t, svc, turn) {
			t.Fatal("the turn history keeps a reply built on the old words")
		}
	}
	calls := prompts.calls()
	for i := range 3 {
		turn := chainTurn(t, svc, "c", fmt.Sprintf("after%d", i), "bob")
		if n := assistantTurns(t, svc, "c"); n != i+1 {
			t.Fatalf("after %d turns on the corrected message: %d replies kept", i+1, n)
		}
		if !answerKept(t, svc, turn) {
			t.Fatalf("turn %d after the edit lost its answer", i)
		}
	}
	if !prompts.sawSince(calls, "corrected words") {
		t.Fatal("the turns after the edit did not read the corrected message: this proves nothing")
	}
	if prompts.sawSince(calls, "message secret from alice") {
		t.Fatal("a turn after the edit still read the old words")
	}
}

// A reply on the edited words is built on that message like any other: the message's next change takes it.
func TestRepliesOnTheEditedWordsFollowTheNextChange(t *testing.T) {
	for _, change := range []string{"edit", "delete", "blank", "withdraw"} {
		t.Run(change, func(t *testing.T) {
			svc, _ := editSvc(t)
			chainTurn(t, svc, "c", "secret", "alice")
			changeSource(t, svc, "edit", "secret", "alice")
			first := chainTurn(t, svc, "c", "after0", "bob")
			second := chainTurn(t, svc, "c", "after1", "bob")
			if n := assistantTurns(t, svc, "c"); n != 2 || !answerKept(t, svc, first) || !answerKept(t, svc, second) {
				t.Fatalf("replies on the edited words: %d kept", n)
			}
			changeSource(t, svc, change, "secret", "alice")
			if n := assistantTurns(t, svc, "c"); n != 0 {
				t.Fatalf("%d replies on the edited words survived the next change", n)
			}
			if answerKept(t, svc, first) || answerKept(t, svc, second) {
				t.Fatal("the turn history keeps a reply on the edited words")
			}
		})
	}
}

// The race: the turn read the old words, the change finished while the model was thinking. Whatever came
// first, the reply is refused. The turn after it has read what is there now.
func TestAChangeDuringATurnRefusesItsReply(t *testing.T) {
	for _, change := range []string{"edit", "delete", "blank", "withdraw"} {
		t.Run(change, func(t *testing.T) {
			svc, prompts := editSvc(t)
			chainTurn(t, svc, "c", "secret", "alice")
			before := prompts.calls()
			prompts.whileRunning(func() { changeSource(t, svc, change, "secret", "alice") })
			inFlight := chainTurn(t, svc, "c", "during", "bob")
			if !prompts.sawSince(before, "message secret from alice") {
				t.Fatal("the turn did not read the message before it changed: this proves nothing")
			}
			if n := assistantTurns(t, svc, "c"); n != 0 {
				t.Fatalf("%d replies built on what the turn read before the change were kept", n)
			}
			if answerKept(t, svc, inFlight) {
				t.Fatal("the turn history keeps the reply of the turn under way")
			}
			if change != "edit" {
				return
			}
			next := chainTurn(t, svc, "c", "after", "bob")
			if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, next) {
				t.Fatalf("the turn after the edit was refused: %d replies kept", n)
			}
		})
	}
}

// The intent is committed but the worker has not applied it: a turn starting now still reads the old words,
// though the tombstone is older than the turn. Comparing the two times would keep this reply.
func TestATurnStartedBeforeAnEditIsAppliedIsRefused(t *testing.T) {
	svc, prompts := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	svc.HoldRedactionForTest(true)
	if err := svc.QueueEditForTest("g", "c", "secret", "corrected words", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := prompts.calls()
	prompts.whileRunning(func() {
		svc.HoldRedactionForTest(false)
		if err := svc.DrainRedactions(context.Background()); err != nil {
			t.Error(err)
		}
	})
	inFlight := chainTurn(t, svc, "c", "during", "bob")
	if !prompts.sawSince(before, "message secret from alice") {
		t.Fatal("the turn did not read the old words: this proves nothing")
	}
	if n := assistantTurns(t, svc, "c"); n != 0 {
		t.Fatalf("%d replies on the old words were kept", n)
	}
	if answerKept(t, svc, inFlight) {
		t.Fatal("the turn history keeps a reply on the old words")
	}
}

// A clock that disagrees proves nothing either way: Discord's edit time may be ahead of this computer or
// behind it, and the replies on the new words stay all the same.
func TestAnEditIsJudgedWithoutAskingAClock(t *testing.T) {
	for _, skew := range []time.Duration{-time.Hour, time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			svc, _ := editSvc(t)
			chainTurn(t, svc, "c", "secret", "alice")
			if err := svc.MessageEdited("g", "c", "secret", "corrected words", time.Now().Add(skew)); err != nil {
				t.Fatal(err)
			}
			if err := svc.DrainRedactions(context.Background()); err != nil {
				t.Fatal(err)
			}
			turn := chainTurn(t, svc, "c", "after", "bob")
			if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, turn) {
				t.Fatalf("a reply after the edit was refused: %d replies kept", n)
			}
		})
	}
}

// A deletion or a withdrawal blocks what it blocked before: the window keeps refusing replies that hold the
// message's IDs, and a turn that starts long after still cannot lift that.
func TestADeletionStaysBlockedForLaterTurns(t *testing.T) {
	for _, change := range []string{"delete", "blank"} {
		t.Run(change, func(t *testing.T) {
			svc, _ := editSvc(t)
			chainTurn(t, svc, "c", "secret", "alice")
			changeSource(t, svc, change, "secret", "alice")
			if settled := svc.SettledSourcesForTest(); len(settled) != 0 {
				t.Fatalf("a deletion left sources marked as settled: %v", settled)
			}
			// a turn that starts now and somehow still holds the message's ID
			if kept := svc.KeepLateReplyForTest("g", "c", "late-q", "bob", []string{"secret"}); kept {
				t.Fatal("a reply holding a deleted message's ID was kept")
			}
		})
	}
}

// An edit followed by a deletion, in one pass of the worker or in two: what the edit settled does not
// outlive the deletion.
func TestADeletionUndoesWhatAnEditSettled(t *testing.T) {
	for _, together := range []bool{false, true} {
		t.Run(fmt.Sprintf("together=%v", together), func(t *testing.T) {
			svc, _ := editSvc(t)
			chainTurn(t, svc, "c", "secret", "alice")
			if together {
				svc.HoldRedactionForTest(true)
			}
			if err := svc.MessageEdited("g", "c", "secret", "corrected words", time.Now()); err != nil {
				t.Fatal(err)
			}
			if !together {
				if err := svc.DrainRedactions(context.Background()); err != nil {
					t.Fatal(err)
				}
				if settled := svc.SettledSourcesForTest(); !contains(settled, "secret") {
					t.Fatalf("the finished edit is not settled: %v", settled)
				}
			}
			if err := svc.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
				t.Fatal(err)
			}
			if together {
				svc.HoldRedactionForTest(false)
			}
			if err := svc.DrainRedactions(context.Background()); err != nil {
				t.Fatal(err)
			}
			if settled := svc.SettledSourcesForTest(); contains(settled, "secret") {
				t.Fatalf("the deleted message is still settled: %v", settled)
			}
			if kept := svc.KeepLateReplyForTest("g", "c", "late-q", "bob", []string{"secret"}); kept {
				t.Fatal("a reply holding a deleted message's ID was kept")
			}
		})
	}
}

// A second edit waiting in the queue: what the first one finished says nothing about the words still to
// come, so a turn reading now is not excused.
func TestAnEditWaitingBehindAnotherIsNotSettled(t *testing.T) {
	svc, _ := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	if err := svc.MessageEdited("g", "c", "secret", "first correction", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.HoldRedactionForTest(true)
	if err := svc.QueueEditForTest("g", "c", "secret", "second correction", time.Now()); err != nil {
		t.Fatal(err)
	}
	if settled := svc.SettledSourcesForTest(); contains(settled, "secret") {
		t.Fatalf("a queued edit left the message settled: %v", settled)
	}
	svc.HoldRedactionForTest(false)
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settled := svc.SettledSourcesForTest(); !contains(settled, "secret") {
		t.Fatalf("the last edit finished but the message is not settled: %v", settled)
	}
}

// An edit that keeps the words (a changed attachment) is applied like any other.
func TestAnEditKeepingTheWordsSettles(t *testing.T) {
	svc, _ := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	if err := svc.MessageEdited("g", "c", "secret", "message secret from alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	turn := chainTurn(t, svc, "c", "after", "bob")
	if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, turn) {
		t.Fatalf("a reply after the edit was refused: %d replies kept", n)
	}
}

// The bot's own posts in the live window hold the words of replies. A reply built on the old words is out of
// the chat context after a change; its post must not come back as context in its place.
func TestABotPostBuiltOnChangedWordsLeavesTheContext(t *testing.T) {
	for _, change := range []string{"edit", "delete"} {
		t.Run(change, func(t *testing.T) {
			svc, prompts := editSvc(t)
			turn := chainTurn(t, svc, "c", "secret", "alice")
			const post = "post built on the old words"
			svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "bot-post", AuthorID: "the-bot", AuthorName: "bot", Content: post, FromBot: true, ReplyTo: "secret", At: time.Now()})
			svc.NoteReplyMessages(turn, "g", "c", []string{"bot-post"})
			before := prompts.calls()
			chainTurn(t, svc, "c", "check", "alice")
			if prompts.sawSince(before, post) {
				t.Fatal("the stored answer should stand in for its post while it is kept")
			}
			changeSource(t, svc, change, "secret", "alice")
			before = prompts.calls()
			chainTurn(t, svc, "c", "after", "alice")
			if prompts.sawSince(before, post) {
				t.Fatal("a post built on the old words reached the next prompt")
			}
		})
	}
}

// The post of a reply that was refused can reach the window after the change: nothing in the window knows
// yet, but the turn's record does.
func TestAPostArrivingAfterTheChangeStaysOutOfTheContext(t *testing.T) {
	svc, prompts := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	prompts.whileRunning(func() { changeSource(t, svc, "edit", "secret", "alice") })
	refused := chainTurn(t, svc, "c", "during", "alice")
	if n := assistantTurns(t, svc, "c"); n != 0 {
		t.Fatalf("the reply was not refused: %d kept", n)
	}
	const post = "late post built on the old words"
	svc.NoteReplyMessages(refused, "g", "c", []string{"late-post"})
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "late-post", AuthorID: "the-bot", AuthorName: "bot", Content: post, FromBot: true, ReplyTo: "during", At: time.Now()})
	before := prompts.calls()
	next := chainTurn(t, svc, "c", "after", "alice")
	if prompts.sawSince(before, post) {
		t.Fatal("the refused reply's post reached the next prompt")
	}
	if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, next) {
		t.Fatalf("the next reply was refused: %d kept", n)
	}
}

// searchSvc is a service whose planner always looks the question up.
func searchSvc(t *testing.T) (*application.Service, *engine.Engine, *promptServer) {
	t.Helper()
	p := &promptServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/embeddings":
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			data := make([]map[string]any, len(req.Input))
			for i := range req.Input {
				data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0, 0, 0, 0, 0}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"prompt_tokens": 8, "cost": 0.000001}})
		case "/chat/completions":
			body, _ := io.ReadAll(r.Body)
			p.mu.Lock()
			p.prompts = append(p.prompts, string(body))
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"memory_requirement":"required","tool_plan":[{"tool_name":"memory.search","parameters":{"query":"canary"}}]}`}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_OR_KEY", "k")
	svc := embedSvc(t, t.TempDir(), srv.URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["planner"] = config.Model{Provider: "openrouter", Model: "fake/plan"}
		d.Models["response"] = config.Model{Provider: "openrouter", Model: "fake/answer"}
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true}
		d.Memory.Retrieve = "auto"
		d.Chat.ToolMode = "planner"
	})
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	return svc, eng, p
}

// Search: the same answer, whether the corrected message comes from the recent lines or a lookup. A stale
// index shows no hits from that room, a new one shows the corrected words, and the reply on it stays,
// in the chat context and in the turn history (the leaf's ID comes back when the room is cut again).
func TestARepliesOnASearchHitOfAnEditedMessageIsKept(t *testing.T) {
	svc, eng, prompts := searchSvc(t)
	start := time.Now().Add(-48 * time.Hour)
	const oldWords = "the canary lives at number nine"
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "secret", AuthorID: "alice", AuthorName: "alice", Content: oldWords, At: start}); err != nil {
		t.Fatal(err)
	}
	// enough newer lines that the message is not one of the recent lines every reply reads anyway
	for i := range 24 {
		if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: fmt.Sprintf("filler%d", i), AuthorID: "bob", AuthorName: "bob", Content: fmt.Sprintf("unrelated chatter %d", i), At: start.Add(time.Duration(i+1) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	svc.MemoryPassForTest(context.Background())
	if _, err := eng.Reload(); err != nil {
		t.Fatal(err)
	}
	secret := corpus.DiscordLiveObservationID(engine.RoomKey("g", "c"), "secret")
	first, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "q1", Message: "where does the canary live?", Speaker: "bob", SpeakerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range first.Hits {
		for _, id := range h.ObservationIDs {
			found = found || id == secret
		}
	}
	if !found || !prompts.sawSince(0, oldWords) {
		t.Fatalf("the reply did not retrieve the message (state %s): %+v", first.RagState, first.Hits)
	}
	if err := svc.MessageEdited("g", "c", "secret", "the canary moved to number ten", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if answerKept(t, svc, first.TurnID) {
		t.Fatal("the turn history keeps the reply on the old words")
	}

	// the index is still the one built before the edit: its text of that room must not be shown
	from := prompts.calls()
	stale, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "q2", Message: "where does the canary live now?", Speaker: "bob", SpeakerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if prompts.sawSince(from, oldWords) {
		t.Fatal("a turn after the edit read the old words from the index it had")
	}
	if !answerKept(t, svc, stale.TurnID) {
		t.Fatal("the turn after the edit lost its answer")
	}

	svc.MemoryPassForTest(context.Background())
	if _, err := eng.Reload(); err != nil {
		t.Fatal(err)
	}
	from = prompts.calls()
	fresh, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "q3", Message: "where does the canary live these days?", Speaker: "bob", SpeakerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, h := range fresh.Hits {
		for _, id := range h.ObservationIDs {
			found = found || id == secret
		}
	}
	if !found {
		t.Fatalf("the reply did not retrieve the corrected message (state %s): %+v", fresh.RagState, fresh.Hits)
	}
	if prompts.sawSince(from, oldWords) {
		t.Fatal("the new index still shows the old words")
	}
	if !answerKept(t, svc, fresh.TurnID) {
		t.Fatal("the turn history dropped a reply on the corrected message")
	}
	if n := assistantTurns(t, svc, "c"); n != 2 {
		t.Fatalf("replies kept after the edit: %d, want 2", n)
	}

	// and a deletion stays as it was: the leaf that held a deleted message blocks the turn record
	if err := svc.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if answerKept(t, svc, fresh.TurnID) {
		t.Fatal("deleting the message left the reply on it in the turn history")
	}
	// the one between them saw nothing of that room (the stale index) and read no line of the message
	if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, stale.TurnID) {
		t.Fatalf("%d replies are still in the chat context, want only the one that never read the message", n)
	}
}

// The leaf that held a deleted message comes back with what is left of it. A reply on that has read nothing
// deleted, and it is kept in the chat context, as it was; its turn record stays blocked by the leaf's
// tombstone, also as it was. A deletion's reach is not narrowed here.
func TestALeafThatHeldADeletedMessageStaysBlockedInTheTurnRecord(t *testing.T) {
	svc, eng, _ := searchSvc(t)
	start := time.Now().Add(-48 * time.Hour)
	for i := range 25 {
		id := fmt.Sprintf("filler%d", i)
		if i == 3 {
			id = "secret" // not the first of its leaf: the leaf's ID comes from its first message
		}
		if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "bob", AuthorName: "bob", Content: fmt.Sprintf("the canary chatter %d", i), At: start.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	svc.MemoryPassForTest(context.Background())
	if _, err := eng.Reload(); err != nil {
		t.Fatal(err)
	}
	ask := func(id string) (string, map[string]bool) {
		t.Helper()
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "any canary chatter?", Speaker: "carol", SpeakerID: "carol"})
		if err != nil {
			t.Fatal(err)
		}
		docs := map[string]bool{}
		for _, h := range res.Hits {
			docs[h.DocID] = true
		}
		return res.TurnID, docs
	}
	_, before := ask("q1")
	if err := svc.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.MemoryPassForTest(context.Background())
	if _, err := eng.Reload(); err != nil {
		t.Fatal(err)
	}
	turn, after := ask("q2")
	again := false
	for doc := range after {
		again = again || before[doc]
	}
	if !again {
		t.Fatalf("no leaf came back under its ID (%v, then %v): this proves nothing", before, after)
	}
	if n := assistantTurns(t, svc, "c"); n != 1 {
		t.Fatalf("a reply on what is left of the leaf: %d replies kept, want 1", n)
	}
	if answerKept(t, svc, turn) {
		t.Fatal("the turn record on the leaf of a deleted message is no longer blocked")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// A settled edit excuses nothing about the edit after it: a turn that read the first correction is refused
// when a second one finishes while it runs.
func TestASecondEditDuringATurnRefusesItsReply(t *testing.T) {
	svc, prompts := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	changeSource(t, svc, "edit", "secret", "alice")
	if settled := svc.SettledSourcesForTest(); !contains(settled, "secret") {
		t.Fatalf("the first edit is not settled: %v", settled)
	}
	prompts.whileRunning(func() {
		if err := svc.MessageEdited("g", "c", "secret", "second correction", time.Now().Add(time.Minute)); err != nil {
			t.Error(err)
		}
		if err := svc.DrainRedactions(context.Background()); err != nil {
			t.Error(err)
		}
	})
	inFlight := chainTurn(t, svc, "c", "during", "bob")
	if n := assistantTurns(t, svc, "c"); n != 0 {
		t.Fatalf("%d replies on the first correction were kept", n)
	}
	if answerKept(t, svc, inFlight) {
		t.Fatal("the turn history keeps a reply on the first correction")
	}
	next := chainTurn(t, svc, "c", "after", "bob")
	if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, next) {
		t.Fatalf("the turn after the second edit was refused: %d replies kept", n)
	}
}

// A channel without capture keeps the message only as a chat turn (postmortem 0003): the same promise.
func TestAnEditInAChannelWithoutCapture(t *testing.T) {
	svc, prompts := editSvc(t)
	turn := func(id string) string {
		t.Helper()
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "message " + id, Speaker: "alice", SpeakerID: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		return res.TurnID
	}
	first := turn("secret")
	second := turn("n0")
	changeSource(t, svc, "edit", "secret", "alice")
	if n := assistantTurns(t, svc, "c"); n != 0 || answerKept(t, svc, first) || answerKept(t, svc, second) {
		t.Fatalf("replies on the old words stayed: %d", n)
	}
	before := prompts.calls()
	after := turn("n1")
	if !prompts.sawSince(before, "corrected words") {
		t.Fatal("the turn after the edit did not read the corrected message: this proves nothing")
	}
	if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, after) {
		t.Fatalf("a reply on the edited words was refused: %d kept", n)
	}
	changeSource(t, svc, "delete", "secret", "alice")
	if n := assistantTurns(t, svc, "c"); n != 0 || answerKept(t, svc, after) {
		t.Fatalf("a reply on the edited words survived the deletion: %d kept", n)
	}
}

// A turn that came with something already read (here a command's result) read it before it took its mark, so
// nothing excuses it: it stays as careful as before.
func TestATurnThatCameWithAResultIsNotExcused(t *testing.T) {
	svc, _ := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	changeSource(t, svc, "edit", "secret", "alice")
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "cmd", Message: "the weather?", Speaker: "bob", SpeakerID: "bob",
		Earlier: []chat.ToolResult{{Tool: "weather.now", OK: false, Status: "unavailable"}}})
	if err != nil {
		t.Fatal(err)
	}
	if n := assistantTurns(t, svc, "c"); n != 0 {
		t.Fatalf("a turn with a result read before it started was excused: %d replies kept", n)
	}
	if answerKept(t, svc, res.TurnID) {
		t.Fatal("the turn history keeps the answer of a turn with a result read before it started")
	}
}

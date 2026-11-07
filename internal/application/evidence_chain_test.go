package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// The promise these tests pin (postmortem 0003): a reply is made from what it read, so when a source
// message is deleted, edited or its author withdraws, every later reply built on it goes too, however
// many replies lie between them and whether or not the turn that first read it is still in the 40 kept
// turns. Replies that came before the source, and other channels, stay.

// chainTurn captures one message and answers it, as a channel with capture on does.
func chainTurn(t *testing.T, svc *application.Service, channel, id, author string) string {
	t.Helper()
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	text := "message " + id + " from " + author
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: channel, MessageID: id, AuthorID: author, AuthorName: author, Content: text, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: channel, MessageID: id, Message: text, Speaker: author, SpeakerID: author})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnID == "" {
		t.Fatal("the turn has no ID")
	}
	return res.TurnID
}

func chainSvc(t *testing.T) *application.Service {
	t.Helper()
	var calls atomic.Int32
	url := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &calls)
	return routedSvc(t, url, url)
}

func chainCount(t *testing.T, svc *application.Service, query string, args ...any) int {
	t.Helper()
	var n int
	if err := svc.Store.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func assistantTurns(t *testing.T, svc *application.Service, channel string) int {
	t.Helper()
	return chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE channel_key=? AND role='assistant'`, "discord:g:"+channel)
}

// answerKept reports whether the turn history still has the reply's text.
func answerKept(t *testing.T, svc *application.Service, turn string) bool {
	t.Helper()
	var answer sql.NullString
	if err := svc.Store.DB.QueryRow(`SELECT json_extract(body,'$.answer') FROM turn_log WHERE id=?`, turn).Scan(&answer); err != nil {
		t.Fatalf("turn %s: %v", turn, err)
	}
	return answer.String != ""
}

func TestAChangedSourceTakesEveryReplyBuiltOnIt(t *testing.T) {
	for _, change := range []string{"delete", "edit", "withdraw"} {
		for _, retention := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retention=%v", change, retention), func(t *testing.T) {
				svc := chainSvc(t)
				before := []string{chainTurn(t, svc, "c", "b0", "bob"), chainTurn(t, svc, "c", "b1", "bob")}
				elsewhere := []string{chainTurn(t, svc, "other", "x0", "carol"), chainTurn(t, svc, "other", "x1", "carol")}
				after := []string{chainTurn(t, svc, "c", "secret", "alice")}
				// far past the 40 kept turns: the turn that first read the source is long gone
				for i := range 50 {
					after = append(after, chainTurn(t, svc, "c", fmt.Sprintf("n%d", i), "bob"))
				}
				if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='secret'`); n != 0 {
					t.Fatalf("the source's own turn is still kept (%d rows): the chain is not tested", n)
				}
				if n := assistantTurns(t, svc, "c"); n != 20 {
					t.Fatalf("assistant turns before the change: %d, want the 20 pairs of the 40 kept", n)
				}
				if retention {
					// retention runs between the trim and the change: the chain must outlive it
					if _, err := svc.RunRetentionForTest(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				for _, turn := range after {
					if !answerKept(t, svc, turn) {
						t.Fatalf("turn %s has no answer before the change: the check below proves nothing", turn)
					}
				}

				switch change {
				case "delete":
					if err := svc.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
						t.Fatal(err)
					}
				case "edit":
					if err := svc.MessageEdited("g", "c", "secret", "corrected", time.Now()); err != nil {
						t.Fatal(err)
					}
				case "withdraw":
					if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "alice", Name: "alice"}); !r.OK {
						t.Fatal(r.Error)
					}
				}
				if err := svc.DrainRedactions(context.Background()); err != nil {
					t.Fatal(err)
				}

				if n := assistantTurns(t, svc, "c"); n != 0 {
					t.Fatalf("%d replies built on the source are still in the chat context", n)
				}
				for i, turn := range after {
					if answerKept(t, svc, turn) {
						t.Fatalf("turn %d after the source keeps its answer in the turn history", i)
					}
				}
				for _, turn := range before {
					if !answerKept(t, svc, turn) {
						t.Fatal("a reply made before the source existed lost its answer")
					}
				}
				for _, turn := range elsewhere {
					if !answerKept(t, svc, turn) {
						t.Fatal("a reply in another channel lost its answer")
					}
				}
				if n := assistantTurns(t, svc, "other"); n != 2 {
					t.Fatalf("assistant turns in the other channel: %d, want 2", n)
				}

				// the room is not stuck: the next reply is kept and its text stays
				next := chainTurn(t, svc, "c", "fresh", "bob")
				if n := assistantTurns(t, svc, "c"); n != 1 {
					t.Fatalf("assistant turns after a fresh message: %d, want 1", n)
				}
				if !answerKept(t, svc, next) {
					t.Fatal("the first reply after the change lost its answer")
				}
			})
		}
	}
}

// A second redaction of the same source (a crash between the work and removing the job) finds no more
// and breaks nothing; a later reply stays.
func TestRepeatedDeletionOfASourceIsHarmless(t *testing.T) {
	svc := chainSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	chainTurn(t, svc, "c", "n1", "bob")
	for range 2 {
		if err := svc.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
			t.Fatal(err)
		}
		if err := svc.DrainRedactions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := assistantTurns(t, svc, "c"); n != 0 {
		t.Fatalf("assistant turns: %d", n)
	}
	next := chainTurn(t, svc, "c", "fresh", "bob")
	if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, next) {
		t.Fatalf("assistant turns after a fresh message: %d", n)
	}
}

// Replies written by an earlier release hold the whole chain as a flat list. Replies written now, on top
// of them, must still go when a message only the old list names is deleted.
func TestRepliesOnTopOfAFlatListFollowItsDeletion(t *testing.T) {
	svc := chainSvc(t)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	flat := []string{"old-message", "old-observation", "another-old-message"}
	if err := eng.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{
		{Role: "user", Speaker: "bob", Content: "old question", NativeID: "old-q", SpeakerID: "bob"},
		{Role: "assistant", Content: "old answer", NativeID: "old-q", SpeakerID: "bob", SourceIDs: flat},
	}, 40); err != nil {
		t.Fatal(err)
	}
	var turns []string
	for i := range 3 {
		turns = append(turns, chainTurn(t, svc, "c", fmt.Sprintf("n%d", i), "bob"))
	}
	if n := assistantTurns(t, svc, "c"); n != 4 {
		t.Fatalf("assistant turns: %d, want 4", n)
	}
	if err := svc.MessagesDeleted("g", "c", []string{"old-message"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := assistantTurns(t, svc, "c"); n != 0 {
		t.Fatalf("%d replies on top of the old list survived", n)
	}
	for _, turn := range turns {
		if answerKept(t, svc, turn) {
			t.Fatal("the turn history keeps a reply on top of the old list")
		}
	}
}

// A thread starts with copies of its caller's turns. The copies, and replies in the thread, are built on
// the same sources as the parent's.
func TestThreadCopiesFollowADeletionInTheParentChain(t *testing.T) {
	svc := chainSvc(t)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	chainTurn(t, svc, "c", "secret", "alice")
	chainTurn(t, svc, "c", "n1", "bob")
	parent, err := eng.Store.RecentChatTurns("discord:g:c", 40)
	if err != nil {
		t.Fatal(err)
	}
	var mine []store.ChatTurn
	for _, turn := range parent {
		if turn.SpeakerID == "bob" {
			mine = append(mine, turn)
		}
	}
	if err := eng.Store.AppendChatTurns("discord:g:thread", mine, 40); err != nil {
		t.Fatal(err)
	}
	reply := chainTurn(t, svc, "thread", "t1", "bob")
	if n := assistantTurns(t, svc, "thread"); n != 2 {
		t.Fatalf("assistant turns in the thread: %d, want the copy and the reply", n)
	}
	if err := svc.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := assistantTurns(t, svc, "thread"); n != 0 {
		t.Fatalf("%d replies in the thread survived", n)
	}
	if answerKept(t, svc, reply) {
		t.Fatal("the turn history keeps the thread's reply")
	}
}

// chainBody reads a turn's kept source list.
func chainBody(t *testing.T, svc *application.Service, turn string) []string {
	t.Helper()
	var raw string
	if err := svc.Store.DB.QueryRow(`SELECT body FROM turn_log WHERE id=?`, turn).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Context struct {
			IDs []string `json:"observation_ids"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return body.Context.IDs
}

// The source lists hold no repeats (a window of 16 turns used to name every shared ID eight times).
func TestATurnKeepsEachSourceOnce(t *testing.T) {
	svc := chainSvc(t)
	var last string
	for i := range 12 {
		last = chainTurn(t, svc, "c", fmt.Sprintf("n%d", i), "bob")
	}
	ids := chainBody(t, svc, last)
	if len(ids) == 0 {
		t.Fatal("the turn kept no sources: nothing is tested")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("%q is repeated in %s", id, strings.Join(ids, " "))
		}
		seen[id] = true
	}
}

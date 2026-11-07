package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// The promise these tests pin (postmortems 0003 and 0006): a reply is made from what it read. When a source message is
// deleted or edited, the replies that read it go, and those on top of them stay (evidence_direct_read_test.go). When
// its author withdraws, every later reply built on it goes too, however many replies lie between them and whether or
// not the turn that first read it is still in the 40 kept turns. Replies that came before the source, and other
// channels, stay.

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

				// this channel shows each caller their own conversation: of alice's message, only her turn read it
				wantKept, wantGone := 20, after[:1]
				stayed := after[1:]
				if change == "withdraw" {
					wantKept, wantGone, stayed = 0, after, nil
				}
				if n := assistantTurns(t, svc, "c"); n != wantKept {
					t.Fatalf("%d replies in the chat context, want %d", n, wantKept)
				}
				for i, turn := range wantGone {
					if answerKept(t, svc, turn) {
						t.Fatalf("turn %d after the source keeps its answer in the turn history", i)
					}
				}
				for i, turn := range stayed {
					if !answerKept(t, svc, turn) {
						t.Fatalf("turn %d did not read the source and lost its answer", i+1)
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
				if n := assistantTurns(t, svc, "c"); n != min(wantKept+1, 20) {
					t.Fatalf("assistant turns after a fresh message: %d, want %d", n, min(wantKept+1, 20))
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
	// bob's own conversation: his turn read neither alice's message nor her reply
	if n := assistantTurns(t, svc, "c"); n != 1 {
		t.Fatalf("assistant turns: %d", n)
	}
	next := chainTurn(t, svc, "c", "fresh", "bob")
	if n := assistantTurns(t, svc, "c"); n != 2 || !answerKept(t, svc, next) {
		t.Fatalf("assistant turns after a fresh message: %d", n)
	}
}

// Replies written by an earlier release hold the whole chain as a flat list, and count as having read all of it. The
// replies written now, on top of them, read only the old reply: they stay.
func TestRepliesOnTopOfAFlatListStayAfterItsDeletion(t *testing.T) {
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
	if n := assistantTurns(t, svc, "c"); n != 3 {
		t.Fatalf("%d replies in the context, want the 3 on top of the old list without it", n)
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE content='old answer'`); n != 0 {
		t.Fatal("the reply whose list names the message stayed")
	}
	for _, turn := range turns {
		if !answerKept(t, svc, turn) {
			t.Fatal("a reply on top of the old list lost its answer")
		}
	}
}

// A thread starts with copies of its caller's turns. A copy of a reply that read the message goes with it. A reply in
// the thread read the copy, not the message, and stays.
func TestThreadCopiesFollowADeletionInTheParentChain(t *testing.T) {
	svc := chainSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Attribution: "off"}, "thread": {Respond: "mention", Attribution: "off"}}}}
	})
	chainTurn(t, svc, "c", "secret", "alice")
	chainTurn(t, svc, "c", "n1", "bob")
	if n := svc.SeedThreadForTest("g", "c", "thread", "bob"); n != 2 {
		t.Fatalf("seeded %d", n)
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
	if n := assistantTurns(t, svc, "thread"); n != 1 {
		t.Fatalf("%d replies in the thread, want the one that read the copy and not the copy", n)
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord:g:thread' AND role='assistant' AND native_id='n1'`); n != 0 {
		t.Fatal("the copy of the reply that read the message stayed")
	}
	if !answerKept(t, svc, reply) {
		t.Fatal("the thread's reply lost its answer")
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

// A reply made from a memory the bot looked up is built on that memory: deleting the message takes the
// reply out of the chat context, and every reply after it.
func TestDeletingARetrievedMessageTakesTheReplyBuiltOnIt(t *testing.T) {
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
	start := time.Now().Add(-48 * time.Hour)
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "secret", AuthorID: "alice", AuthorName: "alice", Content: "the canary lives at number nine", At: start}); err != nil {
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
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "q1", Message: "where does the canary live?", Speaker: "bob", SpeakerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, h := range res.Hits {
		for _, id := range h.ObservationIDs {
			seen = seen || id == corpus.DiscordLiveObservationID(engine.RoomKey("g", "c"), "secret")
		}
	}
	if !seen {
		t.Fatalf("the reply did not retrieve the message (state %s): %+v", res.RagState, res.Hits)
	}
	later := chainTurn(t, svc, "c", "n1", "bob")
	if n := assistantTurns(t, svc, "c"); n != 2 {
		t.Fatalf("assistant turns: %d, want 2", n)
	}
	if err := svc.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if answerKept(t, svc, res.TurnID) {
		t.Fatal("the turn history keeps the reply that retrieved the deleted message")
	}
	if n := assistantTurns(t, svc, "c"); n != 0 {
		t.Fatalf("%d replies built on the retrieved message are still in the chat context", n)
	}
	if answerKept(t, svc, later) {
		t.Fatal("the turn history keeps a reply built on the retrieved message")
	}

	// the leaf that held the message is cut again; a reply on what remains of it has read nothing deleted
	svc.MemoryPassForTest(context.Background())
	if _, err := eng.Reload(); err != nil {
		t.Fatal(err)
	}
	res, err = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "q2", Message: "any unrelated chatter?", Speaker: "bob", SpeakerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("no hits after the leaves were cut again (state %s): this proves nothing", res.RagState)
	}
	if n := assistantTurns(t, svc, "c"); n != 1 {
		t.Fatalf("a reply on the remaining messages was refused: %d replies kept", n)
	}
}

// The lists stay the size of what a turn read: a reply names the turns it read, not everything they read.
func TestSourceListsStopGrowingWithTheConversation(t *testing.T) {
	svc := chainSvc(t)
	var last string
	sizes := map[int]int{}
	links := map[int]int{}
	for i := 1; i <= 90; i++ {
		last = chainTurn(t, svc, "c", fmt.Sprintf("n%d", i), fmt.Sprintf("u%d", i%5))
		if i == 30 || i == 60 || i == 90 {
			sizes[i] = len(chainBody(t, svc, last))
			links[i] = chainCount(t, svc, `SELECT COUNT(*) FROM evidence_links`)
		}
	}
	// which lines are the recent ones varies a little between runs when they share a timestamp
	if sizes[30] == 0 || sizes[90] > sizes[30]+sizes[30]/2 {
		t.Fatalf("source list of one turn at 30, 60, 90 turns: %v", sizes)
	}
	// each turn adds a like amount to the index, however long the room has talked
	early, late := links[60]-links[30], links[90]-links[60]
	if late > early+early/3 {
		t.Fatalf("index entries added by turns 31-60: %d, by turns 61-90: %d", early, late)
	}
}

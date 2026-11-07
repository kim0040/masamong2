package application_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// The promise these tests pin (decision of 2026-11-08, postmortem 0006). Deleting or editing a message takes the
// replies whose prompt held its words, and no others: a reply that read only such a reply stays, whatever it carried
// over. A withdrawal stays as it was, and goes on through the replies a deletion has removed.

const (
	chainWords = "the words of the message canary"
	// a prompt shows the last 8 rows, 4 pairs: the message's own turn and the 4 after it read it
	chainReaders = 5
	chainLength  = 13
)

// directChain answers the message and 12 turns after it, in a channel that stores nothing and shows everything.
func directChain(t *testing.T) (*application.Service, *promptServer, []string) {
	t.Helper()
	svc, prompts := quietSvc(t)
	turns := make([]string, chainLength)
	for i := range turns {
		id, speaker, words := fmt.Sprintf("n%d", i), "bob", fmt.Sprintf("question %d", i)
		if i == 0 {
			id, speaker, words = "m", "alice", chainWords
		}
		before := prompts.calls()
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: words, Speaker: speaker, SpeakerID: speaker})
		if err != nil {
			t.Fatal(err)
		}
		turns[i] = res.TurnID
		if read := prompts.sawSince(before, chainWords); read != (i < chainReaders) {
			t.Fatalf("turn %d read the message: %v. The counts below prove nothing", i, read)
		}
	}
	return svc, prompts, turns
}

func wantAnswers(t *testing.T, svc *application.Service, turns []string, gone int) {
	t.Helper()
	for i, turn := range turns {
		if kept := answerKept(t, svc, turn); kept == (i < gone) {
			t.Errorf("turn %d of %d: answer kept %v, want the first %d gone", i, len(turns), kept, gone)
		}
	}
	if n := assistantTurns(t, svc, "c"); n != len(turns)-gone {
		t.Errorf("%d replies in the context, want %d", n, len(turns)-gone)
	}
}

func TestAMessageChangeTakesTheRepliesThatReadIt(t *testing.T) {
	for _, change := range []string{"delete", "blank", "edit"} {
		for _, retention := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retention=%v", change, retention), func(t *testing.T) {
				svc, prompts, turns := directChain(t)
				if retention { // before the change, and after it below: neither cuts what a withdrawal will follow
					if _, err := svc.RunRetentionForTest(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				changeSource(t, svc, change, "m", "alice")
				t.Logf("%s: %d of %d replies gone", change, countGone(t, svc, turns), len(turns))
				wantAnswers(t, svc, turns, chainReaders)
				if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE content LIKE '%message canary%'`); n != 0 {
					t.Fatalf("%d rows hold the old words", n)
				}
				// the room goes on, on what stayed
				before := prompts.calls()
				res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "after", Message: "and now?", Speaker: "bob", SpeakerID: "bob"})
				if err != nil {
					t.Fatal(err)
				}
				if prompts.sawSince(before, chainWords) {
					t.Fatal("the next turn read the old words")
				}
				if !answerKept(t, svc, res.TurnID) {
					t.Fatal("the next turn's reply was refused")
				}
			})
		}
	}
}

func countGone(t *testing.T, svc *application.Service, turns []string) int {
	t.Helper()
	gone := 0
	for _, turn := range turns {
		if !answerKept(t, svc, turn) {
			gone++
		}
	}
	return gone
}

func TestAWithdrawalStillFollowsEveryReply(t *testing.T) {
	t.Run("the whole chain", func(t *testing.T) {
		svc, _, turns := directChain(t)
		changeSource(t, svc, "withdraw", "m", "alice")
		wantAnswers(t, svc, turns, len(turns))
	})
	for _, retention := range []bool{false, true} {
		// a deletion removed the replies between the person and what stayed: the links go on through them
		t.Run(fmt.Sprintf("through replies a deletion removed/retention=%v", retention), func(t *testing.T) {
			svc, _, turns := directChain(t)
			changeSource(t, svc, "delete", "m", "alice")
			wantAnswers(t, svc, turns, chainReaders)
			if retention {
				if _, err := svc.RunRetentionForTest(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			changeSource(t, svc, "withdraw", "m", "alice")
			wantAnswers(t, svc, turns, len(turns))
		})
	}
}

// A withdrawal reaches a reply through its window's links, read or not. Rows without replies between them leave no
// reply to carry the chain through the prompt's last rows.
func TestAWithdrawalFollowsTheWindowBeyondThePrompt(t *testing.T) {
	svc, prompts := quietSvc(t)
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m", Message: chainWords, Speaker: "alice", SpeakerID: "alice"}); err != nil {
		t.Fatal(err)
	}
	eng := mustEngine(t, svc)
	for i := range 9 {
		if err := eng.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "user", Speaker: "bob", Content: "no reply to this", NativeID: fmt.Sprintf("q%d", i), SpeakerID: "bob"}}, 40); err != nil {
			t.Fatal(err)
		}
	}
	before := prompts.calls()
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "n", Message: "and?", Speaker: "bob", SpeakerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if prompts.sawSince(before, chainWords) {
		t.Fatal("the turn read the message: this proves nothing")
	}
	changeSource(t, svc, "delete", "other", "carol") // nothing to do with them
	if !answerKept(t, svc, res.TurnID) {
		t.Fatal("the reply went on an unrelated change")
	}
	changeSource(t, svc, "withdraw", "m", "alice")
	if answerKept(t, svc, res.TurnID) {
		t.Fatal("a withdrawal did not reach the reply through the window's link")
	}
}

// A person's message can have no turn of theirs: a line someone read, or looked up. The withdrawal follows the replies
// on top of it all the same, whichever of its two names the reply holds, and after the message was deleted too: the
// deletion left the reply on top, and the withdrawal finds the message by its row, not by its text.
func TestAWithdrawalFollowsARoomLineOfThePerson(t *testing.T) {
	for _, held := range []string{"observation", "message"} {
		for _, deletedFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deleted first=%v", held, deletedFirst), func(t *testing.T) {
				svc, _ := quietSvc(t)
				eng := mustEngine(t, svc)
				if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "elsewhere", MessageID: "found", AuthorID: "carol", AuthorName: "carol", Content: "an old message of carol's", At: time.Now()}); err != nil {
					t.Fatal(err)
				}
				source := "found"
				if held == "observation" {
					if err := svc.Store.DB.QueryRow(`SELECT observation_id FROM memory_observations WHERE native_message_id='found'`).Scan(&source); err != nil {
						t.Fatal(err)
					}
				}
				if !svc.KeepLateReplyForTest("g", "c", "looked-up", "bob", []string{source}) {
					t.Fatal("the reply was refused before the change")
				}
				turns, err := eng.Store.RecentChatTurns("discord:g:c", 1)
				if err != nil || len(turns) != 1 {
					t.Fatalf("%d %v", len(turns), err)
				}
				if !svc.KeepLateReplyForTest("g", "c", "on-top", "bob", []string{turns[0].SourceIDs[0]}) {
					t.Fatal("the reply on top was refused before the change")
				}
				if deletedFirst {
					changeSource(t, svc, "delete", "found", "carol")
					if n := assistantTurns(t, svc, "c"); n != 1 {
						t.Fatalf("%d replies after the deletion, want the one on top", n)
					}
				}
				changeSource(t, svc, "withdraw", "found", "carol")
				if n := assistantTurns(t, svc, "c"); n != 0 {
					t.Fatalf("%d replies stayed", n)
				}
			})
		}
	}
}

// A line read from the room is read, in a channel that shows them.
func TestALiveLineTakesTheRepliesThatReadIt(t *testing.T) {
	svc, prompts := quietSvc(t)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	const words = "a line of carol's in the room canary"
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "line", AuthorID: "carol", AuthorName: "carol", Content: words, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before := prompts.calls()
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "q", Message: "what did she say?", Speaker: "bob", SpeakerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if !prompts.sawSince(before, words) {
		t.Fatal("the line did not reach the prompt: this proves nothing")
	}
	changeSource(t, svc, "delete", "line", "carol")
	if answerKept(t, svc, res.TurnID) || assistantTurns(t, svc, "c") != 0 {
		t.Fatal("a reply that read the line stayed")
	}
}

// A search result names the message's stored copy, and a reply holding that read it.
func TestASearchResultTakesTheReplyThatReadIt(t *testing.T) {
	svc, _ := quietSvc(t)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "elsewhere", MessageID: "found", AuthorID: "carol", AuthorName: "carol", Content: "an old message", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var observation string
	if err := svc.Store.DB.QueryRow(`SELECT observation_id FROM memory_observations WHERE native_message_id='found'`).Scan(&observation); err != nil {
		t.Fatal(err)
	}
	if !svc.KeepLateReplyForTest("g", "c", "looked-up", "bob", []string{observation}) {
		t.Fatal("the reply was refused before the change")
	}
	if !svc.KeepLateReplyForTest("g", "c", "unrelated", "bob", []string{"another"}) {
		t.Fatal("the other reply was refused")
	}
	changeSource(t, svc, "delete", "found", "carol")
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='looked-up'`); n != 0 {
		t.Fatal("the reply that looked the message up stayed")
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='unrelated'`); n != 1 {
		t.Fatal("a reply that did not look it up went")
	}
}

// A turn under way when the change comes is judged the same way.
func TestAChangeDuringATurnOnTopOfTheReaders(t *testing.T) {
	t.Run("it read only replies: stored", func(t *testing.T) {
		svc, prompts, turns := directChain(t)
		var res string
		prompts.whileRunning(func() { changeSource(t, svc, "delete", "m", "alice") })
		r, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "during", Message: "and?", Speaker: "bob", SpeakerID: "bob"})
		if err != nil {
			t.Fatal(err)
		}
		res = r.TurnID
		settleJobs(t, svc)
		wantAnswers(t, svc, append(turns, res), chainReaders)
	})
	t.Run("it read a reply removed while it ran: stored", func(t *testing.T) {
		// the window holds the last reader: it is gone by the commit, and the turn holds its link
		svc, prompts := quietSvc(t)
		turns := make([]string, 0, chainReaders)
		for i := range chainReaders + 1 {
			id, speaker := fmt.Sprintf("n%d", i), "bob"
			if i == 0 {
				id, speaker = "m", "alice"
			}
			if i == chainReaders {
				prompts.whileRunning(func() { changeSource(t, svc, "delete", "m", "alice") })
			}
			res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "question " + id, Speaker: speaker, SpeakerID: speaker})
			if err != nil {
				t.Fatal(err)
			}
			turns = append(turns, res.TurnID)
		}
		settleJobs(t, svc)
		wantAnswers(t, svc, turns, chainReaders)
	})
	t.Run("it read the message: refused", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m", Message: chainWords, Speaker: "alice", SpeakerID: "alice"}); err != nil {
			t.Fatal(err)
		}
		before := prompts.calls()
		prompts.whileRunning(func() { changeSource(t, svc, "delete", "m", "alice") })
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "during", Message: "and?", Speaker: "bob", SpeakerID: "bob"})
		if err != nil {
			t.Fatal(err)
		}
		if !prompts.sawSince(before, chainWords) {
			t.Fatal("the turn did not read the message: this proves nothing")
		}
		if n := assistantTurns(t, svc, "c"); n != 0 || answerKept(t, svc, res.TurnID) {
			t.Fatalf("a reply that read the deleted message was kept: %d", n)
		}
	})
}

// A burst's turn holds its pieces' words, so a reply that read the turn read them, though only the turn names them.
func TestAReplyOnABurstReadItsPieces(t *testing.T) {
	svc, prompts := quietSvc(t)
	answer := func(id, words string, fragments ...string) string {
		t.Helper()
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: words, Fragments: fragments, Speaker: "bob", SpeakerID: "bob"})
		if err != nil {
			t.Fatal(err)
		}
		return res.TurnID
	}
	burst := answer("last", "piece one canary\nlast piece", "piece")
	before := prompts.calls()
	reader := answer("n0", "and?")
	if !prompts.sawSince(before, "piece one canary") {
		t.Fatal("the reader did not read the piece: this proves nothing")
	}
	changeSource(t, svc, "delete", "piece", "bob")
	if answerKept(t, svc, burst) || answerKept(t, svc, reader) {
		t.Fatal("a reply that read the piece's words stayed")
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE content LIKE '%piece one%'`); n != 0 {
		t.Fatal("the burst's turn stayed")
	}
}

// The same in a channel that shows each caller their conversation: its lines name the messages their words are.
func TestAReplyOnABurstInTheCallersConversation(t *testing.T) {
	svc := chainSvc(t)
	answer := func(id, words string, fragments ...string) string {
		t.Helper()
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: words, Fragments: fragments, Speaker: "bob", SpeakerID: "bob"})
		if err != nil {
			t.Fatal(err)
		}
		return res.TurnID
	}
	answer("last", "piece one canary\nlast piece", "piece")
	reader := answer("n0", "and?")
	changeSource(t, svc, "delete", "piece", "bob")
	if answerKept(t, svc, reader) {
		t.Fatal("a reply that read the piece's words stayed")
	}
}

// An edited message's turn is marked while its job runs, and read again once the edit settles, whatever retention did
// between the two.
func TestAnEditedTurnIsReadAfterRetention(t *testing.T) {
	svc, _, _ := directChain(t)
	changeSource(t, svc, "edit", "m", "alice")
	if _, err := svc.RunRetentionForTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	var words string
	if err := svc.Store.DB.QueryRow(`SELECT content FROM chat_turns WHERE native_id='m'`).Scan(&words); err != nil {
		t.Fatal(err)
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM privacy_settled s JOIN chat_turns t ON s.source_id='chat:'||t.id WHERE t.native_id='m'`); n != 1 {
		t.Fatalf("the edited turn is settled %d times after retention", n)
	}
	turns, err := mustEngine(t, svc).Store.RecentChatTurns("discord:g:c", 40)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, turn := range turns {
		found = found || turn.NativeID == "m" && turn.Content == words
	}
	if !found {
		t.Fatal("the edited turn is no longer read")
	}
}

func mustEngine(t *testing.T, svc *application.Service) *engine.Engine {
	t.Helper()
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// A reply's line in a channel that tells conversations apart is named by the reply, not by what it answered, and a
// reader's sources say so.
func TestAReplysLineIsNotItsRequest(t *testing.T) {
	svc := chainSvc(t)
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m", Message: chainWords, Speaker: "alice", SpeakerID: "alice"}); err != nil {
		t.Fatal(err)
	}
	ids := svc.ChannelLineIDsForTest("g", "c", "next")
	if strings.Contains(strings.Join(ids, " "), "turn:m") {
		t.Fatalf("a reply's line is named by its request: %q", ids)
	}
	var found bool
	for _, id := range ids {
		found = found || strings.HasPrefix(id, "turn:chat:")
	}
	if !found {
		t.Fatalf("no reply among the lines, this proves nothing: %q", ids)
	}
}

// What a reply is recorded as having read and what its prompts held are the same rows, whichever of them are long or
// empty: the words are in no prompt and a deletion leaves the reply, or they are in one and it goes.
func TestEveryPromptStaysInWhatTheReplyRecords(t *testing.T) {
	long := strings.Repeat("long words ", 50)
	for name, tc := range map[string]struct {
		rows     int
		content  string
		question string
		read     bool
	}{
		"long rows, the message before them":  {rows: chat.HistoryShown, content: long, question: "and now?"},
		"long rows, the message among them":   {rows: chat.HistoryShown - 1, content: long, question: "and now?", read: true},
		"empty rows, the message before them": {rows: chat.HistoryShown, content: "  ", question: "and now?"},
		"empty rows, the message among them":  {rows: chat.HistoryShown - 1, content: "  ", question: "and now?", read: true},
		// the memory classifier is told the bot's last reply: only one of the rows the reply records
		"a memory request, no reply among the rows": {rows: chat.HistoryShown, content: long, question: "기억해줘: 나는 차를 좋아해"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, prompts := quietSvc(t)
			e := mustEngine(t, svc)
			rows := []store.ChatTurn{{Role: "user", Content: chainWords, NativeID: "m", SpeakerID: "alice", Speaker: "alice"},
				{Role: "assistant", Content: chainWords + " again", NativeID: "m", SpeakerID: "alice"}}
			for i := range tc.rows {
				rows = append(rows, store.ChatTurn{Role: "user", Content: tc.content, NativeID: fmt.Sprintf("pad-%d", i), SpeakerID: "bob", Speaker: "bob"})
			}
			if tc.read {
				rows = rows[:1:1]
				for i := range tc.rows {
					rows = append(rows, store.ChatTurn{Role: "assistant", Content: tc.content, NativeID: fmt.Sprintf("pad-%d", i), SpeakerID: "bob"})
				}
			}
			if err := e.Store.AppendChatTurns("discord:g:c", rows, 40); err != nil {
				t.Fatal(err)
			}
			before := prompts.calls()
			res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "n", Message: tc.question, Speaker: "bob", SpeakerID: "bob"})
			if err != nil {
				t.Fatal(err)
			}
			if saw := prompts.sawSince(before, chainWords); saw != tc.read {
				t.Fatalf("a prompt held the message's words: %v, want %v", saw, tc.read)
			}
			changeSource(t, svc, "delete", "m", "alice")
			if kept := answerKept(t, svc, res.TurnID); kept == tc.read {
				t.Fatalf("the reply kept %v after a deletion of what it read: %v", kept, tc.read)
			}
		})
	}
}

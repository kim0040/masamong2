package application_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// The promise these tests pin (postmortem 0006): a person who fixes a typo while the bot is answering keeps the fixed
// words in the conversation. The turn stores the new words and not its reply, made from the old ones; whatever order
// the edit's job and the turn's commit come in, the old words are nowhere and the row stays.

const (
	oldWords = "old words of the typo canary"
	newWords = "new words after the fix"
)

// quietSvc is a channel that stores no messages and shows every reply the channel's plain conversation.
func quietSvc(t *testing.T) (*application.Service, *promptServer) {
	t.Helper()
	svc, prompts := editSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Attribution: "off"}}}}
	})
	return svc, prompts
}

// answerWhile answers the message, doing during what the person does while the turn runs.
func answerWhile(t *testing.T, svc *application.Service, prompts *promptServer, in application.ChatInput, during func()) {
	t.Helper()
	if during != nil {
		prompts.whileRunning(during)
	}
	in.GuildID, in.ChannelID, in.Speaker, in.SpeakerID = "g", "c", "alice", "alice"
	if in.MessageID == "" {
		in.MessageID = "m"
	}
	if in.Message == "" {
		in.Message = oldWords
	}
	if _, err := svc.ChatTurn(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if n := svc.TurnsInFlightForTest(); n != 0 {
		t.Fatalf("%d messages still registered after the turn", n)
	}
}

// channelRows reads the channel's rows as the next turn does, as "role: words".
func channelRows(t *testing.T, svc *application.Service, channel string) []string {
	t.Helper()
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	turns, err := eng.Store.RecentChatTurns("discord:g:"+channel, 40)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, turn := range turns {
		out = append(out, turn.Role+": "+turn.Content)
	}
	return out
}

func wantRows(t *testing.T, svc *application.Service, want ...string) {
	t.Helper()
	if got := channelRows(t, svc, "c"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows %q, want %q", got, want)
	}
}

func wantNoOldWords(t *testing.T, svc *application.Service) {
	t.Helper()
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns`); n == 0 {
		t.Fatal("no rows at all: the check below proves nothing")
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE content LIKE '%typo canary%'`); n != 0 {
		t.Fatalf("%d rows hold the old words", n)
	}
}

func settleJobs(t *testing.T, svc *application.Service) {
	t.Helper()
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func edit(t *testing.T, svc *application.Service, words string, at time.Time) {
	t.Helper()
	if err := svc.MessageEdited("g", "c", "m", words, at); err != nil {
		t.Error(err)
	}
}

const userNew = "user: " + newWords

func TestAnEditDuringATurnKeepsTheNewWordsAndNotTheReply(t *testing.T) {
	svc, prompts := quietSvc(t)
	answerWhile(t, svc, prompts, application.ChatInput{}, func() { edit(t, svc, newWords, time.Now()) })
	drain(t, svc)
	wantRows(t, svc, userNew)
	wantNoOldWords(t, svc)
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord:g:c' AND native_id='m' AND speaker='alice' AND speaker_id='alice'`); n != 1 {
		t.Fatalf("the row lost its speaker, channel or message: %d", n)
	}
	// the point of it all: the next turn reads the fixed words, and its reply is an ordinary one
	before := prompts.calls()
	answerWhile(t, svc, prompts, application.ChatInput{MessageID: "next", Message: "and then?"}, nil)
	if !prompts.sawSince(before, newWords) {
		t.Fatal("the next turn did not read the new words")
	}
	if n := assistantTurns(t, svc, "c"); n != 1 {
		t.Fatalf("the reply on the new words was refused: %d kept", n)
	}
}

// The turn commits before, after or at the same moment as the edit's job: the row stays and the old words do not.
func TestTheJobOfAnEditDuringATurnLeavesTheNewRow(t *testing.T) {
	check := func(t *testing.T, svc *application.Service, prompts *promptServer) {
		t.Helper()
		wantRows(t, svc, userNew)
		wantNoOldWords(t, svc)
		if settled := svc.SettledSourcesForTest(); !contains(settled, "m") {
			t.Fatalf("the edit did not settle: %v", settled)
		}
		answerWhile(t, svc, prompts, application.ChatInput{MessageID: "next", Message: "and then?"}, nil)
		if n := assistantTurns(t, svc, "c"); n != 1 {
			t.Fatalf("the reply built on the row was refused once settled: %d kept", n)
		}
		// a change after the commit reaches the row: its index entries are the ordinary ones
		if !svc.KeepLateReplyForTest("g", "c", "late", "bob", []string{"unrelated"}) {
			t.Fatal("an unrelated reply was refused: this proves nothing")
		}
		if err := svc.MessagesDeleted("g", "c", []string{"m"}); err != nil {
			t.Fatal(err)
		}
		drain(t, svc)
		if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='m'`); n != 0 {
			t.Fatalf("the deletion left %d rows of the message", n)
		}
	}
	t.Run("job first", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		answerWhile(t, svc, prompts, application.ChatInput{}, func() {
			edit(t, svc, newWords, time.Now())
			drain(t, svc)
		})
		if n := chainCount(t, svc, `SELECT COUNT(*) FROM redaction_jobs`); n != 0 {
			t.Fatalf("%d jobs left", n)
		}
		check(t, svc, prompts)
	})
	t.Run("commit first", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		svc.HoldRedactionForTest(true)
		answerWhile(t, svc, prompts, application.ChatInput{}, func() { edit(t, svc, newWords, time.Now()) })
		if n := chainCount(t, svc, `SELECT COUNT(*) FROM redaction_jobs`); n != 1 {
			t.Fatalf("the job was to wait, %d in the queue", n)
		}
		// not settled evidence yet: the row is there, and a reply read from it now would be refused
		if settled := svc.SettledSourcesForTest(); len(settled) != 0 {
			t.Fatalf("settled before the job ran: %v", settled)
		}
		if svc.KeepLateReplyForTest("g", "c", "early", "bob", []string{"m"}) {
			t.Fatal("a reply on the message was kept before the edit settled")
		}
		svc.HoldRedactionForTest(false)
		drain(t, svc)
		check(t, svc, prompts)
	})
	t.Run("commit between the note and the intent", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		at := time.Now()
		answerWhile(t, svc, prompts, application.ChatInput{}, func() { svc.NoteEditForTest("m", newWords, at) })
		wantRows(t, svc, userNew)
		if n := chainCount(t, svc, `SELECT COUNT(*) FROM privacy_tombstones WHERE source_id='m'`); n != 0 {
			t.Fatalf("%d tombstones: the commit did not come first", n)
		}
		edit(t, svc, newWords, at)
		settleJobs(t, svc)
		check(t, svc, prompts)
	})
	t.Run("the note comes before the intent", func(t *testing.T) {
		// else a commit between them finds the tombstone and no words
		svc, prompts := quietSvc(t)
		eng, err := svc.MemoryEngine()
		if err != nil {
			t.Fatal(err)
		}
		answerWhile(t, svc, prompts, application.ChatInput{}, func() {
			var wg sync.WaitGroup
			if err := eng.Store.DB.WithWriter(func(*sql.Tx) error {
				wg.Add(1)
				go func() {
					defer wg.Done()
					edit(t, svc, newWords, time.Now())
				}()
				for deadline := time.Now().Add(3 * time.Second); svc.NotedWordsForTest("m") != newWords; time.Sleep(time.Millisecond) {
					if time.Now().After(deadline) {
						t.Error("the edit is not noted while its intent waits for the writer")
						break
					}
				}
				return nil
			}); err != nil {
				t.Error(err)
			}
			wg.Wait()
		})
		settleJobs(t, svc)
		check(t, svc, prompts)
	})
	t.Run("an edit that commits while the turn waits for the writer", func(t *testing.T) {
		// the turn decides in its transaction. Deciding before it, it would find no note and then the tombstone.
		svc, prompts := quietSvc(t)
		eng, err := svc.MemoryEngine()
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now()
		var wg sync.WaitGroup
		answerWhile(t, svc, prompts, application.ChatInput{Deliver: func(_ context.Context, res chat.Result) string {
			holding := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := eng.Store.DB.WithWriter(func(tx *sql.Tx) error {
					close(holding)
					time.Sleep(150 * time.Millisecond) // the turn reaches the writer's lock
					svc.NoteEditForTest("m", newWords, at)
					return store.Tombstone(tx, "message", "m", at.UTC().Format(time.RFC3339Nano))
				}); err != nil {
					t.Error(err)
				}
			}()
			<-holding
			return res.Answer
		}}, nil)
		wg.Wait()
		wantRows(t, svc, userNew)
	})
	t.Run("at the same moment", func(t *testing.T) {
		for range 8 {
			svc, prompts := quietSvc(t)
			var wg sync.WaitGroup
			answerWhile(t, svc, prompts, application.ChatInput{Deliver: func(_ context.Context, res chat.Result) string {
				wg.Add(1)
				go func() { // races the commit that follows this return
					defer wg.Done()
					edit(t, svc, newWords, time.Now())
				}()
				return res.Answer
			}}, nil)
			wg.Wait()
			drain(t, svc)
			check(t, svc, prompts)
		}
	})
}

func TestTheNewestEditDuringATurnWins(t *testing.T) {
	first, second := time.Now(), time.Now().Add(time.Minute)
	for name, order := range map[string][]struct {
		words string
		at    time.Time
	}{
		"in order":     {{"first fix", first}, {newWords, second}},
		"out of order": {{newWords, second}, {"first fix", first}},
	} {
		t.Run(name, func(t *testing.T) {
			svc, prompts := quietSvc(t)
			answerWhile(t, svc, prompts, application.ChatInput{}, func() {
				for _, e := range order {
					edit(t, svc, e.words, e.at)
				}
			})
			drain(t, svc)
			wantRows(t, svc, userNew)
			wantNoOldWords(t, svc)
			if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE content='first fix'`); n != 0 {
				t.Fatal("the older fix stayed")
			}
		})
	}
	t.Run("the same event twice", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		answerWhile(t, svc, prompts, application.ChatInput{}, func() {
			edit(t, svc, newWords, first)
			drain(t, svc)
			edit(t, svc, newWords, first)
		})
		drain(t, svc)
		wantRows(t, svc, userNew)
		if settled := svc.SettledSourcesForTest(); !contains(settled, "m") {
			t.Fatalf("the second delivery undid the settling: %v", settled)
		}
	})
}

// captured stores the message as a channel with capture on does: the copy an edit keeping the words is compared with.
func captured(t *testing.T, svc *application.Service, words string) {
	t.Helper()
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m", AuthorID: "alice", AuthorName: "alice", Content: words, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func keptWords(t *testing.T, svc *application.Service, words string, at time.Time) {
	t.Helper()
	kept, err := svc.MessageKeptItsWords("g", "c", "m", words, at)
	if err != nil || !kept {
		t.Errorf("the edit keeping the words: %v, %v", kept, err)
	}
}

func TestEditsThatChangeNothingDuringATurn(t *testing.T) {
	t.Run("the same words store the turn as before", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		captured(t, svc, oldWords)
		answerWhile(t, svc, prompts, application.ChatInput{}, func() { keptWords(t, svc, oldWords, time.Now()) })
		if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='m' AND role='user' AND content=?`, oldWords); n != 1 {
			t.Fatalf("user rows: %d", n)
		}
		if n := assistantTurns(t, svc, "c"); n != 1 {
			t.Fatalf("the reply of a turn whose words stayed: %d", n)
		}
	})
	t.Run("an edit older than one that kept the words", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		captured(t, svc, oldWords)
		answerWhile(t, svc, prompts, application.ChatInput{}, func() {
			at := time.Now()
			keptWords(t, svc, oldWords, at)
			edit(t, svc, "stale fix", at.Add(-time.Minute))
		})
		if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='m' AND role='user' AND content=?`, oldWords); n != 1 {
			t.Fatalf("user rows: %d", n)
		}
		if n := assistantTurns(t, svc, "c"); n != 1 {
			t.Fatalf("the stale edit took the reply: %d", n)
		}
	})
	t.Run("an edit older than the newest tombstone", func(t *testing.T) {
		// an edit before the turn registered wrote the tombstone: the words of an older one are not the current ones
		svc, prompts := quietSvc(t)
		at := time.Now()
		if err := svc.MessageEdited("g", "c", "m", "fix before the turn", at); err != nil {
			t.Fatal(err)
		}
		drain(t, svc)
		answerWhile(t, svc, prompts, application.ChatInput{}, func() { edit(t, svc, "stale fix", at.Add(-time.Minute)) })
		drain(t, svc)
		if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns`); n != 0 {
			t.Fatalf("%d rows from a stale edit", n)
		}
	})
	t.Run("same words after an edit: the edit's words stay", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		captured(t, svc, oldWords)
		answerWhile(t, svc, prompts, application.ChatInput{}, func() {
			at := time.Now()
			edit(t, svc, newWords, at)
			drain(t, svc)
			keptWords(t, svc, newWords, at.Add(time.Minute))
		})
		drain(t, svc)
		wantRows(t, svc, userNew)
	})
}

func TestChangesDuringATurnThatLeaveNothing(t *testing.T) {
	for name, during := range map[string]func(*testing.T, *application.Service){
		"delete": func(t *testing.T, svc *application.Service) {
			if err := svc.MessagesDeleted("g", "c", []string{"m"}); err != nil {
				t.Error(err)
			}
		},
		"edit then delete": func(t *testing.T, svc *application.Service) {
			edit(t, svc, newWords, time.Now())
			if err := svc.MessagesDeleted("g", "c", []string{"m"}); err != nil {
				t.Error(err)
			}
		},
		"edit, its job, then delete": func(t *testing.T, svc *application.Service) {
			edit(t, svc, newWords, time.Now())
			drain(t, svc)
			if err := svc.MessagesDeleted("g", "c", []string{"m"}); err != nil {
				t.Error(err)
			}
		},
		"blank": func(t *testing.T, svc *application.Service) { edit(t, svc, " \n ", time.Now()) },
		"edit then blank": func(t *testing.T, svc *application.Service) {
			edit(t, svc, newWords, time.Now())
			edit(t, svc, "", time.Now().Add(time.Minute))
		},
		"withdraw": func(t *testing.T, svc *application.Service) { changeSource(t, svc, "withdraw", "m", "alice") },
		"edit then withdraw": func(t *testing.T, svc *application.Service) {
			edit(t, svc, newWords, time.Now())
			changeSource(t, svc, "withdraw", "m", "alice")
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, prompts := quietSvc(t)
			answerWhile(t, svc, prompts, application.ChatInput{}, func() { during(t, svc) })
			drain(t, svc)
			if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns`); n != 0 {
				t.Fatalf("%d rows kept: %v", n, channelRows(t, svc, "c"))
			}
		})
	}
}

// Consent outlives the speaker's tombstone: it is still asked at the commit.
func TestAnEditDuringATurnOfAPersonWhoWithdrew(t *testing.T) {
	svc, prompts := quietSvc(t)
	answerWhile(t, svc, prompts, application.ChatInput{}, func() {
		edit(t, svc, newWords, time.Now())
		changeSource(t, svc, "withdraw", "m", "alice")
		if _, err := svc.Store.DB.Exec(`DELETE FROM privacy_tombstones WHERE kind='account'`); err != nil {
			t.Error(err)
		}
	})
	settleJobs(t, svc)
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns`); n != 0 {
		t.Fatalf("%d rows kept", n)
	}
}

// A blanking edit is a deletion and its tombstone decides. Until it commits there are no words to hold, and an empty
// row is never what the turn comes to.
func TestABlankingEditIsNotNotedAsWords(t *testing.T) {
	svc, prompts := quietSvc(t)
	answerWhile(t, svc, prompts, application.ChatInput{}, func() { svc.NoteEditForTest("m", "   ", time.Now()) })
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE trim(content)=''`); n != 0 {
		t.Fatalf("%d empty rows", n)
	}
	changeSource(t, svc, "blank", "m", "alice")
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='m'`); n != 0 {
		t.Fatalf("the deletion left %d rows", n)
	}
}

// After the commit the row is an ordinary turn of its message.
func TestALaterChangeReachesTheRowOfAnEditDuringATurn(t *testing.T) {
	svc, prompts := quietSvc(t)
	at := time.Now()
	answerWhile(t, svc, prompts, application.ChatInput{}, func() { edit(t, svc, newWords, at) })
	drain(t, svc)
	wantRows(t, svc, userNew)
	if err := svc.MessageEdited("g", "c", "m", "third words", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	wantRows(t, svc, "user: third words")
	// and a withdrawal: the row carries the speaker
	changeSource(t, svc, "withdraw", "m", "alice")
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns`); n != 0 {
		t.Fatalf("the withdrawal left %d rows", n)
	}
}

// A burst keeps no seams: the turn's text cannot be split back into its messages. Editing the message it answers
// keeps that message's new words alone, as an edit after the commit does (store.EditMessage). Changing an earlier
// piece keeps nothing, as after the commit too.
func TestAnEditDuringATurnOfABurst(t *testing.T) {
	burst := application.ChatInput{Message: "piece one\n" + oldWords, Fragments: []string{"f1"}}
	edited := func(t *testing.T, svc *application.Service, message, words string) {
		t.Helper()
		if err := svc.MessageEdited("g", "c", message, words, time.Now()); err != nil {
			t.Error(err)
		}
	}
	t.Run("the answered message", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		answerWhile(t, svc, prompts, burst, func() { edited(t, svc, "m", newWords) })
		drain(t, svc)
		wantRows(t, svc, userNew)
		wantNoOldWords(t, svc)
		// it holds none of the piece's words, so the piece's later change does not take it
		edited(t, svc, "f1", "piece one fixed")
		drain(t, svc)
		wantRows(t, svc, userNew)
	})
	for name, during := range map[string]func(*testing.T, *application.Service){
		"an earlier piece edited":  func(t *testing.T, svc *application.Service) { edited(t, svc, "f1", "piece one fixed") },
		"an earlier piece blanked": func(t *testing.T, svc *application.Service) { edited(t, svc, "f1", " ") },
		"an earlier piece deleted": func(t *testing.T, svc *application.Service) {
			if err := svc.MessagesDeleted("g", "c", []string{"f1"}); err != nil {
				t.Error(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, prompts := quietSvc(t)
			answerWhile(t, svc, prompts, burst, func() { during(t, svc) })
			drain(t, svc)
			if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns`); n != 0 {
				t.Fatalf("the old words of the burst stayed: %v", channelRows(t, svc, "c"))
			}
		})
	}
	t.Run("both", func(t *testing.T) {
		svc, prompts := quietSvc(t)
		answerWhile(t, svc, prompts, burst, func() {
			edited(t, svc, "f1", "piece one fixed")
			edited(t, svc, "m", newWords)
		})
		drain(t, svc)
		wantRows(t, svc, userNew)
		wantNoOldWords(t, svc)
	})
}

// Every way out of a turn takes it off the list, and an edit with no turn under way notes nothing.
func TestARunningTurnLeavesNothingBehind(t *testing.T) {
	svc, prompts := quietSvc(t)
	seen := func(want int) func() {
		return func() {
			if n := svc.TurnsInFlightForTest(); n != want {
				t.Errorf("%d messages registered while the turn runs, want %d", n, want)
			}
		}
	}
	base := application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m", Message: oldWords, Speaker: "alice", SpeakerID: "alice"}
	check := func(t *testing.T) {
		t.Helper()
		if n := svc.TurnsInFlightForTest(); n != 0 {
			t.Fatalf("%d messages still registered", n)
		}
	}
	t.Run("a turn registers while it runs", func(t *testing.T) {
		in := base
		in.Fragments = []string{"f1", "f2"}
		prompts.whileRunning(seen(1))
		if _, err := svc.ChatTurn(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		check(t)
	})
	t.Run("a lab turn registers nothing", func(t *testing.T) {
		in := base
		in.DryRun = true
		prompts.whileRunning(seen(0))
		if _, err := svc.ChatTurn(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		check(t)
	})
	t.Run("a refused message", func(t *testing.T) {
		in := base
		in.Message = " "
		if _, err := svc.ChatTurn(context.Background(), in); err == nil {
			t.Fatal("a blank message was answered")
		}
		check(t)
	})
	t.Run("a canceled turn", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = svc.ChatTurn(ctx, base)
		check(t)
	})
	t.Run("a panic in the turn", func(t *testing.T) {
		in := base
		in.Deliver = func(context.Context, chat.Result) string { panic(errors.New("the connector broke")) }
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic did not come through: this proves nothing")
				}
			}()
			_, _ = svc.ChatTurn(context.Background(), in)
		}()
		check(t)
	})
	t.Run("an edit with no turn under way", func(t *testing.T) {
		if err := svc.MessageEdited("g", "c", "nobody", "words", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := svc.MessagesDeleted("g", "c", []string{"nobody2"}); err != nil {
			t.Fatal(err)
		}
		check(t)
	})
	t.Run("two turns of one message", func(t *testing.T) {
		// one landing does not take the other off the list
		in := base
		prompts.whileRunning(func() {
			other := base
			other.ChannelID = "elsewhere"
			if _, err := svc.ChatTurn(context.Background(), other); err != nil {
				t.Error(err)
			}
			seen(1)()
		})
		if _, err := svc.ChatTurn(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		check(t)
	})
}

// A practice instance answers offline and takes the same way in and out.
func TestAPracticeTurnLeavesNothingBehind(t *testing.T) {
	p, err := application.OpenPractice(context.Background(), t.TempDir(), "ko", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ChatTurn(context.Background(), application.ChatInput{GuildID: application.PracticeGuild, ChannelID: application.PracticeChannel, MessageID: "m", Message: "캠핑 텐트", Speaker: "a", SpeakerID: "a"}); err != nil {
		t.Fatal(err)
	}
	if n := p.TurnsInFlightForTest(); n != 0 {
		t.Fatalf("%d messages still registered", n)
	}
}

// A thread's seed copies the row of the edit once settled, and a reminder or task, with no message, is untouched.
func TestThreadCopiesAndTurnsWithoutAMessageAfterAnEditDuringATurn(t *testing.T) {
	svc, prompts := quietSvc(t)
	answerWhile(t, svc, prompts, application.ChatInput{}, func() { edit(t, svc, newWords, time.Now()) })
	drain(t, svc)
	if n := svc.SeedThreadForTest("g", "c", "thread", "alice"); n != 1 {
		t.Fatalf("seeded %d", n)
	}
	if got := channelRows(t, svc, "thread"); len(got) != 1 || got[0] != userNew {
		t.Fatalf("the thread's copy: %q", got)
	}
	// the copy follows the message like any other
	if err := svc.MessagesDeleted("g", "c", []string{"m"}); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='m'`); n != 0 {
		t.Fatalf("%d rows of the message stayed", n)
	}
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "plain", Message: "from a reminder", SpeakerID: "alice", Speaker: "alice"}); err != nil {
		t.Fatal(err)
	}
	if got := channelRows(t, svc, "plain"); len(got) != 2 {
		t.Fatalf("a turn with no message: %q", got)
	}
}

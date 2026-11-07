package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// The promise after review of postmortem 0006: a finished edit stays finished. Settling, removing the edit's job
// and a replay of that job can happen in any order a crash leaves them in without touching a reply on the new
// words, and an unrelated withdrawal in the same batch does not leave the edit unfinished.

func queuedJobs(t *testing.T, s *Service) []redactionJob {
	t.Helper()
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Reader().Query(`SELECT id,payload FROM redaction_jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var jobs []redactionJob
	for rows.Next() {
		var j redactionJob
		var raw string
		if err := rows.Scan(&j.id, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	return jobs
}

func windowSources(t *testing.T, s *Service, key string) []string {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	turns, err := e.Store.RecentChatTurns(key, 16)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, turn := range turns {
		out = append(out, turn.SourceIDs...)
	}
	return out
}

func settledSet(s *Service) map[string]bool {
	settled := map[string]bool{}
	for _, id := range s.SettledSourcesForTest() {
		settled[id] = true
	}
	return settled
}

// seedEditedTurn writes a user turn for the message and a reply on it, as a channel without capture has them.
func seedEditedTurn(t *testing.T, s *Service, key, message, speaker string) {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
		{Role: "user", Speaker: speaker, Content: "old words", NativeID: message, SpeakerID: speaker},
	}, 40); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
		{Role: "assistant", Content: "reply on old words", NativeID: message, SpeakerID: speaker, SourceIDs: windowSources(t, s, key)},
	}, 40); err != nil {
		t.Fatal(err)
	}
}

// A crash between a finished edit and the removal of its job runs the same job again. By then replies on the new
// words have been kept: the walk from the message reaches them through the user turn that stayed.
func TestAnEditJobRunTwiceKeepsRepliesOnTheNewWords(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	db, _ := s.adminDB()
	const key = "discord:g:c"
	seedEditedTurn(t, s, key, "secret", "alice")
	s.redaction.running.Store(true)
	defer s.redaction.running.Store(false)
	if err := s.MessageEdited("g", "c", "secret", "new words", s.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	jobs := queuedJobs(t, s)
	if len(jobs) != 1 {
		t.Fatalf("jobs: %d", len(jobs))
	}
	if err := s.processRedactions(context.Background(), db, jobs); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Fatalf("replies on the old words after the first run: %d", n)
	}
	mark := store.SettledMark(s.Store)
	if mark == 0 {
		t.Fatal("nothing settled: the check below proves nothing")
	}
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
		{Role: "user", Speaker: "bob", Content: "q", NativeID: "n1", SpeakerID: "bob"},
		{Role: "assistant", Content: "reply on new words", NativeID: "n1", SpeakerID: "bob", SourceIDs: windowSources(t, s, key), ReadAsOf: mark},
	}, 40); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 1 {
		t.Fatalf("the reply on the new words was refused: %d kept", n)
	}
	// the crash: the same job runs again
	if err := s.processRedactions(context.Background(), db, jobs); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 1 {
		t.Fatalf("the second run of a finished edit removed the reply on the new words: %d kept", n)
	}
	var edited string
	if err := s.Store.DB.QueryRow(`SELECT content FROM chat_turns WHERE native_id='secret' AND role='user'`).Scan(&edited); err != nil || edited != "new words" {
		t.Fatalf("the edited turn after the second run: %q, %v", edited, err)
	}
	// a turn that took its mark before the second run is still excused: no new number was given out
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
		{Role: "assistant", Content: "another reply on new words", NativeID: "n2", SpeakerID: "bob", SourceIDs: windowSources(t, s, key), ReadAsOf: mark},
	}, 40); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE native_id='n2'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a turn whose mark predates the second run of a finished edit: %d rows, %v", n, err)
	}
}

// Settling and removing the job are one commit: there is no moment at which a new turn can be excused while the
// job can still run again. A removal that fails leaves nothing settled, and the next run finishes the edit.
func TestSettlingAndRemovingTheJobAreOneCommit(t *testing.T) {
	s := privacyService(t)
	db, _ := s.adminDB()
	seedEditedTurn(t, s, "discord:g:c", "secret", "alice")
	s.redaction.running.Store(true)
	defer s.redaction.running.Store(false)
	if err := s.MessageEdited("g", "c", "secret", "new words", s.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	jobs := queuedJobs(t, s)
	if len(jobs) != 1 {
		t.Fatalf("jobs: %d", len(jobs))
	}
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER test_keep_jobs BEFORE DELETE ON redaction_jobs BEGIN SELECT RAISE(ABORT,'the job cannot be removed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.processRedactions(context.Background(), db, jobs); err == nil {
		t.Fatal("a job that could not be removed was reported as finished")
	}
	if settled := settledSet(s); settled["secret"] {
		t.Fatalf("settled without the job being removed: %v", settled)
	}
	if _, err := s.Store.DB.Exec(`DROP TRIGGER test_keep_jobs`); err != nil {
		t.Fatal(err)
	}
	if err := s.processRedactions(context.Background(), db, jobs); err != nil {
		t.Fatal(err)
	}
	if settled := settledSet(s); !settled["secret"] {
		t.Fatalf("the next run did not finish the edit: %v", settled)
	}
	if left := queuedJobs(t, s); len(left) != 0 {
		t.Fatalf("a settled edit left its job in the queue: %d", len(left))
	}
}

// A job of a later revision is not a replay: the message changed again, and the job runs in full.
func TestAnEditJobOfALaterRevisionStillRuns(t *testing.T) {
	s := privacyService(t)
	seedEditedTurn(t, s, "discord:g:c", "secret", "alice")
	first := s.Clock.Now()
	if err := s.MessageEdited("g", "c", "secret", "new words", first); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := s.MemoryEngine()
	mark := store.SettledMark(s.Store)
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{
		{Role: "assistant", Content: "reply on new words", NativeID: "n1", SpeakerID: "bob", SourceIDs: windowSources(t, s, "discord:g:c"), ReadAsOf: mark},
	}, 40); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 1 {
		t.Fatalf("the reply on the new words was refused: %d kept", n)
	}
	if err := s.MessageEdited("g", "c", "secret", "newer words", first.Add(1e9)); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Fatalf("a reply on the words the second edit replaced stayed: %d kept", n)
	}
}

// Any withdrawal in the same batch used to leave every edit in it unsettled for good.
func TestAnEditInABatchWithAWithdrawalIsSettled(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	const key = "discord:g:c"
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
		{Role: "user", Speaker: "alice", Content: "old words", NativeID: "secret", SpeakerID: "alice"},
		{Role: "user", Speaker: "carol", Content: "hi", NativeID: "carol-m", SpeakerID: "carol"},
	}, 40); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(true)
	if err := s.MessageEdited("g", "c", "secret", "new words", s.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	db, _ := s.adminDB()
	if err := db.WithWriter(func(tx *sql.Tx) error { return s.withdrawalIntent(tx, "carol") }); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(false)
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settled := settledSet(s); !settled["secret"] {
		t.Fatalf("the edit processed in the same batch as a withdrawal was never settled: %v", settled)
	}
	var carol int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE speaker_id='carol'`).Scan(&carol); err != nil || carol != 0 {
		t.Fatalf("the withdrawal was not carried out: %d rows, %v", carol, err)
	}
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
		{Role: "assistant", Content: "reply on new words", NativeID: "n1", SpeakerID: "bob", SourceIDs: windowSources(t, s, key), ReadAsOf: store.SettledMark(s.Store)},
	}, 40); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 1 {
		t.Fatalf("a reply on the edited words was refused: %d kept", n)
	}
}

// An edit by the person who withdrew, in the same batch: nothing of theirs is excused, in either order of the jobs.
func TestAnEditByThePersonWithdrawingIsNotSettled(t *testing.T) {
	for _, order := range []string{"edit first", "withdrawal first"} {
		t.Run(order, func(t *testing.T) {
			s := privacyService(t)
			e, _ := s.MemoryEngine()
			if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{
				{Role: "user", Speaker: "carol", Content: "old words", NativeID: "carol-m", SpeakerID: "carol"},
			}, 40); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "captured-m", AuthorID: "carol", AuthorName: "carol", Content: "stored words of carol", At: s.Clock.Now()}); err != nil {
				t.Fatal(err)
			}
			db, _ := s.adminDB()
			withdraw := func() {
				if err := db.WithWriter(func(tx *sql.Tx) error { return s.withdrawalIntent(tx, "carol") }); err != nil {
					t.Fatal(err)
				}
			}
			edit := func() {
				for _, id := range []string{"carol-m", "captured-m"} {
					if err := s.queueSourceChange("g", "c", []string{id}, "edit", "new words", s.Clock.Now()); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.redaction.running.Store(true)
			if order == "edit first" {
				edit()
				withdraw()
			} else {
				withdraw()
				edit()
			}
			s.redaction.running.Store(false)
			if err := s.DrainRedactions(context.Background()); err != nil {
				t.Fatal(err)
			}
			if settled := settledSet(s); settled["captured-m"] {
				t.Fatalf("a stored message of a person who withdrew was settled: %v", settled)
			}
			var rows int
			if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE speaker_id='carol'`).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("%d turns of the person who withdrew stayed, %v", rows, err)
			}
			// the message's copy in a channel without capture leaves nothing to read; a late reply is refused
			if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{
				{Role: "user", Speaker: "carol", Content: "late", NativeID: "carol-m", SpeakerID: "carol"},
				{Role: "assistant", Content: "late reply", NativeID: "carol-m", SpeakerID: "carol", ReadAsOf: store.SettledMark(s.Store)},
			}, 40); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE speaker_id='carol'`).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("a late turn of the person who withdrew was kept: %d rows, %v", rows, err)
			}
		})
	}
}

// A thread starts from the parent's turns. An edited (settled) user turn is a kept turn of the parent, and so is a
// reply on its new words.
func TestAThreadSeedAfterAnEditKeepsTheEditedTurn(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	const key = "discord:g:c"
	seedEditedTurn(t, s, key, "secret", "alice")
	if err := s.MessageEdited("g", "c", "secret", "new words", s.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
		{Role: "user", Speaker: "alice", Content: "follow-up", NativeID: "n1", SpeakerID: "alice"},
		{Role: "assistant", Content: "reply on new words", NativeID: "n1", SpeakerID: "alice", SourceIDs: windowSources(t, s, key), ReadAsOf: store.SettledMark(s.Store)},
	}, 40); err != nil {
		t.Fatal(err)
	}
	s.seedThread("g", "c", "thread", "alice")
	copied := func(native, role string) (n int) {
		t.Helper()
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord:g:thread' AND native_id=? AND role=?`, native, role).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if copied("secret", "user") != 1 {
		t.Fatal("the edited (settled) user turn was not copied into the thread")
	}
	if copied("n1", "assistant") != 1 {
		t.Fatal("the reply on the new words was not copied into the thread")
	}
	var words string
	if err := s.Store.DB.QueryRow(`SELECT content FROM chat_turns WHERE channel_key='discord:g:thread' AND native_id='secret'`).Scan(&words); err != nil || words != "new words" {
		t.Fatalf("the copy holds %q, %v", words, err)
	}
	// and a change after the copy takes the copy too
	if err := s.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := copied("secret", "user") + copied("n1", "assistant"); n != 0 {
		t.Fatalf("%d copies stayed after the message was deleted", n)
	}
}

// A copy of a message deleted or under way stays refused however new the mark.
func TestAThreadSeedRefusesATurnWhoseMessageIsNotSettled(t *testing.T) {
	for _, change := range []string{"edit under way", "delete under way"} {
		t.Run(change, func(t *testing.T) {
			s := privacyService(t)
			seedEditedTurn(t, s, "discord:g:c", "secret", "alice")
			s.redaction.running.Store(true)
			defer s.redaction.running.Store(false)
			var err error
			if change == "edit under way" {
				err = s.QueueEditForTest("g", "c", "secret", "new words", s.Clock.Now())
			} else {
				err = s.queueSourceChange("g", "c", []string{"secret"}, "delete", "", s.Clock.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			s.seedThread("g", "c", "thread", "alice")
			var n int
			if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord:g:thread'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("%d turns copied while the message's change is not finished", n)
			}
		})
	}
}

// A withdrawal still waiting in the queue, in a channel without capture: no stored row says the person withdrew, but
// the account tombstone does, and an edit of theirs is not settled in the meantime.
func TestAnEditIsNotSettledWhileItsAuthorsWithdrawalWaits(t *testing.T) {
	s := privacyService(t)
	seedEditedTurn(t, s, "discord:g:c", "carol-m", "carol")
	db, _ := s.adminDB()
	s.redaction.running.Store(true)
	defer s.redaction.running.Store(false)
	if err := s.QueueEditForTest("g", "c", "carol-m", "new words", s.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithWriter(func(tx *sql.Tx) error { return s.withdrawalIntent(tx, "carol") }); err != nil {
		t.Fatal(err)
	}
	jobs := queuedJobs(t, s)
	if len(jobs) != 2 {
		t.Fatalf("jobs: %d", len(jobs))
	}
	if err := s.settleEdits(context.Background(), db, jobs[:1], jobs[:1]); err != nil {
		t.Fatal(err)
	}
	if settled := settledSet(s); len(settled) != 0 {
		t.Fatalf("settled while the author's withdrawal waits: %v", settled)
	}
}

// A copy is refused for a person who withdrew however new its mark, and stays tied to every message of a burst.
func TestAThreadSeedRefusesAPersonWhoWithdrew(t *testing.T) {
	s := privacyService(t)
	seedEditedTurn(t, s, "discord:g:c", "m", "carol")
	db, _ := s.adminDB()
	if err := db.WithWriter(func(tx *sql.Tx) error {
		return store.Tombstone(tx, "account", "carol", s.Clock.Now().Format(time.RFC3339Nano))
	}); err != nil {
		t.Fatal(err)
	}
	s.seedThread("g", "c", "thread", "carol")
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord:g:thread'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d turns of a person who withdrew were copied, %v", n, err)
	}
}

func TestAThreadCopyOfAMergedTurnGoesWithItsFirstMessage(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{
		{Role: "user", Speaker: "alice", Content: "first\nlast", NativeID: "last", SpeakerID: "alice", Merged: []string{"first"}},
		{Role: "assistant", Content: "reply", NativeID: "last", SpeakerID: "alice", Merged: []string{"first"}},
	}, 40); err != nil {
		t.Fatal(err)
	}
	s.seedThread("g", "c", "thread", "alice")
	count := func() (n int) {
		t.Helper()
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord:g:thread'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 2 {
		t.Fatalf("the merged turn was not copied: %d rows", count())
	}
	if err := s.MessagesDeleted("g", "c", []string{"first"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatalf("%d copies of the merged turn stayed after its first message was deleted", count())
	}
}

// The words of the bot's lines in the live window go where a reply can have read the changed message: its channel,
// the thread family, a channel with blocks attached. Not in the rest of the guild, and a withdrawal reaches them all.
func TestLiveBlankingStaysWithTheChannelsThatCanReadIt(t *testing.T) {
	setup := func(t *testing.T) (*Service, func(guild, channel, id string) string) {
		t.Helper()
		s := privacyService(t)
		for _, thread := range []string{"thread", "sibling"} {
			if _, err := s.Store.DB.Exec(`INSERT INTO channel_threads(thread_id,guild_id,parent_id,state,created_at,last_at) VALUES(?,'g','c','open','','')`, thread); err != nil {
				t.Fatal(err)
			}
		}
		at := s.Clock.Now().Add(-time.Hour)
		line := func(guild, channel, id string, bot bool, offset time.Duration) {
			s.noteLive(ObservedMessage{GuildID: guild, ChannelID: channel, MessageID: id, AuthorID: "someone", AuthorName: "someone", Content: "words of " + id, FromBot: bot, At: at.Add(offset)})
		}
		line("g", "c", "m", false, 0)
		line("g", "c", "bot-before", true, -time.Minute)
		line("g", "c", "bot-c", true, time.Minute)
		line("g", "thread", "bot-thread", true, time.Minute)
		line("g", "sibling", "bot-sibling", true, time.Minute)
		line("g", "other", "bot-other", true, time.Minute)
		line("g2", "c", "bot-other-guild", true, time.Minute)
		line("", "dm", "dm-m", false, 0)
		line("", "dm", "bot-dm", true, time.Minute)
		line("", "dm2", "bot-dm2", true, time.Minute)
		return s, func(guild, channel, id string) string {
			t.Helper()
			for _, l := range s.liveLines(guild, channel) {
				if l.ID == id {
					return l.Text
				}
			}
			t.Fatalf("no line %s in %s", id, channel)
			return ""
		}
	}
	for _, change := range []string{"edit", "delete"} {
		apply := func(s *Service, guild, channel string, ids ...string) {
			if change == "edit" {
				s.liveEdit(guild, channel, ids[0], "new words")
			} else {
				s.liveDelete(guild, channel, ids)
			}
		}
		t.Run(change, func(t *testing.T) {
			s, text := setup(t)
			apply(s, "g", "c", "m")
			for _, id := range []struct{ channel, line string }{{"c", "bot-c"}, {"thread", "bot-thread"}, {"sibling", "bot-sibling"}} {
				if got := text("g", id.channel, id.line); got != "" {
					t.Errorf("%s can be built on the message and kept %q", id.line, got)
				}
			}
			if text("g", "c", "bot-before") == "" {
				t.Error("a bot line older than the message lost its words")
			}
			if text("g", "other", "bot-other") == "" {
				t.Error("a channel that cannot read this one lost its bot's words")
			}
			if text("g2", "c", "bot-other-guild") == "" {
				t.Error("another guild's channel of the same name lost its bot's words")
			}
		})
		t.Run(change+"/from a thread", func(t *testing.T) {
			s, text := setup(t)
			apply(s, "g", "thread", "not-in-the-window")
			for _, id := range []struct{ channel, line string }{{"c", "bot-c"}, {"c", "bot-before"}, {"thread", "bot-thread"}, {"sibling", "bot-sibling"}} {
				if got := text("g", id.channel, id.line); got != "" {
					t.Errorf("%s can be built on the thread's message and kept %q", id.line, got)
				}
			}
			if text("g", "other", "bot-other") == "" {
				t.Error("a channel that cannot read the thread lost its bot's words")
			}
		})
		t.Run(change+"/with blocks attached", func(t *testing.T) {
			s, text := setup(t)
			raw, _ := config.Marshal(s.Doc())
			doc, err := config.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			doc.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"other": {RAGView: config.MemoryView{Blocks: []string{"some-block"}}}}}}
			raw, _ = config.Marshal(doc)
			input, _ := json.Marshal(map[string]any{"yaml": string(raw), "base_revision": s.Revision()})
			if r := s.Execute(context.Background(), action.Request{Action: "config.apply", Input: input}); !r.OK {
				t.Fatal(r.Error)
			}
			apply(s, "g", "c", "m")
			if got := text("g", "other", "bot-other"); got != "" {
				t.Errorf("a channel with blocks may read this room and kept %q", got)
			}
		})
		t.Run(change+"/channel not known", func(t *testing.T) {
			s, text := setup(t)
			apply(s, "g", "", "m")
			if got := text("g", "other", "bot-other"); got != "" {
				t.Errorf("with no channel known the whole guild is reached, and %q stayed", got)
			}
		})
		t.Run(change+"/deleted thread", func(t *testing.T) {
			s, text := setup(t)
			if _, err := s.Store.DB.Exec(`INSERT INTO thread_tombstones(thread_id) VALUES('gone-thread')`); err != nil {
				t.Fatal(err)
			}
			apply(s, "g", "gone-thread", "m")
			if got := text("g", "other", "bot-other"); got != "" {
				t.Errorf("a deleted thread's family is not known, so the guild is reached, and %q stayed", got)
			}
		})
		t.Run(change+"/direct message", func(t *testing.T) {
			s, text := setup(t)
			apply(s, "", "dm", "dm-m")
			if got := text("", "dm", "bot-dm"); got != "" {
				t.Errorf("the bot's line in the same direct message kept %q", got)
			}
			if text("", "dm2", "bot-dm2") == "" {
				t.Error("another person's direct messages lost the bot's words")
			}
		})
	}
}

// The same edit event delivered a second time (an intent replayed after a restart, another connector) has nothing to
// add: writing its tombstone again would undo the settling, the job would run again and take the replies built on
// the new words, and the new number would refuse the turns under way.
func TestTheSameEditDeliveredTwiceChangesNothing(t *testing.T) {
	s := privacyService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	const key = "discord:g:c"
	seedEditedTurn(t, s, key, "secret", "alice")
	at := s.Clock.Now()
	deliver := func(apply func() error) {
		t.Helper()
		if err := apply(); err != nil {
			t.Fatal(err)
		}
		if err := s.DrainRedactions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	edited := func() error { return s.MessageEdited("g", "c", "secret", "new words", at) }
	deliver(edited)
	mark := store.SettledMark(s.Store)
	if mark == 0 {
		t.Fatal("nothing settled")
	}
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
		{Role: "user", Speaker: "bob", Content: "q", NativeID: "n1", SpeakerID: "bob"},
		{Role: "assistant", Content: "reply on new words", NativeID: "n1", SpeakerID: "bob", SourceIDs: windowSources(t, s, key), ReadAsOf: mark},
	}, 40); err != nil {
		t.Fatal(err)
	}
	for name, again := range map[string]func() error{
		"the service": edited,
		"a replayed intent": func() error {
			return s.commitSourceChange(context.Background(), "g", "c", []string{"secret"}, "edit", "new words", at)
		},
	} {
		deliver(again)
		if n := repliesKept(t, s, "c"); n != 1 {
			t.Fatalf("%s: the reply on the new words went: %d kept", name, n)
		}
		if got := store.SettledMark(s.Store); got != mark {
			t.Fatalf("%s: the settling got a new number, %d was %d", name, got, mark)
		}
	}
	// a later edit of the same message is another event
	deliver(func() error { return s.MessageEdited("g", "c", "secret", "newer words", at.Add(time.Minute)) })
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Fatalf("a newer edit left %d replies built on the words it replaced", n)
	}
}

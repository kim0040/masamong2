package application_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/vision"
)

// What the review of postmortem 0006 asked for: everything a turn holds names where it came from, and what it read
// before its own mark (a picture, a command's result) is judged by the number taken before reading it.

// A turn with a picture: the connector takes the mark before it picks the pictures. An edit settled by then is one the
// picture list already follows, and the reply on the edited message's new words stays. An edit settled after that
// mark, or a turn with no mark at all, has no such excuse.
func TestATurnWithAPictureIsJudgedByTheMarkBeforeItsPictures(t *testing.T) {
	svc, _ := visionSvc(t)
	url, _ := picture(t)
	chainTurn(t, svc, "c", "secret", "alice")
	before := svc.SettledMark()
	changeSource(t, svc, "edit", "secret", "alice")
	turn := func(id string, from int64) string {
		t.Helper()
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "what is this?", Speaker: "bob", SpeakerID: "bob",
			ReadFrom: from, Images: []vision.Ref{{URL: url, ContentType: "image/png", MessageID: id}}})
		if err != nil {
			t.Fatal(err)
		}
		return res.TurnID
	}
	older, none := turn("older", before), turn("none", 0)
	if n := assistantTurns(t, svc, "c"); n != 0 || answerKept(t, svc, older) || answerKept(t, svc, none) {
		t.Fatalf("a turn whose pictures were picked before the edit settled was excused: %d replies kept", n)
	}
	after := turn("after", svc.SettledMark())
	if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, after) {
		t.Fatalf("a turn with a picture after a settled edit was refused: %d replies kept", n)
	}
	changeSource(t, svc, "delete", "secret", "alice")
	if n := assistantTurns(t, svc, "c"); n != 0 || answerKept(t, svc, after) {
		t.Fatalf("the reply stayed after the message was deleted: %d kept", n)
	}
}

// The picture of the edited message itself, described for a model that cannot see: the description's source is
// judged by the same mark, and the turn still goes when the message does.
func TestADescribedPictureOfAnEditedMessage(t *testing.T) {
	svc, h := visionSvc(t)
	h.blind.Store(true)
	url, _ := picture(t)
	chainTurn(t, svc, "c", "secret", "alice")
	before := svc.SettledMark()
	changeSource(t, svc, "edit", "secret", "alice")
	rows := func(id string) int {
		t.Helper()
		return chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id=?`, id)
	}
	turn := func(id string, from int64) {
		t.Helper()
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "what is that?", Speaker: "bob", SpeakerID: "bob",
			ReadFrom: from, Images: []vision.Ref{{URL: url, ContentType: "image/png", MessageID: "secret"}}}); err != nil {
			t.Fatal(err)
		}
	}
	turn("older", before)
	if n := rows("older"); n != 0 {
		t.Fatalf("a description read before the edit settled: %d rows kept", n)
	}
	turn("after", svc.SettledMark())
	if n := rows("after"); n != 2 {
		t.Fatalf("a description read after the edit settled: %d rows kept, want the turn and its reply", n)
	}
	changeSource(t, svc, "delete", "secret", "alice")
	if n := rows("after"); n != 0 {
		t.Fatalf("%d rows stayed after the described message was deleted", n)
	}
}

// A command's result: the connector takes the mark before the command runs.
func TestATurnWithAResultIsJudgedByTheMarkBeforeTheCommand(t *testing.T) {
	svc, _ := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	before := svc.SettledMark()
	changeSource(t, svc, "edit", "secret", "alice")
	turn := func(id string, from int64) string {
		t.Helper()
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "the weather?", Speaker: "bob", SpeakerID: "bob",
			ReadFrom: from, Earlier: []chat.ToolResult{{Tool: "weather.now", OK: true, Status: "ok"}}})
		if err != nil {
			t.Fatal(err)
		}
		return res.TurnID
	}
	older := turn("older", before)
	if n := assistantTurns(t, svc, "c"); n != 0 || answerKept(t, svc, older) {
		t.Fatalf("a result read before the edit settled was excused: %d replies kept", n)
	}
	after := turn("after", svc.SettledMark())
	if n := assistantTurns(t, svc, "c"); n != 1 || !answerKept(t, svc, after) {
		t.Fatalf("a result read after the edit settled was refused: %d replies kept", n)
	}
}

func keptMessage(t *testing.T, svc *application.Service, turn string) (message string, deleted bool) {
	t.Helper()
	var d int
	if err := svc.Store.DB.QueryRow(`SELECT COALESCE(json_extract(body,'$.message'),''),COALESCE(json_extract(body,'$.message_deleted'),0) FROM turn_log WHERE id=?`, turn).Scan(&message, &d); err != nil {
		t.Fatal(err)
	}
	return message, d != 0
}

func mergedTurn(t *testing.T, svc *application.Service, channel string) string {
	t.Helper()
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: channel, MessageID: "last", Fragments: []string{"first"},
		Message: "first words\nlast words", Speaker: "alice", SpeakerID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	return res.TurnID
}

// A burst answered as one turn holds the words of all its messages under the ID of the last one.
func TestAMergedTurnGoesWithAnyOfItsMessages(t *testing.T) {
	for _, change := range []string{"delete", "edit", "blank"} {
		t.Run(change, func(t *testing.T) {
			svc, _ := editSvc(t)
			earlier := chainTurn(t, svc, "c", "b0", "bob")
			merged := mergedTurn(t, svc, "c")
			built := chainTurn(t, svc, "c", "n0", "bob")
			if n := assistantTurns(t, svc, "c"); n != 3 {
				t.Fatalf("replies before the change: %d", n)
			}
			if message, _ := keptMessage(t, svc, merged); !strings.Contains(message, "first words") {
				t.Fatalf("the turn history did not hold the words: %q: this proves nothing", message)
			}
			changeSource(t, svc, change, "first", "alice")
			if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='last'`); n != 0 {
				t.Fatalf("%d rows of the merged turn stayed", n)
			}
			if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE content LIKE '%first words%'`); n != 0 {
				t.Fatalf("%d rows still hold the first message's words", n)
			}
			if n := chainCount(t, svc, `SELECT COUNT(*) FROM turn_log WHERE body LIKE '%first words%'`); n != 0 {
				t.Fatalf("%d turn records still hold the first message's words", n)
			}
			if n := chainCount(t, svc, `SELECT COUNT(*) FROM traces WHERE body LIKE '%first words%'`); n != 0 {
				t.Fatalf("%d traces still hold the first message's words", n)
			}
			if message, deleted := keptMessage(t, svc, merged); message != "" || !deleted || answerKept(t, svc, merged) {
				t.Fatalf("the merged turn's record: %q, deleted %v", message, deleted)
			}
			if answerKept(t, svc, built) {
				t.Fatal("a reply built on the merged turn stayed")
			}
			if !answerKept(t, svc, earlier) || assistantTurns(t, svc, "c") != 1 {
				t.Fatalf("the reply before the merged turn: kept %v, %d replies", answerKept(t, svc, earlier), assistantTurns(t, svc, "c"))
			}
		})
	}
}

// The last message of the burst: an edit keeps the turn with its new words, which is all that is left of the burst.
func TestAMergedTurnWithItsLastMessageChanged(t *testing.T) {
	svc, _ := editSvc(t)
	merged := mergedTurn(t, svc, "c")
	changeSource(t, svc, "edit", "last", "alice")
	var words string
	if err := svc.Store.DB.QueryRow(`SELECT content FROM chat_turns WHERE native_id='last' AND role='user'`).Scan(&words); err != nil || words != "corrected words" {
		t.Fatalf("the edited turn holds %q, %v", words, err)
	}
	if answerKept(t, svc, merged) {
		t.Fatal("the reply on the old words stayed")
	}
	changeSource(t, svc, "delete", "last", "alice")
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='last'`); n != 0 {
		t.Fatalf("%d rows stayed after the last message was deleted", n)
	}
}

// Deleted while the turn is under way: the connector read the words before any mark, and nothing excuses them.
func TestAFragmentChangedDuringTheTurn(t *testing.T) {
	for _, change := range []string{"delete", "edit"} {
		t.Run(change, func(t *testing.T) {
			svc, prompts := editSvc(t)
			prompts.whileRunning(func() {
				var err error
				if change == "delete" {
					err = svc.MessagesDeleted("g", "c", []string{"first"})
				} else {
					err = svc.MessageEdited("g", "c", "first", "corrected words", time.Now())
				}
				if err != nil {
					t.Error(err)
				}
				if err := svc.DrainRedactions(context.Background()); err != nil {
					t.Error(err)
				}
			})
			merged := mergedTurn(t, svc, "c")
			if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='last'`); n != 0 {
				t.Fatalf("%d rows of a turn whose first message changed under way were kept", n)
			}
			if message, deleted := keptMessage(t, svc, merged); message != "" || !deleted || answerKept(t, svc, merged) {
				t.Fatalf("the turn's record: %q, deleted %v", message, deleted)
			}
		})
	}
}

// A live turn's words came from the connector before its mark, whatever the mark says. An edit settled in between
// must not excuse them: the turn holds the words the edit replaced.
func TestALiveTurnOfAnEditedMessageIsNotExcused(t *testing.T) {
	svc, _ := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	changeSource(t, svc, "edit", "secret", "alice")
	if svc.SettledMark() == 0 {
		t.Fatal("nothing settled: this proves nothing")
	}
	const before = "the words before the edit"
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "secret", Message: before, Speaker: "alice", SpeakerID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns WHERE content=?`, before); n != 0 {
		t.Fatalf("%d rows hold the words the edit replaced", n)
	}
	if n := assistantTurns(t, svc, "c"); n != 0 || answerKept(t, svc, res.TurnID) {
		t.Fatalf("the reply on the words the edit replaced stayed: %d rows", n)
	}
}

// The steps for going back to v1.1.x (docs/MAINTAINING.md section 4) are what a maintainer pastes into a live database.
// They run as written, take what the old binary cannot reach, and leave the rest.
func TestTheStepsForGoingBackRunAsWritten(t *testing.T) {
	raw, err := os.ReadFile("../../docs/MAINTAINING.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, found := strings.Cut(string(raw), "v1.1.x로 되돌릴 때")
	if !found {
		t.Fatal("the section is gone")
	}
	_, block, found := strings.Cut(after, "```sql\n")
	if !found {
		t.Fatal("the section has no SQL")
	}
	steps, _, _ := strings.Cut(block, "```")
	svc, _ := editSvc(t)
	first := chainTurn(t, svc, "c", "n0", "alice")
	second := chainTurn(t, svc, "c", "n1", "alice")
	if !answerKept(t, svc, first) || !answerKept(t, svc, second) {
		t.Fatal("no answers to test with")
	}
	if chainCount(t, svc, `SELECT COUNT(*) FROM evidence_refs WHERE holder_kind='turn_ev' AND source_id GLOB 'chat:*'`) == 0 {
		t.Fatal("no turn holds a link: this proves nothing")
	}
	traces := chainCount(t, svc, `SELECT COUNT(*) FROM traces WHERE body<>'{}'`)
	if _, err := svc.Store.DB.Exec(steps); err != nil {
		t.Fatalf("the steps do not run: %v", err)
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM chat_turns`); n != 0 {
		t.Fatalf("%d turns stayed", n)
	}
	if answerKept(t, svc, second) {
		t.Fatal("the answer of a turn that holds a link stayed")
	}
	if !answerKept(t, svc, first) {
		t.Fatal("the answer of a turn with no link was blanked")
	}
	if n := chainCount(t, svc, `SELECT COUNT(*) FROM traces WHERE body<>'{}'`); traces > 0 && n >= traces {
		t.Fatalf("the trace of a turn that holds a link stayed: %d of %d", n, traces)
	}
}

func tombstonesOf(t *testing.T, svc *application.Service, id string) int {
	t.Helper()
	return chainCount(t, svc, `SELECT COUNT(*) FROM privacy_tombstones WHERE source_id=?`, id)
}

// An edit that kept the words leaves no tombstone, and is a revision all the same: an older edit arriving after
// it (out of order, or replayed after a restart) must not bring back words the newer one took out.
func TestAnOlderEditDoesNotPassAnEditThatKeptTheWords(t *testing.T) {
	content := func(t *testing.T, svc *application.Service, query, id string) string {
		t.Helper()
		var words string
		if err := svc.Store.DB.QueryRow(query, id).Scan(&words); err != nil {
			t.Fatal(err)
		}
		return words
	}
	newer := time.Now().Add(time.Hour)
	older := newer.Add(-30 * time.Minute)
	t.Run("stored", func(t *testing.T) {
		svc, _ := editSvc(t)
		chainTurn(t, svc, "c", "m", "alice")
		built := chainTurn(t, svc, "c", "n0", "bob")
		const query = `SELECT content FROM memory_observations WHERE native_message_id=?`
		words := content(t, svc, query, "m")
		if kept, err := svc.MessageKeptItsWords("g", "c", "m", words, newer); err != nil || !kept {
			t.Fatalf("kept %v, %v", kept, err)
		}
		if err := svc.MessageEdited("g", "c", "m", "words taken out later", older); err != nil {
			t.Fatal(err)
		}
		svc.DrainRedactions(t.Context())
		if got := content(t, svc, query, "m"); got != words {
			t.Fatalf("the older edit brought back %q", got)
		}
		if !answerKept(t, svc, built) {
			t.Fatal("the older edit took a reply built on the current words")
		}
		// a newer edit still goes through
		if err := svc.MessageEdited("g", "c", "m", "newest words", newer.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		svc.DrainRedactions(t.Context())
		if got := content(t, svc, query, "m"); got != "newest words" {
			t.Fatalf("a newer edit was refused: %q", got)
		}
	})
	t.Run("without capture", func(t *testing.T) {
		svc, _ := editSvc(t)
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m", Message: "the words", Speaker: "alice", SpeakerID: "alice", DryRun: false}); err != nil {
			t.Fatal(err)
		}
		const query = `SELECT content FROM chat_turns WHERE native_id=? AND role='user'`
		if kept, err := svc.MessageKeptItsWords("g", "c", "m", "the words", newer); err != nil || !kept {
			t.Fatalf("kept %v, %v", kept, err)
		}
		if err := svc.MessageEdited("g", "c", "m", "words taken out later", older); err != nil {
			t.Fatal(err)
		}
		svc.DrainRedactions(t.Context())
		if got := content(t, svc, query, "m"); got != "the words" {
			t.Fatalf("the older edit brought back %q", got)
		}
	})
}

// The words and the files stayed: what was read is what the message says, nothing built on it changes.
func TestAnEditKeepingTheWordsChangesNothing(t *testing.T) {
	keep := func(t *testing.T, svc *application.Service, id, words string, at time.Time) bool {
		t.Helper()
		kept, err := svc.MessageKeptItsWords("g", "c", id, words, at)
		if err != nil {
			t.Fatal(err)
		}
		return kept
	}
	t.Run("stored", func(t *testing.T) {
		svc, _ := editSvc(t)
		source := chainTurn(t, svc, "c", "secret", "alice")
		built := chainTurn(t, svc, "c", "n0", "bob")
		mark := svc.SettledMark()
		at := time.Now().Add(time.Minute)
		if !keep(t, svc, "secret", "message secret from alice", at) {
			t.Fatal("the same words were not recognised")
		}
		if n := assistantTurns(t, svc, "c"); n != 2 || !answerKept(t, svc, source) || !answerKept(t, svc, built) {
			t.Fatalf("an edit that kept the words took replies: %d kept", n)
		}
		if n := tombstonesOf(t, svc, "secret"); n != 0 || svc.SettledMark() != mark {
			t.Fatalf("an edit that kept the words wrote %d tombstones, mark %d was %d", n, svc.SettledMark(), mark)
		}
		if n := chainCount(t, svc, `SELECT COUNT(*) FROM redaction_jobs`); n != 0 {
			t.Fatalf("an edit that kept the words queued %d jobs", n)
		}
		stored := func() (edited time.Time) {
			t.Helper()
			var raw string
			if err := svc.Store.DB.QueryRow(`SELECT edited_at FROM memory_observations WHERE native_message_id='secret'`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			edited, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				t.Fatal(err)
			}
			return edited
		}
		if !stored().Equal(at.UTC()) {
			t.Fatalf("the revision did not move: %v, want %v", stored(), at.UTC())
		}
		// an older event of the same words moves nothing
		if !keep(t, svc, "secret", "message secret from alice", at.Add(-time.Hour)) {
			t.Fatal("an older event with the same words was not recognised")
		}
		if !stored().Equal(at.UTC()) {
			t.Fatalf("an older event moved the revision back: %v", stored())
		}
		if keep(t, svc, "secret", "other words", at.Add(time.Hour)) {
			t.Fatal("other words were taken for the same")
		}
		if n := assistantTurns(t, svc, "c"); n != 2 || tombstonesOf(t, svc, "secret") != 0 {
			t.Fatalf("a call that reports other words changed something: %d replies, %d tombstones", n, tombstonesOf(t, svc, "secret"))
		}
	})
	t.Run("without capture", func(t *testing.T) {
		svc, _ := editSvc(t)
		turn := func(id, words string, fragments ...string) {
			t.Helper()
			if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: words, Speaker: "alice", SpeakerID: "alice", Fragments: fragments}); err != nil {
				t.Fatal(err)
			}
		}
		turn("secret", "the words")
		turn("merged", "first\nlast", "first")
		replies := assistantTurns(t, svc, "c")
		if !keep(t, svc, "secret", "the words", time.Now()) {
			t.Fatal("the turn's words were not recognised")
		}
		if keep(t, svc, "secret", "other words", time.Now()) {
			t.Fatal("other words were taken for the turn's")
		}
		if keep(t, svc, "merged", "last", time.Now()) {
			t.Fatal("a message was taken as the whole merged turn it is part of")
		}
		if keep(t, svc, "first", "first", time.Now()) {
			t.Fatal("a fragment with no copy of its own was taken as compared")
		}
		if n := assistantTurns(t, svc, "c"); n != replies || tombstonesOf(t, svc, "secret") != 0 {
			t.Fatalf("%d replies, was %d; %d tombstones", n, replies, tombstonesOf(t, svc, "secret"))
		}
	})
	t.Run("in the window alone", func(t *testing.T) {
		svc, _ := editSvc(t)
		long := strings.Repeat("가", 1600)
		svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "seen", AuthorID: "alice", AuthorName: "alice", Content: "the words", At: time.Now()})
		svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "long", AuthorID: "alice", AuthorName: "alice", Content: long, At: time.Now()})
		if !keep(t, svc, "seen", "the words", time.Now()) {
			t.Fatal("the window's words were not recognised")
		}
		if keep(t, svc, "seen", "other words", time.Now()) {
			t.Fatal("other words were taken for the window's")
		}
		if keep(t, svc, "long", long, time.Now()) {
			t.Fatal("a clipped copy was taken as compared")
		}
		if keep(t, svc, "long", strings.Repeat("가", 1500)+"…", time.Now()) {
			t.Fatal("the clipped copy's own words were taken for the message's")
		}
		if keep(t, svc, "never-seen", "the words", time.Now()) {
			t.Fatal("a message with no copy was taken as compared")
		}
	})
	t.Run("with a change not finished", func(t *testing.T) {
		svc, _ := editSvc(t)
		chainTurn(t, svc, "c", "secret", "alice")
		svc.HoldRedactionForTest(true)
		if err := svc.QueueEditForTest("g", "c", "secret", "corrected words", time.Now()); err != nil {
			t.Fatal(err)
		}
		if keep(t, svc, "secret", "message secret from alice", time.Now().Add(time.Minute)) {
			t.Fatal("the stored words were taken as current while a change of them waits")
		}
		svc.HoldRedactionForTest(false)
	})
	for _, change := range []string{"delete", "withdraw"} {
		t.Run("after "+change, func(t *testing.T) {
			svc, _ := editSvc(t)
			chainTurn(t, svc, "c", "secret", "alice")
			changeSource(t, svc, change, "secret", "alice")
			if keep(t, svc, "secret", "message secret from alice", time.Now().Add(time.Minute)) {
				t.Fatalf("an edit event after the %s was taken as keeping the words", change)
			}
		})
	}
}

// A change that came in and is not in the queue yet has only its event file: these words may be what it replaces.
func TestAnEditKeepingTheWordsWaitsForAChangeNotYetInTheQueue(t *testing.T) {
	svc, _ := editSvc(t)
	chainTurn(t, svc, "c", "secret", "alice")
	if _, err := svc.Store.EnqueueEvent("source-change", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	kept, err := svc.MessageKeptItsWords("g", "c", "secret", "message secret from alice", time.Now().Add(time.Minute))
	if err != nil || kept {
		t.Fatalf("taken as keeping the words with a change not yet queued: %v, %v", kept, err)
	}
}

func captureLine(t *testing.T, svc *application.Service, id, author, words string) string {
	t.Helper()
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: author, AuthorName: author, Content: words, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: author, AuthorName: author, Content: words, At: time.Now()})
	return corpus.DiscordLiveObservationID(engine.RoomKey("g", "c"), id)
}

// answerPrompts leaves out the attribution classifier, which is offered lines and answers with labels only.
func answerPrompts(rs *recordingServer) string {
	var kept []string
	for _, p := range rs.all() {
		if !strings.Contains(p, "refers_to") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

func recentLineSvc(t *testing.T) (*application.Service, *recordingServer, *recordingServer) {
	t.Helper()
	plan, resp := &recordingServer{}, &recordingServer{}
	planURL := plan.start(t, func(string) string { return `{"memory_requirement":"none","tool_plan":[]}` })
	respURL := resp.start(t, func(string) string { return "ok" })
	return routedSvc(t, planURL, respURL), plan, resp
}

// Attribution on (the default): the recent lines of other conversations do not reach the reply, and a person's message
// nobody answered does not take the replies of the room with it. The test logs the size of the chain: the number of
// replies a deletion of such a line takes.
func TestARecentLineNotInThePromptIsNotWhatAReplyIsBuiltOn(t *testing.T) {
	const depth = 15
	svc, plan, resp := recentLineSvc(t)
	const words = "carol talks about her lunch"
	observation := captureLine(t, svc, "ambient", "carol", words)
	turns := make([]string, 0, depth)
	for i := range depth {
		turns = append(turns, chainTurn(t, svc, "c", fmt.Sprintf("n%d", i), "alice"))
	}
	if strings.Contains(answerPrompts(plan), words) || strings.Contains(answerPrompts(resp), words) {
		t.Fatal("the line reached the reply: this proves nothing")
	}
	holders := chainCount(t, svc, `SELECT COUNT(*) FROM evidence_refs WHERE holder_kind='chat' AND source_id=?`, observation)
	changeSource(t, svc, "delete", "ambient", "carol")
	lost := 0
	for _, turn := range turns {
		if !answerKept(t, svc, turn) {
			lost++
		}
	}
	t.Logf("a line that never reached a reply: linked by %d replies, its deletion took %d of %d", holders, lost, depth)
	if lost != 0 || holders != 0 {
		t.Fatalf("deleting a line that never reached a reply took %d of %d replies (%d linked to it)", lost, depth, holders)
	}
}

// The promise stays for every line the reply did read. Attribution off: the recent lines are in the prompt.
func TestARecentLineInThePromptIsWhatAReplyIsBuiltOn(t *testing.T) {
	const depth = 15
	svc, _, resp := recentLineSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Attribution: "off"}}}}
	})
	const words = "carol talks about her lunch"
	captureLine(t, svc, "ambient", "carol", words)
	turns := make([]string, 0, depth)
	for i := range depth {
		turns = append(turns, chainTurn(t, svc, "c", fmt.Sprintf("n%d", i), "alice"))
	}
	if !strings.Contains(answerPrompts(resp), words) {
		t.Fatal("the line did not reach the reply: this proves nothing")
	}
	changeSource(t, svc, "delete", "ambient", "carol")
	lost := 0
	for _, turn := range turns {
		if !answerKept(t, svc, turn) {
			lost++
		}
	}
	t.Logf("a line in the prompt: its deletion took %d of %d", lost, depth)
	if lost != depth {
		t.Fatalf("deleting a line the replies read took %d of %d replies", lost, depth)
	}
}

// Attribution on and the line in the caller's conversation: it is in the prompt through that conversation.
func TestALineOfTheCallersConversationIsWhatAReplyIsBuiltOn(t *testing.T) {
	svc, _, resp := recentLineSvc(t)
	const words = "alice said this a moment ago"
	captureLine(t, svc, "own-line", "alice", words)
	turn := chainTurn(t, svc, "c", "n0", "alice")
	if !strings.Contains(answerPrompts(resp), words) {
		t.Fatal("the caller's own line did not reach the reply: this proves nothing")
	}
	changeSource(t, svc, "delete", "own-line", "alice")
	if answerKept(t, svc, turn) || assistantTurns(t, svc, "c") != 0 {
		t.Fatalf("deleting a line of the caller's conversation left the reply: %d kept", assistantTurns(t, svc, "c"))
	}
}

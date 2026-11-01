package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/search"
)

// captureAndAnswer stores a message of alice's and records a turn of bob's that used it.
func captureAndAnswer(t *testing.T, s *Service, id, text string) string {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "alice", AuthorName: "Alice", Content: text, At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	obs := corpus.DiscordLiveObservationID("discord:g:c", id)
	turn := "turn-" + id
	s.recordTurn(TurnRecord{ID: turn, GuildID: "g", ChannelID: "c", SpeakerID: "bob", Lab: true,
		Body: map[string]any{"message": "a question", "hits": hitBrief([]search.Hit{{Rank: 1, DocID: "leaf-" + id, Excerpt: text, ObservationIDs: []string{obs}, VisibleObservationIDs: []string{obs}}})}})
	return turn
}

func turnBody(t *testing.T, s *Service, id string) string {
	t.Helper()
	var raw string
	if err := s.Store.DB.QueryRow(`SELECT body FROM turn_log WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func countRows(t *testing.T, s *Service, table string) int {
	t.Helper()
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The Discord event only queues the work: one durable row, no scan of the history, and nothing a chat
// reply waits for. The copies are redacted by the background worker.
func TestMessageEventQueuesTheRedactionAndTheWorkerFindsTheTurnsByIndex(t *testing.T) {
	s := privacyService(t)
	hit := captureAndAnswer(t, s, "m1", "private canary text of the first message")
	other := captureAndAnswer(t, s, "m2", "another message that stays")
	if n := countRows(t, s, "evidence_refs"); n == 0 {
		t.Fatal("recording a turn must index the sources it used")
	}
	s.redaction.running.Store(true) // hold the worker back: the event path alone is under test
	s.MessagesDeleted("g", "c", []string{"m1"})
	s.MessageEdited("g", "c", "never-stored", "x", time.Now())
	if n := countRows(t, s, "redaction_jobs"); n != 2 {
		t.Fatalf("each event must leave one durable job, got %d", n)
	}
	if strings.Contains(turnBody(t, s, hit), "evidence_redacted") {
		t.Fatal("the event path redacted turns itself")
	}
	s.redaction.running.Store(false)
	s.DrainRedactions(context.Background())
	if countRows(t, s, "redaction_jobs") != 0 {
		t.Fatal("a finished job must be deleted")
	}
	if !strings.Contains(turnBody(t, s, hit), "evidence_redacted") || strings.Contains(turnBody(t, s, other), "evidence_redacted") {
		t.Fatalf("exactly the turn that used the changed message is redacted:\n%s\n%s", turnBody(t, s, hit), turnBody(t, s, other))
	}
}

// A crash cannot skip a redaction: the job is a row in the database, and the next start finishes it.
func TestQueuedRedactionSurvivesARestart(t *testing.T) {
	s := privacyService(t)
	root := s.DataRoot
	turn := captureAndAnswer(t, s, "m1", "private canary text of the first message")
	s.redaction.running.Store(true)
	s.MessagesDeleted("g", "c", []string{"m1"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if strings.Contains(turnBody(t, again, turn), "evidence_redacted") {
		t.Fatal("the job should still be waiting")
	}
	again.kickRedaction() // what the daemon's scheduler does at start
	again.DrainRedactions(context.Background())
	if !strings.Contains(turnBody(t, again, turn), "evidence_redacted") || countRows(t, again, "redaction_jobs") != 0 {
		t.Fatalf("the job was lost: %s", turnBody(t, again, turn))
	}
}

// Turns written before the index existed are classified once in the background, and a job never runs
// before they are, so none can be missed.
func TestTurnsFromBeforeTheIndexAreClassifiedBeforeAnyJobRuns(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "alice", Content: "private canary text of the first message", At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	obs := corpus.DiscordLiveObservationID("discord:g:c", "m1")
	body, _ := json.Marshal(map[string]any{"message": "q", "hits": []any{map[string]any{"doc_id": "x", "observation_ids": []string{obs}}}})
	if _, err := s.Store.DB.Exec(`INSERT INTO turn_log(id,at,day,hour,guild_id,channel_id,outcome,body) VALUES('old','now','today',0,'g','c','answered',?)`, string(body)); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "evidence_refs") != 0 {
		t.Fatal("the old row has no index entries yet")
	}
	s.MessagesDeleted("g", "c", []string{"m1"})
	s.DrainRedactions(context.Background())
	if !strings.Contains(turnBody(t, s, "old"), "evidence_redacted") {
		t.Fatalf("a turn from before the index was missed: %s", turnBody(t, s, "old"))
	}
	var state int
	_ = s.Store.DB.QueryRow(`SELECT evidence_state FROM turn_log WHERE id='old'`).Scan(&state)
	if state != evidenceIndexed {
		t.Fatalf("evidence_state %d", state)
	}
}

// The answer to a deleted message loses the message's text too (the turn answered it).
func TestTurnThatAnsweredADeletedMessageLosesItsText(t *testing.T) {
	s := privacyService(t)
	s.recordTurn(TurnRecord{ID: "t1", GuildID: "g", ChannelID: "c", SpeakerID: "alice", Body: map[string]any{"message": "my address is secret street", "message_id": "m9", "answer": "ok"}})
	s.MessagesDeleted("g", "c", []string{"m9"})
	s.DrainRedactions(context.Background())
	raw := turnBody(t, s, "t1")
	if strings.Contains(raw, "secret street") || !strings.Contains(raw, `"message_deleted":true`) || !strings.Contains(raw, `"answer":"ok"`) {
		t.Fatalf("%s", raw)
	}
}

func TestTextMatcherFindsCopiesWithoutComparingEveryPair(t *testing.T) {
	var texts []string
	for i := 0; i < 3000; i++ {
		texts = append(texts, fmt.Sprintf("message number %d said something private", i))
	}
	texts = append(texts, "short", clip(strings.Repeat("long text ", 30), 80))
	m := newTextMatcher(texts)
	if !m.in(`{"excerpt":"alice: message number 1234 said something private and more"}`) {
		t.Fatal("a copied text must be found inside a longer record")
	}
	if !m.inValues(map[string]any{"a": []any{"x", "the " + strings.Repeat("long text ", 30)}}) {
		t.Fatal("a long text is matched by its first 80 characters")
	}
	if m.in(`{"excerpt":"message number 99999 said something else entirely"}`) || m.in("short") || m.in("") {
		t.Fatal("false match: short or different texts must not match")
	}
	big := strings.Repeat("an ordinary sentence in a big record ", 90)
	start := time.Now()
	for i := 0; i < 2000; i++ {
		m.in(big)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("matching 2000 records against 3000 texts took %v", d)
	}
}

// Retention deletes old turns; their entries in the evidence index go with them.
func TestRetentionDropsIndexEntriesOfDeletedTurns(t *testing.T) {
	s := privacyService(t)
	kept := captureAndAnswer(t, s, "m1", "a message the turn used")
	if _, err := s.Store.DB.Exec(`INSERT INTO evidence_refs(source_id, holder_kind, holder_id) VALUES('x','turn_ev','ghost'),('y','turn_msg','ghost')`); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, s, "evidence_refs")
	if _, err := s.runRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := countRows(t, s, "evidence_refs"); after != before-2 {
		t.Fatalf("index entries before %d, after %d: only the two of the deleted turn should go", before, after)
	}
	var n int
	_ = s.Store.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE holder_id=?`, kept).Scan(&n)
	if n == 0 {
		t.Fatal("entries of a kept turn were dropped")
	}
}

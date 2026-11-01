package application

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/memory/corpus"
)

// TestRedactionPerformance prints how long edit, delete and withdrawal events take on a large history
// (60k stored messages of which 5,000 are one heavy user's, 5k turn rows of about 3 KB). It is slow to
// build, so it runs only with MASAMONG_PERF=1:
//
//	MASAMONG_PERF=1 go test ./internal/application -run TestRedactionPerformance -v
//
// Turns are written by recordTurn like production, so they carry provenance. MASAMONG_PERF_LEGACY=1
// writes them straight into the table instead (the worker classifies them in the background first);
// MASAMONG_PERF_LEGACY=text writes rows from an older release that kept only the copied text.
func TestRedactionPerformance(t *testing.T) {
	if os.Getenv("MASAMONG_PERF") == "" {
		t.Skip("set MASAMONG_PERF=1")
	}
	s := privacyService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	const nObs, nTurns = 60000, 5000
	var obs []corpus.Observation
	for i := 0; i < nObs; i++ {
		acct := fmt.Sprintf("u%d", i%20)
		if i%12 == 0 {
			acct = "alice"
		}
		obs = append(obs, corpus.Observation{ObservationID: fmt.Sprintf("o%d", i), Platform: "discord", Collection: "g", SourceRoom: "discord:g:c", NativeMessageID: fmt.Sprintf("m%d", i),
			ActorLabel: acct, ActorAccount: acct, Content: fmt.Sprintf("this is message number %d with some words to make it longer than twelve runes", i),
			ParsedTime: time.Now().Add(-time.Duration(nObs-i) * time.Minute).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
	}
	st := time.Now()
	if _, err := e.Store.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("insert %d observations: %v", nObs, time.Since(st))
	st = time.Now()
	n, err := e.Store.SyncLeaves("discord:g:c", e.Settings.Recipe, time.Now(), nil)
	var nl int
	_ = s.Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_leaves`).Scan(&nl)
	t.Logf("leaves %+v (rows %d) err=%v: %v", n, nl, err, time.Since(st))
	big := strings.Repeat("x", 2500)
	st = time.Now()
	for i := 0; i < nTurns; i++ {
		hit := map[string]any{"doc_id": fmt.Sprintf("leaf%d", i), "observation_ids": []string{fmt.Sprintf("o%d", i)}, "pad": big}
		if os.Getenv("MASAMONG_PERF_LEGACY") == "text" {
			hit = map[string]any{"doc_id": fmt.Sprintf("leaf%d", i), "pad": big,
				"excerpt": fmt.Sprintf("u%d: this is message number %d with some words to make it longer than twelve runes", i%20, i)}
			if i%12 == 0 {
				hit["excerpt"] = fmt.Sprintf("alice: this is message number %d with some words to make it longer than twelve runes", i)
			}
		}
		if os.Getenv("MASAMONG_PERF_LEGACY") != "" {
			body, _ := json.Marshal(map[string]any{"message": fmt.Sprintf("q%d", i), "answer": "a", "hits": []any{hit}})
			if _, err := s.Store.DB.Exec(`INSERT INTO turn_log(id,at,day,hour,guild_id,channel_id,outcome,body) VALUES(?,?,?,?,?,?,?,?)`, fmt.Sprintf("t%d", i), "now", "today", 0, "g", "c", "answered", string(body)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		s.recordTurn(TurnRecord{ID: fmt.Sprintf("t%d", i), GuildID: "g", ChannelID: "c", SpeakerID: "bob", Outcome: "answered",
			Body: map[string]any{"message": fmt.Sprintf("q%d", i), "message_id": fmt.Sprintf("q%d", i), "answer": "a", "hits": []any{hit}}})
	}
	t.Logf("write %d turns: %v", nTurns, time.Since(st))
	settle := func() { s.DrainRedactions(context.Background()) }
	timeIt := func(name string, fn func()) {
		st := time.Now()
		fn()
		gateway := time.Since(st)
		settle()
		t.Logf("%-34s on the event path: %-12v until fully redacted: %v", name, gateway, time.Since(st))
	}
	timeIt("delete of a message never stored", func() { s.MessagesDeleted("g", "c", []string{"never-stored"}) })
	timeIt("delete of 3 stored messages", func() { s.MessagesDeleted("g", "c", []string{"m1", "m2", "m3"}) })
	timeIt("edit of 1 stored message", func() { s.MessageEdited("g", "c", "m5", "edited text that is long enough", time.Now()) })
	timeIt("withdrawal (5000 messages)", func() {
		r := s.Execute(context.Background(), action.Request{Action: "privacy.consent", Input: []byte(`{"user_id":"alice","scope":"memory","decision":"withdraw"}`)})
		if !r.OK {
			t.Fatal(r.Error)
		}
	})
	// the work was really done: turn t<i> used message o<i>; alice wrote every 12th message, and m1..m3 and m5 changed
	var redacted int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM turn_log WHERE body LIKE '%"evidence_redacted":true%'`).Scan(&redacted); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d turns redacted", redacted)
	if want := (nTurns+11)/12 + 4; redacted != want {
		t.Fatalf("%d turns redacted, want %d", redacted, want)
	}
}

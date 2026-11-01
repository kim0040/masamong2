package application

import (
	"context"
	"encoding/json"
	"fmt"
	"masamong/internal/action"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"strings"
	"testing"
	"time"
)

func TestFinalBLeafCopySurvivesResyncBeforeWorker(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	text := "s3cr3t"
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "alice", AuthorName: "Alice", Content: text, At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.SyncLeaves("discord:g:c", e.Settings.Recipe, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	var leaf, body string
	if err := s.Store.DB.QueryRow(`SELECT leaf_id,body FROM memory_leaves LIMIT 1`).Scan(&leaf, &body); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"doc_ids": []string{leaf}})
	input, _ := json.Marshal(map[string]any{"verdict": "good", "question": "q", "answer": text, "channel_key": "g:c", "meta": json.RawMessage(meta)})
	result := s.Execute(context.Background(), action.Request{Action: "lab.review", Input: input})
	if !result.OK {
		t.Fatal(result.Error)
	}
	reviewID := result.Output["id"].(string)
	input, _ = json.Marshal(map[string]any{"leaf_id": leaf, "kind": "correct", "note": "operator correction"})
	result = s.Execute(context.Background(), action.Request{Action: "memory.review", Input: input})
	if !result.OK {
		t.Fatal(result.Error)
	}
	items, _ := json.Marshal([]any{map[string]any{"review_id": reviewID, "question": text, "modes": map[string]any{"hybrid": map[string]any{"top": []string{leaf}}}}})
	if _, err := s.Store.DB.Exec(`INSERT INTO eval_runs(id,at,params,summary,items) VALUES('eval','now','{}','{}',?)`, string(items)); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(true)
	s.MessagesDeleted("g", "c", []string{"m1"})
	// Model the normal resync running before legacy classification / a queued batch finishes.
	if _, err := e.Store.SyncLeaves("discord:g:c", e.Settings.Recipe, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.DrainRedactions(ctx)
	var leaves, reviews, lab, jobs int
	_ = s.Store.DB.QueryRow(`SELECT count(*) FROM memory_leaves WHERE leaf_id=?`, leaf).Scan(&leaves)
	_ = s.Store.DB.QueryRow(`SELECT count(*) FROM memory_reviews WHERE leaf_id=?`, leaf).Scan(&reviews)
	_ = s.Store.DB.QueryRow(`SELECT count(*) FROM lab_reviews WHERE answer=?`, text).Scan(&lab)
	_ = s.Store.DB.QueryRow(`SELECT count(*) FROM redaction_jobs`).Scan(&jobs)
	var eval string
	_ = s.Store.DB.QueryRow(`SELECT items FROM eval_runs WHERE id='eval'`).Scan(&eval)
	t.Logf("old leaf=%d memory copies=%d lab copies=%d pending jobs=%d eval=%s", leaves, reviews, lab, jobs, eval)
	if reviews != 0 || lab != 0 || strings.Contains(eval, text) {
		t.Fatalf("source-bound private copies survived the completed redaction")
	}
}

func TestFinalBDeletedTurnWrittenAfterQueueDrained(t *testing.T) {
	s := privacyService(t)
	s.MessagesDeleted("g", "c", []string{"slow-request"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.DrainRedactions(ctx)
	// ChatTurn records after a slow model call; the Discord delete event can arrive during that call.
	s.recordTurn(TurnRecord{ID: "late", GuildID: "g", ChannelID: "c", SpeakerID: "alice", Body: map[string]any{"message_id": "slow-request", "message": "private request canary", "answer": "ok"}})
	raw := turnBody(t, s, "late")
	t.Logf("late recorded body=%s", raw)
	if strings.Contains(raw, "private request canary") {
		t.Fatal("completed job does not guard subsequent turn writes")
	}
}

func TestFinalBQueueInsertFailurePreservesSourceAndCanRetry(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	text := "a private queue failure canary"
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "alice", Content: text, At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"hits": []any{map[string]any{"excerpt": text}}})
	if _, err := s.Store.DB.Exec(`INSERT INTO turn_log(id,at,day,hour,guild_id,channel_id,outcome,body) VALUES('old','now','today',0,'g','c','answered',?)`, string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_job BEFORE INSERT ON redaction_jobs BEGIN SELECT RAISE(ABORT,'simulated queue write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.MessagesDeleted("g", "c", []string{"m1"}); err == nil {
		t.Fatal("delete must report enqueue failure")
	}
	if _, err := s.Store.DB.Exec(`DROP TRIGGER reject_job`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.DrainRedactions(ctx)
	var content string
	_ = s.Store.DB.QueryRow(`SELECT content FROM memory_observations WHERE native_message_id='m1'`).Scan(&content)
	t.Logf("source=%q legacy=%s jobs=%d", content, turnBody(t, s, "old"), countRows(t, s, "redaction_jobs"))
	if content != text || countRows(t, s, "redaction_jobs") != 0 {
		t.Fatal("failed intent must preserve source and roll back tombstones")
	}
	s.MessagesDeleted("g", "c", []string{"m1"})
	s.DrainRedactions(ctx)
	if strings.Contains(turnBody(t, s, "old"), text) {
		t.Fatal("retry must erase the preserved legacy copy")
	}
}

func TestFinalBConsentCommitCrashGap(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	text := "private withdrawal crash canary"
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "alice", Content: text, At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO lab_reviews(id,at,channel_key,question,answer,verdict,tags,note,meta) VALUES('review','now','g:c','q',?,'good','','','{}')`, text); err != nil {
		t.Fatal(err)
	}
	if _, err := s.setConsent("alice", "memory", "withdrawn", "console"); err != nil {
		t.Fatal(err)
	}
	// Exact durable boundary in privacyConsent: setConsent commits before queueWithdrawal is called.
	root := s.DataRoot
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.kickRedaction()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	again.DrainRedactions(ctx)
	var answer string
	_ = again.Store.DB.QueryRow(`SELECT answer FROM lab_reviews WHERE id='review'`).Scan(&answer)
	obs := corpus.DiscordLiveObservationID("discord:g:c", "m1")
	var content string
	_ = again.Store.DB.QueryRow(`SELECT content FROM memory_observations WHERE observation_id=?`, obs).Scan(&content)
	t.Logf("memory allowed=%v source=%q lab=%q jobs=%d", again.MemoryAllowed("alice"), content, answer, countRows(t, again, "redaction_jobs"))
	if answer == text || content != "" {
		t.Fatal("committed withdrawal must replay both source erasure and derivative cleanup")
	}
}

func TestFinalBAccountWithoutNativeIDsPerformance(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			s := privacyService(t)
			e, _ := s.MemoryEngine()
			var obs []corpus.Observation
			const total = 6000
			for i := 0; i < total; i++ {
				account := "bob"
				if i%4 == 0 {
					account = "alice"
				}
				n := ""
				if native {
					n = fmt.Sprintf("m%d", i)
				}
				obs = append(obs, corpus.Observation{ObservationID: fmt.Sprintf("o%d", i), Platform: "discord", Collection: "g", SourceRoom: "discord:g:c", NativeMessageID: n, ActorAccount: account, ActorLabel: account, Content: fmt.Sprintf("this message number %d contains personal text", i), ParsedTime: time.Now().Add(-time.Duration(total-i) * time.Minute).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
			}
			if _, err := e.Store.InsertObservations(obs, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Store.SyncLeaves("discord:g:c", e.Settings.Recipe, time.Now(), nil); err != nil {
				t.Fatal(err)
			}
			leaves := countRows(t, s, "memory_leaves")
			start := time.Now()
			revs, err := e.Store.DeleteAccountMessages("discord", "alice", time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("native=%v deleted=%d leaves=%d elapsed=%v", native, len(revs), leaves, time.Since(start))
		})
	}
}

func TestFinalBBackfillResumesBetweenBatches(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	text := "private backfill resume canary"
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "alice", Content: text, At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	obs := corpus.DiscordLiveObservationID("discord:g:c", "m1")
	for i := 0; i < 701; i++ {
		hit := map[string]any{"observation_ids": []string{obs}}
		if i%2 == 0 {
			hit = map[string]any{"excerpt": text}
		}
		b, _ := json.Marshal(map[string]any{"hits": []any{hit}})
		if _, err := s.Store.DB.Exec(`INSERT INTO turn_log(id,at,day,hour,guild_id,channel_id,outcome,body) VALUES(?,'now','today',0,'g','c','answered',?)`, fmt.Sprintf("old%04d", i), string(b)); err != nil {
			t.Fatal(err)
		}
	}
	s.redaction.running.Store(true)
	s.MessagesDeleted("g", "c", []string{"m1"})
	n, err := s.classifyTurns(context.Background(), s.Store)
	if err != nil || n != 300 {
		t.Fatalf("first batch n=%d err=%v", n, err)
	}
	root := s.DataRoot
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	again.kickRedaction()
	again.DrainRedactions(ctx)
	var unclassified, redacted int
	_ = again.Store.DB.QueryRow(`SELECT count(*) FROM turn_log WHERE evidence_state=0`).Scan(&unclassified)
	_ = again.Store.DB.QueryRow(`SELECT count(*) FROM turn_log WHERE body LIKE '%"evidence_redacted":true%'`).Scan(&redacted)
	t.Logf("after restart: unclassified=%d redacted=%d jobs=%d", unclassified, redacted, countRows(t, again, "redaction_jobs"))
	if unclassified != 0 || redacted != 701 || countRows(t, again, "redaction_jobs") != 0 {
		t.Fatal("backfill did not resume and redact both indexed and text-only turns")
	}
}

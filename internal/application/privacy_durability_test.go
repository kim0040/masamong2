package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

func privateLeaf(t *testing.T, s *Service) (string, string) {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "alice", AuthorName: "Alice", Content: "private canary", At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.SyncLeaves("discord:g:c", e.Settings.Recipe, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	var leaf string
	if err := s.Store.DB.QueryRow(`SELECT leaf_id FROM memory_leaves LIMIT 1`).Scan(&leaf); err != nil {
		t.Fatal(err)
	}
	return leaf, corpus.DiscordLiveObservationID("discord:g:c", "m1")
}

func TestAllReviewKindsIndexSourcesAtWriteTimeAndRejectLateCopies(t *testing.T) {
	s := privacyService(t)
	leaf, obs := privateLeaf(t, s)
	input, _ := json.Marshal(map[string]any{"question": "private canary?", "answer": "private canary", "verdict": "good", "meta": map[string]any{"doc_ids": []string{leaf}, "rooms": []string{"discord:g:c"}}})
	r := s.Execute(t.Context(), action.Request{Action: "lab.review", Input: input})
	if !r.OK {
		t.Fatal(r.Error)
	}
	reviewID := r.Output["id"].(string)
	mem, _ := json.Marshal(map[string]any{"leaf_id": leaf, "kind": "correct", "note": "private correction"})
	if r := s.Execute(t.Context(), action.Request{Action: "memory.review", Input: mem}); !r.OK {
		t.Fatal(r.Error)
	}
	r = s.Execute(t.Context(), action.Request{Action: "eval.run", Input: []byte(`{"modes":["hybrid"]}`)})
	if !r.OK {
		t.Fatal(r.Error)
	}
	evalID := r.Output["id"].(string)
	for kind, id := range map[string]string{holderLab: reviewID, holderReview: leaf, holderEval: evalID} {
		var n int
		if err := s.Store.DB.QueryRow(`SELECT count(*) FROM evidence_refs WHERE source_id=? AND holder_kind=? AND holder_id=?`, obs, kind, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s source refs=%d err=%v", kind, n, err)
		}
	}
	if err := s.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	s.DrainRedactions(t.Context())
	if r := s.Execute(t.Context(), action.Request{Action: "lab.review", Input: input, Locale: "ko"}); r.OK || r.Error.Message == "" {
		t.Fatal("late lab copy must be rejected with a localized explanation")
	}
	if r := s.Execute(t.Context(), action.Request{Action: "memory.review", Input: mem}); r.OK {
		t.Fatal("late memory review must be rejected")
	}
	s.recordTurn(TurnRecord{ID: "late-evidence", Body: map[string]any{"hits": []any{map[string]any{"doc_id": leaf, "excerpt": "private canary"}}}})
	if raw := turnBody(t, s, "late-evidence"); strings.Contains(raw, "private canary") || !strings.Contains(raw, "evidence_redacted") {
		t.Fatal(raw)
	}
}

func TestConsentIntentFailureRollsBackConsentAndAudit(t *testing.T) {
	s := privacyService(t)
	privateLeaf(t, s)
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_consent_job BEFORE INSERT ON redaction_jobs BEGIN SELECT RAISE(ABORT,'cannot persist intent'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.setConsent("alice", "memory", "withdrawn", "console"); err == nil {
		t.Fatal("consent must fail if durable intent fails")
	}
	for _, table := range []string{"consent_state", "consent_audit", "privacy_tombstones", "redaction_jobs"} {
		if n := countRows(t, s, table); n != 0 {
			t.Fatalf("%s was committed independently", table)
		}
	}
	if err := s.MessageEdited("g", "c", "m1", "replacement", time.Now()); err == nil {
		t.Fatal("edit must report enqueue failure")
	}
	var content string
	_ = s.Store.DB.QueryRow(`SELECT content FROM memory_observations WHERE native_message_id='m1'`).Scan(&content)
	if content != "private canary" {
		t.Fatal("failed edit changed source")
	}
}

func TestNativeLessWithdrawalCommitsBoundedProgressAndResumes(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	var obs []corpus.Observation
	for i := 0; i < 801; i++ {
		obs = append(obs, corpus.Observation{ObservationID: fmt.Sprintf("raw%04d", i), Platform: "discord", Collection: "g", SourceRoom: "discord:g:c", ActorAccount: "alice", ActorLabel: "Alice", Content: fmt.Sprintf("private canary %d", i), ParsedTime: time.Now().Add(-time.Duration(801-i) * time.Minute).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
	}
	if _, err := e.Store.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.SyncLeaves("discord:g:c", e.Settings.Recipe, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.setConsent("alice", "memory", "withdrawn", "console"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER fail_second_batch BEFORE UPDATE OF deleted_at ON memory_observations WHEN (SELECT count(*) FROM memory_observations WHERE deleted_at<>'')>=400 BEGIN SELECT RAISE(ABORT,'interrupted second batch'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.processWithdrawal(t.Context(), s.Store, redactionJob{User: "alice", Ref: personRef("alice"), At: s.nowString()}); err == nil {
		t.Fatal("fixture must interrupt withdrawal")
	}
	var erased int
	_ = s.Store.DB.QueryRow(`SELECT count(*) FROM memory_observations WHERE deleted_at<>''`).Scan(&erased)
	if erased != 400 {
		t.Fatalf("writer did not commit a bounded batch: erased=%d", erased)
	}
	if _, err := s.Store.DB.Exec(`DROP TRIGGER fail_second_batch`); err != nil {
		t.Fatal(err)
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
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := again.DrainRedactions(ctx); err != nil {
		t.Fatalf("drain redactions: %v", err)
	}
	_ = again.Store.DB.QueryRow(`SELECT count(*) FROM memory_observations WHERE content<>''`).Scan(&erased)
	if erased != 0 || countRows(t, again, "redaction_jobs") != 0 {
		t.Fatal("restart did not finish the committed withdrawal")
	}
}

func TestReviewAndEvalBackfillResumesBetweenBatches(t *testing.T) {
	s := privacyService(t)
	leaf, _ := privateLeaf(t, s)
	meta, _ := json.Marshal(map[string]any{"doc_ids": []string{leaf}})
	if err := s.Store.WithWriter(func(tx *sql.Tx) error {
		for i := 0; i < 701; i++ {
			id := fmt.Sprintf("old-review-%04d", i)
			if _, err := tx.Exec(`INSERT INTO lab_reviews(id,at,channel_key,question,answer,verdict,tags,note,meta) VALUES(?,'now','g:c','q','shrt','good','','',?)`, id, string(meta)); err != nil {
				return err
			}
			items, _ := json.Marshal([]any{map[string]any{"review_id": id, "question": "shrt"}})
			if _, err := tx.Exec(`INSERT INTO eval_runs(id,at,params,summary,items) VALUES(?,'now','{}','{}',?)`, fmt.Sprintf("old-eval-%04d", i), string(items)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(true)
	if err := s.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	e, _ := s.MemoryEngine()
	if _, err := e.Store.SyncLeaves("discord:g:c", e.Settings.Recipe, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.classifyCopies(t.Context(), s.Store); err != nil || n != 300 {
		t.Fatalf("backfill checkpoint n=%d err=%v", n, err)
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
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := again.DrainRedactions(ctx); err != nil {
		t.Fatalf("drain redactions: %v", err)
	}
	var remaining int
	_ = again.Store.DB.QueryRow(`SELECT count(*) FROM eval_runs WHERE evidence_state=0 OR items<>'[]'`).Scan(&remaining)
	if remaining != 0 || countRows(t, again, "lab_reviews") != 0 || countRows(t, again, "redaction_jobs") != 0 {
		t.Fatal("copy backfill did not resume or source membership was lost after resync")
	}
}

func TestDrainRedactionsReportsStoreFailure(t *testing.T) {
	s := privacyService(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainRedactions(context.Background()); err == nil {
		t.Fatal("drain reported success after its store closed")
	}
}

func TestTombstoneRetentionWaitsForPendingCleanup(t *testing.T) {
	s := privacyService(t)
	if _, err := s.MemoryEngine(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO privacy_tombstones VALUES('message','old-id','2000-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO redaction_jobs(payload,created_at) VALUES('not json: the event worker keeps it queued','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "privacy_tombstones") != 1 {
		t.Fatal("retention discarded the guard before committed cleanup finished")
	}
	if _, err := s.Store.DB.Exec(`DELETE FROM redaction_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "privacy_tombstones") != 0 {
		t.Fatal("expired tombstones did not leave with retention")
	}
}

func TestSummariesTracesAndConversationCopiesUseDurableSources(t *testing.T) {
	s := privacyService(t)
	leaf, obs := privateLeaf(t, s)
	e, _ := s.MemoryEngine()
	msgs, err := e.Store.RecentMessages("discord:g:c", 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages=%d err=%v", len(msgs), err)
	}
	last := msgs[0]
	if err := s.saveChannelSummary(t.Context(), s.Store, "g", "c", last, "private canary", 1, s.Clock.Now().UTC().Format(time.RFC3339Nano), msgs, false, channelSummaryState{}); err != nil {
		t.Fatal(err)
	}
	previous, _ := s.loadChannelSummary("g", "c")
	result := chat.Result{Hits: []search.Hit{{DocID: leaf, ObservationIDs: []string{obs}}}, Retrieve: map[string]any{"queries_used": []string{"private canary"}}}
	s.logTurn(ChatInput{MessageID: "bob-question", SpeakerID: "bob"}, result)
	if err := e.Store.AppendChatTurns("g:c", []store.ChatTurn{{Role: "assistant", Content: "private canary", NativeID: "bob-question", SpeakerID: "bob", SourceIDs: []string{obs}}}, 40); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"channel_summary", "trace", "chat"} {
		var n int
		if err := s.Store.DB.QueryRow(`SELECT count(*) FROM evidence_refs WHERE source_id=? AND holder_kind=?`, obs, kind).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s source index=%d err=%v", kind, n, err)
		}
	}
	s.redaction.running.Store(true)
	if err := s.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	// The queued tombstone closes the gap before source replay, while old text still exists.
	if err := s.saveChannelSummary(t.Context(), s.Store, "g", "c", last, "private canary", 1, s.Clock.Now().UTC().Format(time.RFC3339Nano), msgs, false, channelSummaryState{}); err == nil {
		t.Fatal("summary persisted after deletion intent committed")
	}
	s.redaction.running.Store(false)
	s.DrainRedactions(t.Context())
	if _, ok := s.loadChannelSummary("g", "c"); ok {
		t.Fatal("summary kept deleted source text")
	}
	var traces string
	if err := s.Store.DB.QueryRow(`SELECT body FROM traces LIMIT 1`).Scan(&traces); err != nil || traces != "{}" {
		t.Fatalf("trace=%q err=%v", traces, err)
	}
	if countRows(t, s, "chat_turns") != 0 {
		t.Fatal("assistant conversation copy survived deletion")
	}
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m2", AuthorID: "bob", AuthorName: "Bob", Content: "fresh harmless message", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := e.Store.RecentMessages("discord:g:c", 10)
	// The earlier summary used by the model disappeared, even though its fresh input remains valid.
	if err := s.saveChannelSummary(t.Context(), s.Store, "g", "c", fresh[0], "private canary and fresh harmless message", 2, s.Clock.Now().UTC().Format(time.RFC3339Nano), fresh, true, previous); err == nil {
		t.Fatal("incremental summary restored an invalidated previous summary")
	}
	s.logTurn(ChatInput{MessageID: "late-bob", SpeakerID: "bob"}, result)
	if err := e.Store.AppendChatTurns("g:c", []store.ChatTurn{{Role: "assistant", Content: "private canary", SourceIDs: []string{obs}}}, 40); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "chat_turns") != 0 {
		t.Fatal("late conversation writer restored deleted text")
	}
	rows, err := s.Store.DB.Query(`SELECT body FROM traces`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil || strings.Contains(body, "private canary") {
			t.Fatalf("late trace=%q err=%v", body, err)
		}
	}
}

func TestLegacyUnattributableCachesRetireInResumableBackfill(t *testing.T) {
	s := privacyService(t)
	if _, err := s.MemoryEngine(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO channel_summary(guild_id,channel_id,anchor_observation,anchor_time,summary,messages,updated_at) VALUES('g','c','missing','now','old private text',1,'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO traces(id,body,created_at) VALUES('old-trace','{"queries_used":["old private text"]}','now')`); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := s.classifyCopies(t.Context(), s.Store)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if countRows(t, s, "channel_summary") != 0 {
		t.Fatal("legacy summary with missing provenance remains")
	}
	var body string
	if err := s.Store.DB.QueryRow(`SELECT body FROM traces WHERE id='old-trace' AND evidence_state=1`).Scan(&body); err != nil || body != "{}" {
		t.Fatalf("legacy diagnostic=%q err=%v", body, err)
	}
}

func TestLateTurnCannotPersistAnAnswerQuotingItsDeletedRequest(t *testing.T) {
	s := privacyService(t)
	if err := s.MessagesDeleted("g", "c", []string{"never-captured"}); err != nil {
		t.Fatal(err)
	}
	s.DrainRedactions(t.Context())
	s.recordTurn(TurnRecord{ID: "late-quote", SpeakerID: "alice", Body: map[string]any{"message_id": "never-captured", "message": "shrt", "answer": "Alice said shrt", "answer_preview": "shrt"}})
	if raw := turnBody(t, s, "late-quote"); strings.Contains(raw, "shrt") {
		t.Fatal("late answer quoted a deleted request: " + raw)
	}
}

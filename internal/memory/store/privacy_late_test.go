package store

import (
	"database/sql"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

func TestLateSummaryCacheAndLeafSyncCannotRestoreDeletedText(t *testing.T) {
	st := openStore(t)
	at := time.Now().Add(-time.Hour)
	o := mk(0, at)
	if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	r := leaves.Recipe{Size: 8, Split: leaves.SplitFixed}
	if _, err := st.SyncLeaves(o.SourceRoom, r, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Messages([]string{o.SourceRoom})
	if err != nil {
		t.Fatal(err)
	}
	var leaf, body string
	if err := st.DB.DB.QueryRow(`SELECT leaf_id,body FROM memory_leaves`).Scan(&leaf, &body); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSummaryText("before", leaf, body, "private summary"); err != nil {
		t.Fatal(err)
	}
	var refs int
	_ = st.DB.DB.QueryRow(`SELECT count(*) FROM evidence_refs WHERE holder_kind='cache' AND holder_id='before' AND source_id=?`, o.ObservationID).Scan(&refs)
	if refs != 1 {
		t.Fatal("summary text cache must index its source at write time")
	}
	if _, err := st.DeleteMessages("discord", []string{o.NativeMessageID}, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSummary(leaf, "late private summary", nil, body); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSummaryText("late", leaf, body, "late cache text"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.syncLeaves(o.SourceRoom, r, time.Now(), nil, msgs); err == nil {
		t.Fatal("old leaf snapshot must be refused after deletion")
	}
	var summary string
	_ = st.DB.DB.QueryRow(`SELECT summary FROM memory_leaves WHERE leaf_id=?`, leaf).Scan(&summary)
	var caches int
	_ = st.DB.DB.QueryRow(`SELECT count(*) FROM memory_embed_cache WHERE cache_key='late'`).Scan(&caches)
	if summary != "" || caches != 0 {
		t.Fatal("late model work persisted deleted text")
	}
}

func TestLateChatAndCaptureCheckUncapturedMessageTombstones(t *testing.T) {
	st := openStore(t)
	if err := st.DB.WithWriter(func(tx *sql.Tx) error { return Tombstone(tx, "message", "never-captured", now()) }); err != nil {
		t.Fatal(err)
	}
	o := mk(0, time.Now())
	o.NativeMessageID = "never-captured"
	res, err := st.InsertObservations([]corpus.Observation{o}, nil)
	if err != nil || res.Inserted != 0 {
		t.Fatalf("late capture: %+v %v", res, err)
	}
	if err := st.AppendChatTurns("c", []ChatTurn{{Role: "user", Content: "secret", NativeID: o.NativeMessageID}, {Role: "assistant", Content: "secret echoed", NativeID: o.NativeMessageID}}, 10); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = st.DB.DB.QueryRow(`SELECT count(*) FROM chat_turns`).Scan(&n)
	if n != 0 {
		t.Fatal("late context write ignored tombstone")
	}
}

func TestLateEditCannotOverwriteANewerCommittedRevision(t *testing.T) {
	st := openStore(t)
	o := mk(0, time.Now())
	if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	older, newer := time.Now().Add(time.Second), time.Now().Add(2*time.Second)
	if _, ok, err := st.EditMessage("discord", o.NativeMessageID, "latest", newer.UTC().Format(time.RFC3339Nano)); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := st.EditMessage("discord", o.NativeMessageID, "stale", older.UTC().Format(time.RFC3339Nano)); err != nil || ok {
		t.Fatalf("late edit changed=%v err=%v", ok, err)
	}
	var text string
	_ = st.DB.DB.QueryRow(`SELECT content FROM memory_observations WHERE observation_id=?`, o.ObservationID).Scan(&text)
	if text != "latest" {
		t.Fatal(text)
	}
}

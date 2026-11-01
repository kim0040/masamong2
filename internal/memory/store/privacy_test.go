package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
)

func TestDeleteObservationsContextHonorsWithdrawalAndCancellation(t *testing.T) {
	st := openStore(t)
	o := mk(0, time.Now())
	if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	if err := st.DB.WithWriter(func(tx *sql.Tx) error {
		var err error
		blocked, err = SourceBlocked(tx, o.ObservationID)
		return err
	}); err != nil || blocked {
		t.Fatalf("active source blocked=%v err=%v", blocked, err)
	}
	if _, err := st.DB.DB.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('source',?,'2026-01-01T00:00:00Z')`, o.ObservationID); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.WithWriter(func(tx *sql.Tx) error {
		var err error
		blocked, err = SourceBlocked(tx, o.ObservationID)
		return err
	}); err != nil || !blocked {
		t.Fatalf("withdrawn source blocked=%v err=%v", blocked, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.DeleteObservationsContext(ctx, []string{o.ObservationID}, time.Now().UTC().Format(time.RFC3339Nano)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled deletion error = %v", err)
	}
	var content string
	if err := st.DB.DB.QueryRow(`SELECT content FROM memory_observations WHERE observation_id=?`, o.ObservationID).Scan(&content); err != nil || content != o.Content {
		t.Fatalf("canceled deletion changed source: %q err=%v", content, err)
	}
	revs, err := st.DeleteObservationsContext(context.Background(), []string{o.ObservationID}, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil || len(revs) != 1 || !revs[0].Deleted {
		t.Fatalf("withdrawal deletion revisions=%+v err=%v", revs, err)
	}
	if err := st.DB.DB.QueryRow(`SELECT content FROM memory_observations WHERE observation_id=?`, o.ObservationID).Scan(&content); err != nil || content != "" {
		t.Fatalf("withdrawn content remains: %q err=%v", content, err)
	}
	revs, err = st.DeleteObservationsContext(context.Background(), []string{o.ObservationID}, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil || len(revs) != 0 {
		t.Fatalf("repeated deletion was not idempotent: %+v err=%v", revs, err)
	}
}

package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

func TestLoadMessagesPreservesCorpusOrder(t *testing.T) {
	st := openStore(t)
	batch := seedMsgs(t, st, msgRoom, 12, time.Now().Add(-48*time.Hour))
	// Equal/naive/offset timestamps still use the corpus's string keys.
	for i, o := range batch {
		ts := []string{"2026-01-01T12:00:00", "2026-01-01T12:00:00+09:00", ""}[i%3]
		if _, err := st.DB.DB.Exec(`UPDATE memory_observations SET parsed_time=? WHERE observation_id=?`, ts, o.ObservationID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.Messages([]string{msgRoom})
	if err != nil {
		t.Fatal(err)
	}
	for lo, hi := 0, len(rows)-1; lo < hi; lo, hi = lo+1, hi-1 {
		rows[lo], rows[hi] = rows[hi], rows[lo]
	}
	want := corpus.OrderByRoom(rows)
	for _, capacity := range []int{0, 2, 100} {
		got, err := st.LoadMessages([]string{msgRoom}, capacity)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("capacity hint changed canonical ordering")
		}
	}
}

func TestColdLeafScanUsesReaderAndClosesOnCallbackError(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	if _, err := st.DB.DB.Exec(`UPDATE memory_leaves SET embed_space=?,leaf_vec=?`, msgSpace, []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	tx, err := st.DB.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	sentinel := errors.New("stop scan")
	done := make(chan error, 1)
	go func() {
		done <- st.ScanLeafVectorBatches([]string{msgRoom}, msgSpace, leaves.DefaultRecipe().Version(), func(rows []StoredLeaf) error {
			if len(rows) != 1 {
				return errors.New("wrong scan filter")
			}
			return sentinel
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, sentinel) {
			t.Fatalf("callback error: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = tx.Rollback()
		<-done
		t.Fatal("cold scan waited for writer")
	}
	// A failed callback must release the rows and its reader connection.
	if err := st.ScanLeafVectorBatches([]string{msgRoom}, msgSpace, leaves.DefaultRecipe().Version(), func([]StoredLeaf) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

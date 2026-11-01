package store

import (
	"bytes"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/search"
)

// A console read must not queue behind the only writer connection, even before its transaction writes.
func TestConsoleCountsUseReaderWhileWriterHeld(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	tx, err := st.DB.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	done := make(chan error, 1)
	go func() {
		if _, err := st.Counts(); err != nil {
			done <- err
			return
		}
		if _, _, err := st.SpaceCounts(msgSpace); err != nil {
			done <- err
			return
		}
		if _, err := st.EmbeddedRooms(); err != nil {
			done <- err
			return
		}
		if _, err := st.Spaces(); err != nil {
			done <- err
			return
		}
		if _, err := st.RoomSpans(); err != nil {
			done <- err
			return
		}
		_, _, err := st.SpendSince("2026-01-01")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("console counts waited for the writer connection")
		_ = tx.Rollback()
		<-done
	}
}

func TestPendingCountMatchesListAcrossUnicodeAndOverlappingLeaves(t *testing.T) {
	st := openStore(t)
	texts := []string{"abcdef", "???????", "???????", "abc??de", "abcdef\x00z", "abc def", "??????", "!!!!!!", "e\u0301e\u0301e\u0301"}
	var batch []corpus.Observation
	for i, text := range texts {
		o := mk(i, time.Now().Add(-48*time.Hour).Add(time.Duration(i)*time.Minute))
		o.Content = text
		batch = append(batch, o)
	}
	if _, err := st.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	recipe := leaves.DefaultRecipe()
	recipe.Split = leaves.SplitFixed
	recipe.Size = 4
	recipe.Overlap = 2
	if _, err := st.SyncLeaves(msgRoom, recipe, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for _, rooms := range [][]string{nil, {msgRoom}, {"absent"}} {
			pending, err := st.PendingMessages(rooms, msgSpace, 0, recipe.Version())
			if err != nil {
				t.Fatal(err)
			}
			n, err := st.CountPendingMessages(rooms, msgSpace, recipe.Version())
			if err != nil || n != len(pending) {
				t.Fatalf("count %d list %d (%v)", n, len(pending), err)
			}
		}
	}
	check()
	pending, err := st.PendingMessages(nil, msgSpace, 0, recipe.Version())
	if err != nil || len(pending) == 0 {
		t.Fatal("empty eligible fixture")
	}
	p := pending[0]
	if _, err := st.SaveMessageVectors([]MessageVector{{ObservationID: p.ObservationID, Space: msgSpace, TextHash: p.TextHash, Vec: search.EncodeQuantizedRow([]float32{1, 0})}}); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := st.DB.DB.Exec(`UPDATE memory_observations SET actor_label='renamed' WHERE observation_id=?`, p.ObservationID); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := st.DeleteObservationsContext(t.Context(), []string{p.ObservationID}, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestMessageMetadataDoesNotLoadVectorBytes(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	pending, err := st.PendingMessages(nil, msgSpace, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if _, err := st.SaveMessageVectors([]MessageVector{{ObservationID: p.ObservationID, Space: msgSpace, TextHash: p.TextHash, Vec: search.EncodeQuantizedRow([]float32{1, 0})}}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	if err := st.ScanMessageVectorMeta([]string{msgRoom}, msgSpace, func(v MessageVector) error {
		if len(v.Vec) != 0 || v.Bytes != 6 || !v.Current || v.Label == "" || v.UpdatedAt == "" {
			t.Fatal("invalid metadata scan")
		}
		n++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("metadata rows %d", n)
	}
	one, err := st.MessageVectors([]string{pending[0].ObservationID}, msgSpace)
	if err != nil || len(one) != 1 || len(one[pending[0].ObservationID].Vec) != 6 {
		t.Fatal("selective vector fetch failed")
	}
}

func TestMessageVectorSnapshotKeepsMetadataAndBytesTogether(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	pending, err := st.PendingMessages(nil, msgSpace, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := pending[0]
	old := search.EncodeQuantizedRow([]float32{1, 0})
	changed := search.EncodeQuantizedRow([]float32{0, 1})
	save := func(raw []byte) {
		t.Helper()
		if _, err := st.SaveMessageVectors([]MessageVector{{ObservationID: p.ObservationID, Space: msgSpace, TextHash: p.TextHash, Vec: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	save(old)
	if err := st.ReadMessageVectors(func(r *MessageVectorReader) error {
		var before MessageVector
		if err := r.ScanMessageVectorMeta([]string{msgRoom}, msgSpace, func(v MessageVector) error { before = v; return nil }); err != nil {
			return err
		}
		save(changed) // a writer can commit while the read-only snapshot stays open
		rows, err := r.MessageVectors([]string{p.ObservationID}, msgSpace)
		if err != nil {
			return err
		}
		got := rows[p.ObservationID]
		if !bytes.Equal(got.Vec, old) || got.UpdatedAt != before.UpdatedAt {
			t.Fatal("metadata and bytes came from different versions")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.MessageVectors([]string{p.ObservationID}, msgSpace)
	if err != nil || !bytes.Equal(rows[p.ObservationID].Vec, changed) {
		t.Fatal("next snapshot missed the committed change")
	}
}

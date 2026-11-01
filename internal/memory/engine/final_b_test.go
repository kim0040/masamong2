package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/store"
)

func TestOfflineRawAndCSVReimportsAreIdempotentInBothOrders(t *testing.T) {
	for _, rawFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("raw-first=%v", rawFirst), func(t *testing.T) {
			e, _ := newEngine(t, false)
			defer e.Close()
			p := writeCSV(t, "2026-01-01 10:00:00,Alice,yes", "2026-01-01 10:00:00,Bob,hello", "2026-01-01 10:00:00,Alice,yes")
			var records []byte
			_, _, err := corpus.ReadKakaoCSVContext(t.Context(), p, "c", "kakao:c", corpus.DefaultMarkerLocale, func(o corpus.Observation) error {
				b, err := json.Marshal(map[string]any{"observation_id": o.LegacyID, "platform": "kakao", "collection_id": "c", "source_room_id": "kakao:c", "actor": map[string]any{"observed_label": o.ActorLabel}, "content": o.Content, "time": map[string]any{"parsed": o.ParsedTime}, "record_kind": "unclassified_observation"})
				if err != nil {
					return err
				}
				records = append(records, b...)
				records = append(records, '\n')
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			raw := filepath.Join(t.TempDir(), "raw.jsonl")
			if err := os.WriteFile(raw, records, 0o600); err != nil {
				t.Fatal(err)
			}
			csvImport := func() {
				if _, err := e.ImportKakaoCSVContext(t.Context(), p, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale); err != nil {
					t.Fatal(err)
				}
			}
			rawImport := func() {
				if _, err := e.ImportRawContext(t.Context(), raw, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			if rawFirst {
				rawImport()
				csvImport()
			} else {
				csvImport()
				rawImport()
			}
			csvImport()
			rawImport()
			if n := countObservations(t, e); n != 3 {
				t.Fatalf("stored %d distinct messages instead of 3", n)
			}
		})
	}
}

func TestLegacyAliasMigrationPreservesReferencedIDs(t *testing.T) {
	e, _ := newEngine(t, false)
	defer e.Close()
	p := writeCSV(t, "2026-01-01 10:00:00,Alice,yes", "2026-01-01 10:00:00,Bob,hello", "2026-01-01 10:00:00,Alice,yes")
	var old []corpus.Observation
	_, _, err := corpus.ReadKakaoCSVContext(t.Context(), p, "c", "kakao:c", corpus.DefaultMarkerLocale, func(o corpus.Observation) error {
		o.ObservationID = o.LegacyID
		old = append(old, o)
		_, err := e.Store.DB.DB.Exec(`INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,actor_label,actor_account,content,parsed_time,record_kind,imported_at) VALUES(?,'kakao','c','kakao:c','kakao:c',?,'',?,?,?,'now')`, o.ObservationID, o.ActorLabel, o.Content, o.ParsedTime, o.RecordKind)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.DB.Exec(`DELETE FROM memory_identity_migration`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(e.Store.DB); err != nil {
		t.Fatal(err)
	}
	edited := writeCSV(t, "2026-01-01 10:00:00,Alice,yes", "2026-01-01 10:00:00,Bob,hello", "2026-01-01 10:00:00,Alice,yes", "2026-01-01 11:00:00,Bob,new")
	res, err := e.ImportKakaoCSVContext(t.Context(), edited, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale)
	if err != nil || res.Inserted != 1 {
		t.Fatalf("migration reimport %+v %v", res, err)
	}
	for _, o := range old {
		var id string
		if err := e.Store.DB.DB.QueryRow(`SELECT observation_id FROM memory_observations WHERE observation_id=?`, o.ObservationID).Scan(&id); err != nil {
			t.Fatal("migration rewrote existing observation IDs")
		}
	}
}

func TestFinalBChronologicalInterleavedIdenticalRows(t *testing.T) {
	e, _ := newEngine(t, false)
	defer e.Close()
	p := writeCSV(t, "2026-01-01 10:00:00,Alice,yes", "2026-01-01 10:00:00,Bob,hello", "2026-01-01 10:00:00,Alice,yes")
	res, err := e.ImportKakaoCSVContext(t.Context(), p, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("three chronological input rows: inserted=%d duplicate=%d stored=%d", res.Inserted, res.Duplicate, countObservations(t, e))
	if res.Inserted != 3 {
		t.Fatal("distinct messages at same timestamp are silently discarded")
	}
}

func TestKakaoOccurrencesUseTheExactRowTuple(t *testing.T) {
	p := writeCSV(t, "2026-01-01 10:00:00,Alice,yes", "2026-01-01 10:00:00,Alice, yes", "2026-01-01 10:00:00,Alice,yes")
	var rows []corpus.Observation
	_, _, err := corpus.ReadKakaoCSVContext(t.Context(), p, "c", "kakao:c", corpus.DefaultMarkerLocale, func(o corpus.Observation) error { rows = append(rows, o); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if rows[1].ObservationID != corpus.KakaoStableObservationID("kakao:c", "2026-01-01 10:00:00", "Alice", " yes", 0) || rows[2].ObservationID != corpus.KakaoStableObservationID("kakao:c", "2026-01-01 10:00:00", "Alice", "yes", 1) {
		t.Fatal("different text must not increment the same tuple's occurrence count")
	}
	e, _ := newEngine(t, false)
	defer e.Close()
	for i := 0; i < 2; i++ {
		if _, err := e.ImportKakaoCSVContext(t.Context(), p, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale); err != nil {
			t.Fatal(err)
		}
	}
	if n := countObservations(t, e); n != 3 {
		t.Fatalf("exact row identities stored=%d", n)
	}
}

func TestFinalBEditedFileWithOldStoredIDs(t *testing.T) {
	e, _ := newEngine(t, false)
	defer e.Close()
	p := writeCSV(t, "2026-01-01 10:00:00,Alice,one", "2026-01-01 10:00:01,Alice,two")
	var old []corpus.Observation
	_, _, err := corpus.ReadKakaoCSVContext(t.Context(), p, "c", "kakao:c", corpus.DefaultMarkerLocale, func(o corpus.Observation) error {
		o.ObservationID, o.LegacyID = o.LegacyID, ""
		old = append(old, o)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.InsertObservations(old, nil); err != nil {
		t.Fatal(err)
	}
	edited := writeCSV(t, "2026-01-01 10:00:00,Alice,one", "2026-01-01 10:00:01,Alice,two", "2026-01-01 10:00:02,Bob,three")
	res, err := e.ImportKakaoCSVContext(t.Context(), edited, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("edited legacy import: inserted=%d duplicate=%d stored=%d", res.Inserted, res.Duplicate, countObservations(t, e))
	if res.Inserted != 1 {
		t.Fatal("upgrade duplicates all prior rows when repairing the export")
	}
}

func TestFinalBStableCSVThenOfflineRawIDs(t *testing.T) {
	e, _ := newEngine(t, false)
	defer e.Close()
	p := writeCSV(t, "2026-01-01 10:00:00,Alice,one", "2026-01-01 10:00:01,Alice,two")
	if _, err := e.ImportKakaoCSVContext(t.Context(), p, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale); err != nil {
		t.Fatal(err)
	}
	var raw []corpus.Observation
	_, _, err := corpus.ReadKakaoCSVContext(t.Context(), p, "c", "kakao:c", corpus.DefaultMarkerLocale, func(o corpus.Observation) error {
		o.ObservationID, o.LegacyID = o.LegacyID, ""
		raw = append(raw, o)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Store.InsertObservations(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("same chat with prepare_raw IDs after CSV: inserted=%d duplicate=%d stored=%d", res.Inserted, res.Duplicate, countObservations(t, e))
	if res.Inserted != 0 {
		t.Fatal("CSV/raw deduplication is now dependent on import order")
	}
}

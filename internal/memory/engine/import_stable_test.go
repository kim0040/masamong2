package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/memory/corpus"
)

func writeCSV(t *testing.T, rows ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chat.csv")
	if err := os.WriteFile(p, []byte("Date,User,Message\n"+strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func countObservations(t *testing.T, e *Engine) int {
	t.Helper()
	var n int
	if err := e.Store.DB.DB.QueryRow(`SELECT COUNT(*) FROM memory_observations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An import that failed halfway keeps its first batches. The operator fixes the file (here: the
// bad tail is repaired and a line is added) and imports it again: the rows already stored must be
// recognised, not stored twice, although the file's bytes changed.
func TestReimportOfAnEditedFileDoesNotDuplicate(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	var rows []string
	for i := 0; i < 600; i++ {
		rows = append(rows, fmt.Sprintf("2026-01-01 10:%02d:%02d,Alice,Message %d", i/60, i%60, i))
	}
	first := writeCSV(t, rows...)
	if _, err := e.ImportKakaoCSVContext(t.Context(), first, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale); err != nil {
		t.Fatal(err)
	}
	edited := writeCSV(t, append(rows[:599], "2026-01-01 11:00:00,Bob,a repaired last line", "2026-01-01 11:00:01,Bob,and a new one")...)
	res, err := e.ImportKakaoCSVContext(t.Context(), edited, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale)
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 2 || res.Duplicate != 599 {
		t.Fatalf("edited file: %+v (want 2 new rows, 599 already stored)", res)
	}
	if n := countObservations(t, e); n != 602 {
		t.Fatalf("%d rows stored, want 602", n)
	}
}

// Identical messages at one timestamp are different messages, in the file and across imports.
func TestIdenticalRowsAtOneTimestampStayDistinct(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	p := writeCSV(t, "2026-01-01 10:00:00,Alice,ㅋㅋ", "2026-01-01 10:00:00,Alice,ㅋㅋ", "2026-01-01 10:00:00,Alice,ㅋㅋ")
	for i := 0; i < 2; i++ {
		if _, err := e.ImportKakaoCSVContext(t.Context(), p, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale); err != nil {
			t.Fatal(err)
		}
		if n := countObservations(t, e); n != 3 {
			t.Fatalf("import %d: %d rows, want 3", i+1, n)
		}
	}
}

// Rows the previous release stored have IDs that hashed the file's bytes. Importing the same file after
// the upgrade must not store them again.
func TestRowsStoredUnderTheOldIDsAreNotImportedAgain(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	p := writeCSV(t, "2026-01-01 10:00:00,Alice,one", "2026-01-01 10:00:01,Alice,two")
	var old []corpus.Observation
	if _, _, err := corpus.ReadKakaoCSVContext(t.Context(), p, "c", "kakao:c", corpus.DefaultMarkerLocale, func(o corpus.Observation) error {
		o.ObservationID, o.LegacyID = o.LegacyID, "" // what the old release stored
		old = append(old, o)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.InsertObservations(old, nil); err != nil {
		t.Fatal(err)
	}
	res, err := e.ImportKakaoCSVContext(t.Context(), p, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale)
	if err != nil || res.Inserted != 0 || res.Duplicate != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := countObservations(t, e); n != 2 {
		t.Fatalf("%d rows", n)
	}
}

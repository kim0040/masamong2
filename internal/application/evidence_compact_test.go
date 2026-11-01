package application

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"masamong/internal/chat"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

func TestEvidenceMigrationPreservesLinksAndDeduplicatesIdentifiers(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE evidence_refs(source_id TEXT NOT NULL,holder_kind TEXT NOT NULL,holder_id TEXT NOT NULL,PRIMARY KEY(source_id,holder_kind,holder_id)) WITHOUT ROWID`,
		`CREATE INDEX idx_evidence_refs_holder ON evidence_refs(holder_kind,holder_id)`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.WithWriter(func(tx *sql.Tx) error {
		for source := range 20 {
			for holder := range 30 {
				if _, err := tx.Exec(`INSERT INTO evidence_refs VALUES(?,'turn_ev',?)`, fmt.Sprintf("%064x", source), fmt.Sprintf("%064x", holder)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // migration is safe on reopen
		if err := store.MigratePrivacy(db); err != nil {
			t.Fatal(err)
		}
	}
	for table, want := range map[string]int{"evidence_refs": 600, "evidence_links": 600, "evidence_sources": 20, "evidence_holders": 30} {
		var got int
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, want %d, err=%v", table, got, want, err)
		}
	}
	var nonIntegers, legacyIndex int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM evidence_links WHERE typeof(source)<>'integer' OR typeof(holder)<>'integer'`).Scan(&nonIntegers); err != nil || nonIntegers != 0 {
		t.Fatalf("links must contain compact IDs: %d %v", nonIntegers, err)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='idx_evidence_refs_holder'`).Scan(&legacyIndex); err != nil || legacyIndex != 0 {
		t.Fatalf("legacy string index survived: %d %v", legacyIndex, err)
	}
	if _, err := db.DB.Exec(`INSERT OR IGNORE INTO evidence_refs VALUES(?,'turn_ev',?)`, fmt.Sprintf("%064x", 0), fmt.Sprintf("%064x", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`DELETE FROM evidence_refs WHERE holder_id=?`, fmt.Sprintf("%064x", 0)); err != nil {
		t.Fatal(err)
	}
	var holders int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM evidence_holders`).Scan(&holders); err != nil || holders != 29 {
		t.Fatalf("orphan holder was not reclaimed: %d %v", holders, err)
	}
	if _, err := db.DB.Exec(`DELETE FROM evidence_refs`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"evidence_links", "evidence_sources", "evidence_holders"} {
		var n int
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("empty evidence retained %s: %d %v", table, n, err)
		}
	}
}

func TestTraceSharesTurnSourcesEvenBeforeTurnCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			s := privacyService(t)
			turn := captureAndAnswer(t, s, "trace-source", "trace private source canary")
			if !committed {
				turn = "pending-turn"
			}
			obs := corpus.DiscordLiveObservationID("discord:g:c", "trace-source")
			s.logTurn(ChatInput{turnID: turn, MessageID: "question", SpeakerID: "bob"}, chat.Result{
				Hits:     []search.Hit{{DocID: "old-leaf", ObservationIDs: []string{obs}}},
				Retrieve: map[string]any{"queries_used": []string{"trace private source canary"}},
			})
			var refs, copies int
			if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE holder_kind='trace'`).Scan(&refs); err != nil || refs != 3 {
				t.Fatalf("trace duplicated its sources: %d %v", refs, err)
			}
			if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE holder_kind='trace' AND source_id=?`, obs).Scan(&copies); err != nil || copies != 0 {
				t.Fatalf("trace kept a candidate copy: %d %v", copies, err)
			}
			if err := s.MessagesDeleted("g", "c", []string{"trace-source"}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.DrainRedactions(ctx)
			var body string
			if err := s.Store.DB.QueryRow(`SELECT body FROM traces`).Scan(&body); err != nil || body != "{}" {
				t.Fatalf("trace retained a deleted dependency: %s %v", body, err)
			}
		})
	}
}

func TestRetentionRemovesCompactLinksWithExpiredDetails(t *testing.T) {
	s := privacyService(t)
	turn := captureAndAnswer(t, s, "expired", "expired private source canary")
	old := s.Clock.Now().AddDate(-2, 0, 0).UTC().Format(time.RFC3339)
	if _, err := s.Store.DB.Exec(`UPDATE turn_log SET at=? WHERE id=?`, old, turn); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE holder_id=?`, turn).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired detail links survived their retention pass: %d %v", n, err)
	}
}

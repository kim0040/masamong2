package tools

import (
	"path/filepath"
	"testing"
	"time"

	"masamong/internal/storage"
)

func openUsageTest(t *testing.T) (*storage.Store, *Usage) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u, err := OpenUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, u
}

func TestUsageCountersBumpCountsAndPruneBeforeBoundary(t *testing.T) {
	db, u := openUsageTest(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := u.Bump("2026-09-25", "provider", "openrouter", now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := u.Bump("2026-09-24", "provider", "openrouter", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	counts, err := u.Counts("2026-09-25", "provider")
	if err != nil || counts["openrouter"] != 2 || len(counts) != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	if err := u.Prune("2026-09-25"); err != nil {
		t.Fatal(err)
	}
	counts, err = u.Counts("2026-09-25", "provider")
	if err != nil || counts["openrouter"] != 2 {
		t.Fatalf("prune removed its boundary day: %v, %v", counts, err)
	}
	counts, err = u.Counts("2026-09-24", "provider")
	if err != nil || len(counts) != 0 {
		t.Fatalf("prune retained an older day: %v, %v", counts, err)
	}
	if _, err := db.DB.Exec(`CREATE TRIGGER fail_usage_bump BEFORE INSERT ON tool_usage WHEN NEW.day='2026-09-26' BEGIN SELECT RAISE(ABORT,'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := u.Bump("2026-09-26", "provider", "openrouter", now); err == nil {
		t.Fatal("Bump hid a failed write")
	}
	if _, err := db.DB.Exec(`CREATE TRIGGER fail_usage_prune BEFORE DELETE ON tool_usage BEGIN SELECT RAISE(ABORT,'injected prune failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := u.Prune("2026-09-27"); err == nil {
		t.Fatal("Prune hid a failed write")
	}
}

func TestUsageCountsReportsClosedStore(t *testing.T) {
	db, u := openUsageTest(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Counts("2026-09-25", "provider"); err == nil {
		t.Fatal("Counts succeeded after the store closed")
	}
}

func TestUsageBatchRefusalRollsBackAllCountersAndSurvivesReopen(t *testing.T) {
	db, u := openUsageTest(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	day := "2026-10-13"
	caller := Caller{UserID: "synthetic"}
	batch := []Reservation{{Tool: "dm.chat", Limits: Limits{PerUserPerDay: 3}, Caller: caller}, {Tool: "chat.respond", Limits: Limits{PerDay: 1}, Caller: caller}}
	if _, d, err := u.ReserveBatch(day, now, batch); err != nil || !d.OK {
		t.Fatal("first reservation")
	}
	if tool, d, err := u.ReserveBatch(day, now, batch); err != nil || d.OK || tool != "chat.respond" {
		t.Fatal("second quota must refuse")
	}
	counts, err := u.Today(day)
	if err != nil || counts["dm.chat"] != 1 || counts["chat.respond"] != 1 {
		t.Fatal("refusal consumed quota")
	}
	restarted, err := OpenUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := restarted.Reserve(day, now, "dm.chat", Limits{PerUserPerDay: 1}, caller); err != nil || d.OK {
		t.Fatal("reopened usage lost quota")
	}
	if d, err := restarted.Reserve("2026-10-14", now.Add(24*time.Hour), "dm.chat", Limits{PerUserPerDay: 1}, caller); err != nil || !d.OK {
		t.Fatal("next day did not reset")
	}
}

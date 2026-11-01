package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/errclass"
)

type movableClock struct{ t time.Time }

func (c *movableClock) Now() time.Time { return c.t }

func deliveryService(t *testing.T) (*Service, *movableClock) {
	t.Helper()
	s := privacyService(t)
	clk := &movableClock{t: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
	s.Clock = clk
	return s, clk
}

func queuedAlerts(t *testing.T, s *Service) []alertEvent {
	t.Helper()
	var q []alertEvent
	if _, err := s.kvGet("alerts:queue", &q); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestNoteDeliveryStoresOneRowPerDelivery(t *testing.T) {
	s, clk := deliveryService(t)
	s.NoteDelivery("turn-1", "g1", "c1", DeliveryReport{Parts: 3, Sent: 2, Retries: 2, Class: errclass.Overloaded, MessageIDs: []string{"m1", "m2"}})
	s.NoteDelivery("turn-2", "g1", "c1", DeliveryReport{Parts: 1, Sent: 1, ReferenceDropped: true})
	s.NoteDelivery("", "g1", "c1", DeliveryReport{Parts: 1, Sent: 1}) // no turn: nothing to tie it to
	s.NoteDelivery("turn-3", "g1", "c1", DeliveryReport{})            // nothing was to be sent
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM delivery_ledger`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d, %v", n, err)
	}
	var guild, channel, class, at string
	var parts, sent, retries, dropped int
	err = db.DB.QueryRow(`SELECT guild_id, channel_id, parts, sent, retries, class, reference_dropped, created_at FROM delivery_ledger WHERE turn_id='turn-1'`).
		Scan(&guild, &channel, &parts, &sent, &retries, &class, &dropped, &at)
	if err != nil {
		t.Fatal(err)
	}
	if guild != "g1" || channel != "c1" || parts != 3 || sent != 2 || retries != 2 || class != "overloaded" || dropped != 0 || at != clk.t.Format(time.RFC3339) {
		t.Fatalf("row = %s %s %d %d %d %s %d %s", guild, channel, parts, sent, retries, class, dropped, at)
	}
	if err := db.DB.QueryRow(`SELECT reference_dropped FROM delivery_ledger WHERE turn_id='turn-2'`).Scan(&dropped); err != nil || dropped != 1 {
		t.Fatalf("reference_dropped = %d, %v", dropped, err)
	}
	var idx string
	if err := db.DB.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_delivery_ledger_turn'`).Scan(&idx); err != nil {
		t.Fatalf("turn index missing: %v", err)
	}
}

func TestDeliveryLedgerKeepsNinetyDays(t *testing.T) {
	s, clk := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	insert := func(turn string, age time.Duration) {
		t.Helper()
		if _, err := db.DB.Exec(`INSERT INTO delivery_ledger(turn_id, guild_id, channel_id, parts, sent, retries, class, reference_dropped, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			turn, "g", "c", 1, 1, 0, "", 0, clk.t.Add(-age).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	insert("old", 91*24*time.Hour)
	insert("recent", 89*24*time.Hour)
	removed, err := s.runRetention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed["delivery_ledger"] != 1 {
		t.Fatalf("removed = %v", removed)
	}
	var turns []string
	rows, err := db.DB.Query(`SELECT turn_id FROM delivery_ledger`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		turns = append(turns, id)
	}
	if len(turns) != 1 || turns[0] != "recent" {
		t.Fatalf("kept = %v", turns)
	}
}

func TestNoteDeliveryProblemAlertsPermissionOncePerChannelPerSixHours(t *testing.T) {
	s, clk := deliveryService(t)
	s.NoteDeliveryProblem("g1", "c1", errclass.Overloaded) // not something an operator can fix
	s.NoteDeliveryProblem("g1", "", errclass.Permission)
	if q := queuedAlerts(t, s); len(q) != 0 {
		t.Fatalf("queued without a permission problem: %+v", q)
	}
	s.NoteDeliveryProblem("g1", "c1", errclass.Permission)
	s.NoteDeliveryProblem("g1", "c1", errclass.Permission) // same channel, same hour
	q := queuedAlerts(t, s)
	if len(q) != 1 || q[0].Key != alertDeliveryPerm || q[0].Args["channel_id"] != "c1" {
		t.Fatalf("queue = %+v", q)
	}
	s.NoteDeliveryProblem("g1", "c2", errclass.Permission) // another channel has its own window
	if q := queuedAlerts(t, s); len(q) != 2 {
		t.Fatalf("queue = %+v", q)
	}
	clk.t = clk.t.Add(5*time.Hour + 59*time.Minute)
	s.NoteDeliveryProblem("g1", "c1", errclass.Permission)
	if q := queuedAlerts(t, s); len(q) != 2 {
		t.Fatalf("alerted again inside six hours: %+v", q)
	}
	clk.t = clk.t.Add(2 * time.Minute)
	s.NoteDeliveryProblem("g1", "c1", errclass.Permission)
	if q := queuedAlerts(t, s); len(q) != 3 {
		t.Fatalf("no alert after six hours: %+v", q)
	}
}

func TestDeliveryPermissionAlertTextInEveryLanguage(t *testing.T) {
	for _, locale := range []string{"ko", "en", "ja", "zh-Hans", "es"} {
		got := alertText(locale, alertDeliveryPerm, map[string]any{"channel_id": "12345", "guild_id": "7"})
		if got == "" || got == "attention."+alertDeliveryPerm || !strings.Contains(got, "<#12345>") {
			t.Errorf("%s: alert text = %q", locale, got)
		}
	}
}

// Conversation lines kept for a reminder that is still waiting do not outlive their age cap.
func TestRetentionDropsOldReminderContextLines(t *testing.T) {
	s, clk := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO reminders(id, status, owner_id, content, timezone, created_at, updated_at) VALUES('r1','scheduled','u1','x','UTC','','')`); err != nil {
		t.Fatalf("minimal reminder row not accepted: %v", err)
	}
	for i, age := range []time.Duration{31 * 24 * time.Hour, 2 * 24 * time.Hour} {
		if _, err := db.DB.Exec(`INSERT INTO reminder_context(reminder_id, seq, text, at) VALUES('r1',?,?,?)`, i, "line", clk.t.Add(-age).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.runRetention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.DB.QueryRow(`SELECT COUNT(*) FROM reminder_context`).Scan(&n)
	if removed["reminder_context_old"] != 1 || n != 1 {
		t.Fatalf("removed %v, %d lines kept", removed, n)
	}
}

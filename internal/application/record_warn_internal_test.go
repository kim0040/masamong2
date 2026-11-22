package application

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
)

// A record write the database refuses warns — losing history must be visible — but at most once a
// minute, so a broken writer cannot fill the log with one line per turn.
func TestRecordWriteFailureWarnsOnceAMinute(t *testing.T) {
	s, clk := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`DROP TABLE conversation_log`); err != nil {
		t.Fatal(err)
	}
	d := convDecision{Guild: "g", Channel: "c", Message: "m1", Author: "u1", Kind: convNameCall, Decision: "about_bot"}
	s.logConversation(d)
	s.logConversation(d)
	if n := strings.Count(logs.String(), "conversation log write failed"); n != 1 {
		t.Fatalf("warnings = %d, want 1\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("failure not at WARN level\n%s", logs.String())
	}
	clk.t = clk.t.Add(61 * time.Second)
	s.logConversation(d)
	if !strings.Contains(logs.String(), "suppressed=1") {
		t.Fatalf("suppressed count missing\n%s", logs.String())
	}
}

// The ledger's own flush takes the same path: each refused row warns through the throttle, not at
// debug level.
func TestLedgerRowFailureWarnsOnce(t *testing.T) {
	s, _ := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	s.ledger.flush(s, []ledgerRow{
		{query: `INSERT INTO missing_table(x) VALUES(1)`},
		{query: `INSERT INTO missing_table(x) VALUES(1)`},
	})
	if n := strings.Count(logs.String(), "call ledger row failed"); n != 1 {
		t.Fatalf("warnings = %d, want 1\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("failure not at WARN level\n%s", logs.String())
	}
}

// breakingPoster posts fine but breaks the outcome write, the way a database going away between the
// send and the status update does.
type breakingPoster struct {
	drop  func()
	posts int
}

func (p *breakingPoster) Connected() bool                          { return true }
func (p *breakingPoster) BotUser() (string, string)                { return "b", "bot" }
func (p *breakingPoster) Guilds() []GuildInfo                      { return nil }
func (p *breakingPoster) Channels(string) []ChannelInfo            { return nil }
func (p *breakingPoster) Permissions(string, string) (int64, bool) { return 0, false }
func (p *breakingPoster) PostChannel(context.Context, string, string, string, *action.Card) (string, error) {
	p.posts++
	if p.drop != nil {
		p.drop()
	}
	return "msg-1", nil
}

// A reminder that was sent but whose final status write fails warns instead of staying invisible:
// the row is left in 'sending', which the next start recovers as 'unknown'.
func TestReminderStatusWriteFailureWarns(t *testing.T) {
	s, clk := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	due := clk.t.Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := db.DB.Exec(`INSERT INTO reminders(id,owner_id,owner_name,guild_id,channel_id,destination,content,timezone,locale,due_at,status,version,created_at,updated_at)
		VALUES('r1','u1','U','g1','c1','channel','call mom','UTC','en',?,'scheduled',1,'','')`, due); err != nil {
		t.Fatal(err)
	}
	rows, err := s.listReminders(`id='r1'`, nil, 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("due list: %v %d", err, len(rows))
	}
	p := &breakingPoster{drop: func() { _, _ = db.DB.Exec(`DROP TABLE reminders`) }}
	s.Discord = p
	s.deliverRows(context.Background(), rows, clk.t)
	if p.posts != 1 {
		t.Fatalf("posts = %d", p.posts)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "reminders: sent mark failed") {
		t.Fatalf("no warning for the refused status write\n%s", logs.String())
	}
}

// The same pass with a healthy database marks the reminder sent and warns nothing.
func TestReminderStatusWriteRecorded(t *testing.T) {
	s, clk := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	due := clk.t.Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := db.DB.Exec(`INSERT INTO reminders(id,owner_id,owner_name,guild_id,channel_id,destination,content,timezone,locale,due_at,status,version,created_at,updated_at)
		VALUES('r1','u1','U','g1','c1','channel','call mom','UTC','en',?,'scheduled',1,'','')`, due); err != nil {
		t.Fatal(err)
	}
	rows, err := s.listReminders(`id='r1'`, nil, 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("due list: %v %d", err, len(rows))
	}
	s.Discord = &breakingPoster{}
	s.deliverRows(context.Background(), rows, clk.t)
	var status, delivered string
	if err := db.DB.QueryRow(`SELECT status, delivered_id FROM reminders WHERE id='r1'`).Scan(&status, &delivered); err != nil {
		t.Fatal(err)
	}
	if status != "sent" || delivered != "msg-1" {
		t.Fatalf("status = %s delivered = %s", status, delivered)
	}
	if strings.Contains(logs.String(), "mark failed") {
		t.Fatalf("unexpected warning\n%s", logs.String())
	}
}

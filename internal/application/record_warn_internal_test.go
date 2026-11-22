package application

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	featreminders "masamong/internal/features/reminders"
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
	s.SetDiscord(p)
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
	s.SetDiscord(&breakingPoster{})
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

// reminderLive (the SQL condition) and cancellable (the Go predicate) are two names for the same
// thing: a reminder that is still to come. This pins them equal for every status and both series
// shapes, so one cannot drift from the other.
func TestReminderLiveMatchesCancellable(t *testing.T) {
	s, clk := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	created := clk.t.UTC().Format(time.RFC3339)
	statuses := []string{"proposed", "scheduled", "paused", "sending", "sent", "failed", "missed", "cancelled", "unknown"}
	for _, repeat := range []bool{false, true} {
		for _, st := range statuses {
			id, rule := "r-"+st, ""
			var rep *featreminders.Rule
			if repeat {
				id, rule = id+"-rep", `{}` // any non-empty rule: only repeat_rule<>'' / Repeat!=nil matters
				rep = &featreminders.Rule{}
			}
			if _, err := db.DB.Exec(`INSERT INTO reminders(id,owner_id,content,timezone,status,repeat_rule,created_at,updated_at)
				VALUES(?,'u1','x','UTC',?,?,?,?)`, id, st, rule, created, created); err != nil {
				t.Fatal(err)
			}
			var live int
			if err := db.DB.QueryRow(`SELECT COUNT(*) FROM reminders WHERE id=? AND `+reminderLive, id).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if (live == 1) != cancellable(reminder{Status: st, Repeat: rep}) {
				t.Fatalf("status %s repeat %v: SQL live=%d, Go cancellable=%v", st, repeat, live, cancellable(reminder{Status: st, Repeat: rep}))
			}
		}
	}
}

package application

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
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

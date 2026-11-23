package application

import (
	"context"
	"testing"
	"time"

	"masamong/internal/action"
)

// A withdrawal of memory consent removes the person's activity counts (review H6 item 1): the table
// keeps the raw account id and display name, and only age removed them before.
func TestWithdrawalRemovesActivityCounts(t *testing.T) {
	t.Parallel()
	s := privacyService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stampNow := time.Now().UTC().Format(time.RFC3339)
	for _, user := range []string{"leaver", "stayer"} {
		for _, day := range []string{"2026-11-20", "2026-11-21"} {
			if _, err := s.Store.DB.Exec(`INSERT INTO activity_counts(day, guild_id, channel_id, user_id, name, n, last_at) VALUES(?, 'g', 'c', ?, ?, 3, ?)`,
				day, user, "name-"+user, stampNow); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(user string) int {
		t.Helper()
		var n int
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM activity_counts WHERE user_id=?`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("leaver") != 2 || count("stayer") != 2 {
		t.Fatal("the rows the withdrawal must remove are not there to begin with")
	}
	r := s.Execute(ctx, action.Request{Action: "privacy.consent", Input: []byte(`{"scope":"memory","decision":"withdraw"}`), Locale: "en",
		Invoker: &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "leaver"}})
	if !r.OK {
		t.Fatalf("withdraw: %+v", r.Error)
	}
	if err := s.DrainRedactions(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count("leaver"); n != 0 {
		t.Fatalf("%d activity rows of the withdrawn person remain", n)
	}
	if n := count("stayer"); n != 2 {
		t.Fatalf("another person's activity rows went too: %d left", n)
	}
}

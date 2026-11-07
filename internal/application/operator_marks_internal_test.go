package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"masamong/internal/action"
)

func markCall(s *Service, name string, input map[string]any) action.Result {
	b, _ := json.Marshal(input)
	return s.Execute(context.Background(), action.Request{Action: name, Input: b, Locale: "en"})
}

// markableTurn records a turn that answered with a tool; the speaker is alice.
func markableTurn(s *Service, id, messageID string, tools ...string) {
	markableTurnBy(s, "alice", id, messageID, tools...)
}

func markableTurnBy(s *Service, speaker, id, messageID string, tools ...string) {
	s.recordTurn(TurnRecord{ID: id, GuildID: "g", ChannelID: "c", SpeakerID: speaker, Outcome: "answered", Tools: tools,
		Body: map[string]any{"message": "q " + id, "message_id": messageID, "answer": "a " + id, "answer_preview": "a " + id}})
}

func markOf(t *testing.T, s *Service, id string) (string, string) {
	t.Helper()
	r := markCall(s, "turns.get", map[string]any{"id": id})
	if !r.OK {
		t.Fatalf("turns.get %s: %+v", id, r.Error)
	}
	raw, _ := json.Marshal(r.Output["turn"])
	var turn struct {
		Mark        string `json:"mark"`
		MarkBlocked string `json:"mark_blocked"`
	}
	_ = json.Unmarshal(raw, &turn)
	return turn.Mark, turn.MarkBlocked
}

// The operator's verdict is one row per turn in turn_feedback (source "operator"), changed in place and
// taken back with "clear". It never counts as a rating by people: the summary, the turn's good/bad
// counts and the implicit signals stay what they were.
func TestOperatorMarkIsStoredApartFromRatings(t *testing.T) {
	s := privacyService(t)
	markableTurn(s, "t1", "m1", "search.web")
	s.putFeedback("t1", "u1", "menu", -1, "wrong") // a person's bad rating
	s.putFeedback("t1", "u2", "menu", 1, "")       // and a good one
	for _, c := range []struct {
		verdict string
		rating  int
	}{{"wrong", -1}, {"stale", -2}, {"right", 1}} {
		if r := markCall(s, "turns.mark", map[string]any{"id": "t1", "verdict": c.verdict}); !r.OK {
			t.Fatalf("mark %s: %+v", c.verdict, r.Error)
		}
		var n, rating int
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(rating),0) FROM turn_feedback WHERE turn_id='t1' AND source='operator' AND user_hash='operator'`).Scan(&n, &rating); err != nil || n != 1 || rating != c.rating {
			t.Fatalf("%s: rows=%d rating=%d err=%v", c.verdict, n, rating, err)
		}
	}
	if mark, _ := markOf(t, s, "t1"); mark != "right" {
		t.Fatalf("mark %q", mark)
	}
	// people's ratings are untouched by the operator row
	sum := markCall(s, "feedback.summary", nil)
	if sum.Output["good"] != 1 || sum.Output["bad"] != 1 {
		t.Fatalf("summary counted the operator: %v", sum.Output)
	}
	turn := markCall(s, "turns.get", map[string]any{"id": "t1"}).Output["turn"].(TurnRecord)
	if turn.Feedback["good"] != 1 || turn.Feedback["bad"] != 1 || len(turn.Implicit) != 0 {
		t.Fatalf("turn feedback: %v %v", turn.Feedback, turn.Implicit)
	}
	listed := markCall(s, "turns.list", nil).Output["turns"].([]TurnRecord)
	if len(listed) != 1 || listed[0].Mark != "right" || listed[0].Feedback["bad"] != 1 {
		t.Fatalf("list: %+v", listed)
	}
	// clearing, a bad verdict and an unknown turn
	if r := markCall(s, "turns.mark", map[string]any{"id": "t1", "verdict": "clear"}); !r.OK {
		t.Fatal(r.Error)
	}
	if n := countRows(t, s, "turn_feedback WHERE source='operator'"); n != 0 {
		t.Fatalf("clear left %d rows", n)
	}
	if r := markCall(s, "turns.mark", map[string]any{"id": "t1", "verdict": "maybe"}); r.OK {
		t.Fatal("an unknown verdict was accepted")
	}
	if r := markCall(s, "turns.mark", map[string]any{"id": "nope", "verdict": "right"}); r.OK || r.Error.Args["reason"] != "gone" {
		t.Fatalf("unknown turn: %+v", r)
	}
}

// A turn whose message was deleted, whose speaker withdrew memory consent, whose detail aged out, whose
// channel keeps no text or that has no reply cannot be marked: there is nothing left to judge. A mark made
// earlier stays and still counts; taking it back stays possible.
func TestOperatorMarkNeedsTheTurnToStillShowWhatWasSaid(t *testing.T) {
	s := privacyService(t)
	markableTurnBy(s, "alice", "deleted", "m-deleted", "search.web")
	markableTurnBy(s, "bob", "withdrawn", "m-withdrawn", "search.web")
	markableTurnBy(s, "alice", "expired", "m-expired", "search.web")
	s.recordTurn(TurnRecord{ID: "free", GuildID: "g", ChannelID: "c", SpeakerID: "carol", Outcome: "answered", Tools: []string{"search.web"}, Body: map[string]any{"body_free": "metadata", "answer": "a"}})
	s.recordTurn(TurnRecord{ID: "silent", GuildID: "g", ChannelID: "c", SpeakerID: "carol", Outcome: "silent", Body: map[string]any{"message": "q"}})
	s.recordTurn(TurnRecord{ID: "fine", GuildID: "g", ChannelID: "c", SpeakerID: "dave", Outcome: "answered", Tools: []string{"search.web"}, Body: map[string]any{"message": "q", "answer": "a"}})
	for _, id := range []string{"deleted", "withdrawn", "expired"} {
		if r := markCall(s, "turns.mark", map[string]any{"id": id, "verdict": "wrong"}); !r.OK {
			t.Fatalf("before: %s %+v", id, r.Error)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.MessagesDeleted("g", "c", []string{"m-deleted"}); err != nil {
		t.Fatal(err)
	}
	s.DrainRedactions(ctx)
	if err := s.forgetPerson(ctx, "bob"); err != nil { // what a withdrawal of memory consent does to the console's records
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`UPDATE turn_log SET body='{}' WHERE id='expired'`); err != nil {
		t.Fatal(err)
	}
	for id, why := range map[string]string{"deleted": "deleted", "withdrawn": "deleted", "expired": "expired", "free": "no_text", "silent": "no_answer"} {
		mark, blocked := markOf(t, s, id)
		if blocked != why {
			t.Errorf("%s: blocked %q, want %q", id, blocked, why)
		}
		if id == "deleted" && mark != "wrong" {
			t.Errorf("a mark made earlier stays: %q", mark)
		}
		r := markCall(s, "turns.mark", map[string]any{"id": id, "verdict": "right"})
		if r.OK || r.Error.Args["reason"] != why {
			t.Errorf("%s: marking was allowed or gave %+v", id, r.Error)
		}
	}
	if _, blocked := markOf(t, s, "fine"); blocked != "" {
		t.Fatalf("a turn with its text and reply is markable: %q", blocked)
	}
	if r := markCall(s, "turns.mark", map[string]any{"id": "fine", "verdict": "stale"}); !r.OK {
		t.Fatalf("fine: %+v", r.Error)
	}
	totals := markCall(s, "turns.marks", nil).Output["total"].(map[string]int)
	if totals["wrong"] != 3 || totals["stale"] != 1 {
		t.Fatalf("earlier marks still count: %v", totals)
	}
	if r := markCall(s, "turns.mark", map[string]any{"id": "deleted", "verdict": "clear"}); !r.OK {
		t.Fatalf("clear on a deleted turn: %+v", r.Error)
	}
	if n := countRows(t, s, "turn_feedback WHERE turn_id='deleted'"); n != 0 {
		t.Fatalf("cleared mark left %d rows", n)
	}
}

// The draw holds only search turns that can still be marked and have not been, every turn once, as many as
// asked for (30 by default); the filter on tool turns leaves out turns that used no tool.
func TestTurnSampleDrawsMarkableSearchTurns(t *testing.T) {
	s := privacyService(t)
	for i := 0; i < 40; i++ {
		markableTurn(s, fmt.Sprintf("s%02d", i), fmt.Sprintf("m%02d", i), "search.web")
	}
	markableTurn(s, "web", "mw", "weather.query", "web.read")
	markableTurn(s, "weather", "mq", "weather.query")
	markableTurn(s, "plain", "mp")
	s.recordTurn(TurnRecord{ID: "gone", GuildID: "g", ChannelID: "c", Outcome: "answered", Tools: []string{"search.web"}, Body: map[string]any{}})
	if r := markCall(s, "turns.mark", map[string]any{"id": "s00", "verdict": "right"}); !r.OK {
		t.Fatal(r.Error)
	}
	draw := func(in map[string]any) ([]string, int) {
		r := markCall(s, "turns.sample", in)
		if !r.OK {
			t.Fatal(r.Error)
		}
		var ids []string
		for _, tr := range r.Output["turns"].([]TurnRecord) {
			ids = append(ids, tr.ID)
		}
		return ids, r.Output["pool"].(int)
	}
	ids, pool := draw(nil)
	if len(ids) != sampleDefault || pool != 40 { // 40 search turns and the web.read one, minus the marked one (the empty one is not a candidate)
		t.Fatalf("drew %d of a pool of %d", len(ids), pool)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] || id == "s00" || id == "weather" || id == "plain" || id == "gone" {
			t.Fatalf("unexpected or repeated turn %s in %v", id, ids)
		}
		seen[id] = true
	}
	differs := false
	for i := 0; i < 8 && !differs; i++ {
		other, _ := draw(nil)
		for j := range other {
			differs = differs || other[j] != ids[j]
		}
	}
	if !differs {
		t.Fatal("eight draws in a row were identical")
	}
	if all, _ := draw(map[string]any{"n": 100}); len(all) != 40 {
		t.Fatalf("n=100 drew %d", len(all))
	}
	if again, _ := draw(map[string]any{"n": 100, "include_marked": true}); len(again) != 41 {
		t.Fatalf("with marked turns: %d", len(again))
	}
	// the list's tool filter
	listed := markCall(s, "turns.list", map[string]any{"tools_only": true, "limit": 500}).Output["turns"].([]TurnRecord)
	if len(listed) != 43 { // 40 + web + weather + gone
		t.Fatalf("tool turns: %d", len(listed))
	}
}

// Totals by verdict and by tool: a turn that used two tools counts under both, once in the total; only the
// operator's marks, only within the period.
func TestTurnMarksTotalByVerdictAndTool(t *testing.T) {
	s, clk := deliveryService(t)
	markableTurn(s, "a", "ma", "search.web")
	markableTurn(s, "b", "mb", "search.web", "weather.query")
	markableTurn(s, "c", "mc", "weather.query")
	markableTurn(s, "d", "md")
	s.putFeedback("a", "u1", "menu", -1, "") // a person's bad rating is not a mark
	for id, v := range map[string]string{"a": "right", "b": "wrong", "c": "stale", "d": "right"} {
		if r := markCall(s, "turns.mark", map[string]any{"id": id, "verdict": v}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	r := markCall(s, "turns.marks", map[string]any{"days": 7})
	total := r.Output["total"].(map[string]int)
	if total["right"] != 2 || total["wrong"] != 1 || total["stale"] != 1 {
		t.Fatalf("total %v", total)
	}
	got := map[string][3]int{}
	for _, row := range r.Output["tools"].([]map[string]any) {
		got[row["tool"].(string)] = [3]int{row["right"].(int), row["wrong"].(int), row["stale"].(int)}
	}
	if len(got) != 2 || got["search.web"] != [3]int{1, 1, 0} || got["weather.query"] != [3]int{0, 1, 1} {
		t.Fatalf("by tool %v", got)
	}
	clk.t = clk.t.Add(10 * 24 * time.Hour)
	if total := markCall(s, "turns.marks", map[string]any{"days": 7}).Output["total"].(map[string]int); total["right"] != 0 {
		t.Fatalf("marks older than the period counted: %v", total)
	}
}

// Marks leave with the turn records (retention.turn_days), stay with a turn whose person withdrew (they hold
// no text and no account), go into the backup with the rest of the database and are gone after a reset.
func TestOperatorMarksFollowRetentionBackupAndReset(t *testing.T) {
	t.Run("retention", func(t *testing.T) {
		s, clk := deliveryService(t)
		markableTurn(s, "old", "m1", "search.web")
		if r := markCall(s, "turns.mark", map[string]any{"id": "old", "verdict": "right"}); !r.OK {
			t.Fatal(r.Error)
		}
		clk.t = clk.t.Add(3650 * 24 * time.Hour)
		markableTurn(s, "new", "m2", "search.web")
		if r := markCall(s, "turns.mark", map[string]any{"id": "new", "verdict": "wrong"}); !r.OK {
			t.Fatal(r.Error)
		}
		if n := countRows(t, s, "turn_feedback WHERE source='operator'"); n != 2 {
			t.Fatalf("before: %d", n)
		}
		if _, err := s.runRetention(context.Background()); err != nil {
			t.Fatal(err)
		}
		var kept string
		if err := s.Store.DB.QueryRow(`SELECT turn_id FROM turn_feedback WHERE source='operator'`).Scan(&kept); err != nil || kept != "new" {
			t.Fatalf("kept %q (%v), want only the recent mark", kept, err)
		}
		if n := countRows(t, s, "turn_log"); n != 1 {
			t.Fatalf("turns kept: %d", n)
		}
	})
	t.Run("backup and reset", func(t *testing.T) {
		s := maintenanceService(t, false)
		markableTurn(s, "t1", "m1", "search.web")
		if r := markCall(s, "turns.mark", map[string]any{"id": "t1", "verdict": "stale"}); !r.OK {
			t.Fatal(r.Error)
		}
		name, _, err := s.createBackup(backupManual)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := sql.Open("sqlite", filepath.Join(s.backupDir(), name, "masamong.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer snap.Close()
		var rating int
		if err := snap.QueryRow(`SELECT rating FROM turn_feedback WHERE turn_id='t1' AND source='operator'`).Scan(&rating); err != nil || rating != markRatings["stale"] {
			t.Fatalf("the backup holds the mark: rating=%d err=%v", rating, err)
		}
		maintenanceAction(t, s, "instance.reset", map[string]any{"confirm": "reset", "backup": false, "delete_keys": false})
		root := s.DataRoot
		_ = s.Close()
		if err := RecoverMaintenance(root, slog.Default()); err != nil {
			t.Fatal(err)
		}
		again, err := Open(root, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		defer again.Close()
		if r := again.Execute(context.Background(), action.Request{Action: "setup.resume", Input: json.RawMessage(`{"instance":"after-reset"}`), Locale: "en"}); !r.OK {
			t.Fatal(r.Error)
		}
		if n := countRows(t, again, "turn_feedback") + countRows(t, again, "turn_log"); n != 0 {
			t.Fatalf("%d rows survived the reset", n)
		}
	})
}

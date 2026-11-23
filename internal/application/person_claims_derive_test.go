package application_test

import (
	"context"
	"testing"
	"time"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// A derived statement lives as long as its item: a hidden item hides it at once (read gate) and the pass
// deletes it; an item made again over the same lines (a regenerated episode) carries it on with its state.
func TestPersonClaimFollowsItsItem(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ctx := context.Background()
	f.source(t, "g", "c", "item-label", "101", "민수", f.at)
	ev := f.source(t, "g", "c", "item-line", "101", "민수", f.at)
	f.fact(t, "g", "c", "item-marker", "101", "101", ev)
	if n := personClaimCount(t, s, "source='episode_item:item-marker'"); n != 1 {
		t.Fatalf("derived: %d", n)
	}
	// regenerated: a new item over the same line, then the old one hidden
	f.fact(t, "g", "c", "item-marker-2", "101", "101", ev)
	if _, err := s.Store.DB.Exec(`UPDATE episode_item SET state='hidden' WHERE id='item-marker'`); err != nil {
		t.Fatal(err)
	}
	s.PeoplePassForTest(ctx)
	if n := personClaimCount(t, s, "source='episode_item:item-marker-2'"); n != 1 || personClaimCount(t, s, "1") != 1 {
		t.Fatalf("the statement did not move to the regenerated item: %d of %d", n, personClaimCount(t, s, "1"))
	}
	// hidden for good: not shown before the pass, gone after it
	if _, err := s.Store.DB.Exec(`UPDATE episode_item SET state='hidden' WHERE id='item-marker-2'`); err != nil {
		t.Fatal(err)
	}
	if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); got != "" {
		t.Fatalf("a hidden item's statement was shown: %q", got)
	}
	s.PeoplePassForTest(ctx)
	if n := personClaimCount(t, s, "1"); n != 0 {
		t.Fatalf("statements of hidden items: %d", n)
	}
}

// The relation fill recomputes the mention counts of a server once from the stored messages and replaces
// what was counted before (never adds), leaving out people who withdrew profile or were forgotten.
func TestPersonRelationsFillReplaces(t *testing.T) {
	t.Parallel()
	s, _, e := memorySvc(t)
	ctx := context.Background()
	at := time.Now().Add(-3 * time.Hour)
	say := func(id, author, text string, when time.Time) {
		t.Helper()
		if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: author, AuthorName: "n" + author, Content: text, At: when}); err != nil {
			t.Fatal(err)
		}
	}
	say("r1", "101", "<@202> 안녕", at)
	say("r2", "101", "<@202> 뭐해", at.Add(time.Minute))
	say("r3", "202", "<@101> 나 여기", at.Add(2*time.Minute))
	say("r4", "303", "<@202> 반가워", at.Add(3*time.Minute))
	a, b := store.PersonRef("101"), store.PersonRef("202")
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO person_relation(guild_id,a_ref,b_ref,replies,mentions,updated_at) VALUES('g',?,?,4,50,'2026-01-01')`, lo, hi); err != nil {
		t.Fatal(err)
	}
	if r := as(s, "privacy.consent", map[string]any{"scope": "profile", "decision": "withdraw"}, inChannel("303")); !r.OK {
		t.Fatal(r.Error)
	}
	s.RunRoomMemory(ctx)
	var replies, mentions, others int
	if err := s.Store.DB.QueryRow(`SELECT replies,mentions FROM person_relation WHERE guild_id='g' AND a_ref=? AND b_ref=?`, lo, hi).Scan(&replies, &mentions); err != nil {
		t.Fatal(err)
	}
	if replies != 4 || mentions != 3 {
		t.Fatalf("recomputed counts: replies %d (kept), mentions %d (want 3, replaced)", replies, mentions)
	}
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM person_relation WHERE a_ref=? OR b_ref=?`, store.PersonRef("303"), store.PersonRef("303")).Scan(&others); err != nil || others != 0 {
		t.Fatalf("a profile-withdrawn person's pairs: %d (%v)", others, err)
	}
	s.RunRoomMemory(ctx)
	if err := s.Store.DB.QueryRow(`SELECT mentions FROM person_relation WHERE guild_id='g' AND a_ref=? AND b_ref=?`, lo, hi).Scan(&mentions); err != nil || mentions != 3 {
		t.Fatalf("a second pass added to the fill: %d (%v)", mentions, err)
	}
}

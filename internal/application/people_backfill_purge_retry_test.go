package application

import (
	"slices"
	"strings"
	"testing"

	"masamong/internal/features/privacy"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
)

// pbFailWindowWrites makes every write of a stored pilot answer fail until the returned function runs.
func pbFailWindowWrites(t *testing.T, s *Service) func() {
	t.Helper()
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER pb_window_fault BEFORE UPDATE OF result ON people_backfill_window
		BEGIN SELECT RAISE(ABORT,'synthetic pilot write failure'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := s.Store.DB.Exec(`DROP TRIGGER pb_window_fault`); err != nil {
			t.Fatal(err)
		}
	}
}

// pbPurgeFlags counts the flags of a pilot purge left to run.
func pbPurgeFlags(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM admin_state WHERE substr(k,1,?)=?`, len(pbPurgePendingPrefix), pbPurgePendingPrefix).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pbReviewShows reports whether the pilot review shows an item whose text holds sub; ok is false when the
// review refused.
func pbReviewShows(t *testing.T, s *Service, sub string) (shown, ok bool) {
	t.Helper()
	res, out := pbExec(t, s, "memory.people.backfill.review", map[string]any{"guild_id": "g1"})
	if !res.OK {
		return false, false
	}
	items, _ := out["items"].([]any)
	for _, it := range items {
		if text, _ := it.(map[string]any)["text"].(string); strings.Contains(text, sub) {
			return true, true
		}
	}
	return false, true
}

// A profile withdrawal whose pilot purge fails (every write of a stored answer is refused) still commits,
// and the purge is flagged, not lost. While the fault holds, the review refuses rather than show the copy.
// Once it is gone the person grants profile consent again: the flagged purge runs first, while they are
// still withdrawn, so the fact they said stays out of the stored answers and the review after the
// re-grant. The control: another person's fact stays.
func TestFailedPilotPurgeIsRetriedBeforeRegrant(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	_, run := pbPiloted(t, s, nil)
	mina, joon := store.PersonRef("101"), store.PersonRef("102")
	var target, control episodes.PersonFact
	for _, f := range pbStoredFacts(t, s, run.ID) {
		switch {
		case target.Text == "" && slices.Equal(f.By, []string{joon}) && f.About != joon && strings.Contains(f.Text, " drinks "):
			target = f
		case control.Text == "" && f.About == mina && slices.Equal(f.By, []string{mina}) && !pbNames(f, joon):
			control = f
		}
	}
	if target.Text == "" || control.Text == "" {
		t.Fatal("the stored pilot answers lack the facts the test needs")
	}
	topic := target.Topic
	if shown, ok := pbReviewShows(t, s, topic); !ok || !shown {
		t.Fatalf("control: the review does not show the fact to start with (ok=%v)", ok)
	}
	heal := pbFailWindowWrites(t, s)
	if _, err := s.setConsent("102", privacy.Profile, privacy.Withdrawn, "test"); err != nil {
		t.Fatalf("a failing pilot purge held back the withdrawal: %v", err)
	}
	if !pbHasFact(t, s, run.ID, target.Text) {
		t.Fatal("control: the fault did not hold (the copy was purged at the withdrawal)")
	}
	if pbPurgeFlags(t, s) == 0 {
		t.Error("the failed purge left no flag")
	}
	if _, ok := pbReviewShows(t, s, topic); ok {
		t.Error("the review answered while the flagged purge could not run")
	}
	heal()
	if _, err := s.setConsent("102", privacy.Profile, privacy.Granted, "test"); err != nil {
		t.Fatal(err)
	}
	if pbHasFact(t, s, run.ID, target.Text) {
		t.Errorf("after the re-grant the stored pilot answers hold the fact the withdrawal should have purged: %q", target.Text)
	}
	if shown, ok := pbReviewShows(t, s, topic); !ok || shown {
		t.Errorf("after the re-grant the review shows the purged fact (ok=%v)", ok)
	}
	if !pbHasFact(t, s, run.ID, control.Text) {
		t.Error("control: another person's fact went with the purge")
	}
	if n := pbPurgeFlags(t, s); n != 0 {
		t.Errorf("the flag stays after its purge ran: %d", n)
	}
}

// A member's "that's wrong" or item forget commits while every write of a stored pilot answer fails: the
// item is hidden or deleted, its statements go and the markers are written; the pilot purge is flagged.
// Once the fault is gone, the review runs the flagged purge first: the fact is out of the stored answers.
func TestCorrectionAndForgetCommitUnderPilotFault(t *testing.T) {
	mina := store.PersonRef("101")
	for _, how := range []string{"wrong", "forget"} {
		t.Run(how, func(t *testing.T) {
			h := newPBHost(t)
			s := pbService(t, h, "anthropic")
			pbSeed(t, s, 70)
			_, run := pbPiloted(t, s, nil)
			var target episodes.PersonFact
			for _, f := range pbStoredFacts(t, s, run.ID) {
				if f.About == mina && slices.Equal(f.By, []string{mina}) && strings.Contains(f.Text, " likes ") {
					target = f
					break
				}
			}
			if target.Text == "" {
				t.Fatal("the stored pilot answers lack the fact the test needs")
			}
			pbItemFor(t, s, "pb-item", target)
			if _, err := s.DerivePersonItemForTest(t.Context(), "pb-item"); err != nil {
				t.Fatal(err)
			}
			items := s.myItems(s.Store, "g1", mina, []string{"pb-item"}, 1)
			if len(items) != 1 {
				t.Fatalf("the item is not Mina's: %+v", items)
			}
			heal := pbFailWindowWrites(t, s)
			if how == "wrong" {
				if n, err := s.markWrong(t.Context(), s.Store, items); err != nil || n != 1 {
					t.Fatalf("a pilot write fault held back the correction: %d %v", n, err)
				}
			} else {
				p := s.forgetPreview(s.Store, "g1", mina, []string{"pb-item"})
				if p.err != nil || p.count != 1 {
					t.Fatalf("preview: %+v", p)
				}
				if n, err := s.applyForget(t.Context(), s.Store, "g1", mina, p, store.SuppressBySelf); err != nil || n != 1 {
					t.Fatalf("a pilot write fault held back the forget: %d %v", n, err)
				}
			}
			if n := pbCountClaims(t, s, `source='episode_item:pb-item' AND state<>?`, store.PersonHidden); n != 0 {
				t.Errorf("the item's statement is still active: %d", n)
			}
			var marks int
			if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM person_memory_suppress WHERE person_ref=? AND scope LIKE 'line:%'`, mina).Scan(&marks); err != nil || marks == 0 {
				t.Errorf("the correction's markers did not commit: %d %v", marks, err)
			}
			if !pbHasFact(t, s, run.ID, target.Text) {
				t.Fatal("control: the fault did not hold")
			}
			if pbPurgeFlags(t, s) == 0 {
				t.Fatal("the failed purge left no flag")
			}
			heal()
			if _, ok := pbReviewShows(t, s, target.Topic); !ok {
				t.Fatal("the review refused after the fault was gone")
			}
			if pbHasFact(t, s, run.ID, target.Text) {
				t.Errorf("the flagged purge did not run before the review: %q", target.Text)
			}
			if n := pbPurgeFlags(t, s); n != 0 {
				t.Errorf("the flag stays after its purge ran: %d", n)
			}
		})
	}
}

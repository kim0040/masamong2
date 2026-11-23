package application

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"masamong/internal/features/privacy"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
)

// The stored pilot answers of the paid backfill are a copy of what people said: a member's forget-all and a
// withdrawal of memory consent purge that copy in the same transaction, through the real paths.
func TestPeopleBackfillPilotPurgeIsWired(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	_, run := pbPiloted(t, s, nil)
	mina, joon := store.PersonRef("101"), store.PersonRef("102")
	has := func(ref string) bool {
		for _, f := range pbStoredFacts(t, s, run.ID) {
			if pbNames(f, ref) {
				return true
			}
		}
		return false
	}
	if !has(joon) || !has(mina) {
		t.Fatal("the stored pilot answers have nothing of Mina and Joon to start with")
	}
	if err := s.Store.WithWriter(func(tx *sql.Tx) error {
		_, err := s.forgetPersonTx(tx, "g1", joon, "self")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if has(joon) {
		t.Fatal("a forget-all left the person's statements in the stored pilot answers")
	}
	if _, err := s.setConsent("101", privacy.Memory, privacy.Withdrawn, "test"); err != nil {
		t.Fatal(err)
	}
	if has(mina) {
		t.Fatal("a memory withdrawal left the person's statements in the stored pilot answers")
	}
}

// pbItemFor saves an episode item of server g1 that says what a stored pilot fact says (its text, its
// lines, its speaker), as the room-memory extraction would have.
func pbItemFor(t *testing.T, s *Service, id string, f episodes.PersonFact) {
	t.Helper()
	ev, _ := json.Marshal(f.Ev)
	by, _ := json.Marshal(f.By)
	room := engine.RoomKey("g1", "c1")
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.Store.DB.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at)
		VALUES(?,'g1','c1',?,'event',?,'done','discord',?,?,?)`, id, room, id, at, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at) VALUES(?,?,'fact',?,?,?,?)`,
		id, id, f.Text, string(ev), string(by), at); err != nil {
		t.Fatal(err)
	}
}

// pbHasFact reports whether the stored pilot answers of a run hold a fact with this text.
func pbHasFact(t *testing.T, s *Service, runID, text string) bool {
	t.Helper()
	for _, f := range pbStoredFacts(t, s, runID) {
		if f.Text == text {
			return true
		}
	}
	return false
}

// The stored pilot answers are a copy of what people said. A member's "that's wrong" or item forget, and a
// withdrawal of profile consent by the person who said something, purge that copy in the same transaction
// through the real paths; a fact the change does not touch stays.
func TestTargetedCorrectionAndForgetPurgePilotCopy(t *testing.T) {
	mina, joon := store.PersonRef("101"), store.PersonRef("102")
	// a pilot fact of Mina about herself, and an unrelated one of Joon about himself
	pick := func(t *testing.T, s *Service, runID string) (target, control episodes.PersonFact) {
		t.Helper()
		for _, f := range pbStoredFacts(t, s, runID) {
			switch {
			case target.Text == "" && f.About == mina && slices.Equal(f.By, []string{mina}) && strings.Contains(f.Text, " likes "):
				target = f
			case control.Text == "" && f.About == joon && slices.Equal(f.By, []string{joon}) && strings.Contains(f.Text, " likes "):
				control = f
			}
		}
		if target.Text == "" || control.Text == "" {
			t.Fatal("the stored pilot answers lack the facts the test needs")
		}
		return target, control
	}
	for _, how := range []string{"wrong", "forget"} {
		t.Run(how, func(t *testing.T) {
			h := newPBHost(t)
			s := pbService(t, h, "anthropic")
			pbSeed(t, s, 70)
			_, run := pbPiloted(t, s, nil)
			target, control := pick(t, s, run.ID)
			pbItemFor(t, s, "pb-item", target)
			items := s.myItems(s.Store, "g1", mina, []string{"pb-item"}, 1)
			if len(items) != 1 {
				t.Fatalf("the item is not Mina's: %+v", items)
			}
			if how == "wrong" {
				if n, err := s.markWrong(t.Context(), s.Store, items); err != nil || n != 1 {
					t.Fatalf("wrong: %d %v", n, err)
				}
			} else {
				p := s.forgetPreview(s.Store, "g1", mina, []string{"pb-item"})
				if p.err != nil || p.count != 1 {
					t.Fatalf("preview: %+v", p)
				}
				if n, err := s.applyForget(t.Context(), s.Store, "g1", mina, p, store.SuppressBySelf); err != nil || n != 1 {
					t.Fatalf("forget: %d %v", n, err)
				}
			}
			if pbHasFact(t, s, run.ID, target.Text) {
				t.Fatalf("the pilot copy keeps the statement the member %s", map[string]string{"wrong": "said is wrong", "forget": "forgot"}[how])
			}
			if !pbHasFact(t, s, run.ID, control.Text) {
				t.Fatal("control: an unrelated pilot fact went with it")
			}
		})
	}
	t.Run("speaker withdrawal", func(t *testing.T) {
		h := newPBHost(t)
		s := pbService(t, h, "anthropic")
		pbSeed(t, s, 70)
		_, run := pbPiloted(t, s, nil)
		_, control := pick(t, s, run.ID)
		said := false
		for _, f := range pbStoredFacts(t, s, run.ID) {
			said = said || (slices.Contains(f.By, joon) && f.About != joon)
		}
		if !said {
			t.Fatal("no pilot fact of Joon about someone else to start with")
		}
		// Joon withdraws his profile: his lines leave every prompt, so what he said about others goes too
		if _, err := s.setConsent("102", privacy.Profile, privacy.Withdrawn, "test"); err != nil {
			t.Fatal(err)
		}
		for _, f := range pbStoredFacts(t, s, run.ID) {
			if pbNames(f, joon) {
				t.Fatalf("a fact said by or about the withdrawn speaker stays in the pilot answers: %+v", f)
			}
		}
		if pbHasFact(t, s, run.ID, control.Text) {
			t.Fatal("Joon's own control fact should be gone with his profile")
		}
		left := 0
		for _, f := range pbStoredFacts(t, s, run.ID) {
			if f.About == mina && slices.Equal(f.By, []string{mina}) {
				left++
			}
		}
		if left == 0 {
			t.Fatal("control: Mina's own facts went with Joon's withdrawal")
		}
	})
}

// The pilot purge after a member's "that's wrong" or item forget follows the line rule of the claims:
// a stored pilot fact about the member resting on the item's lines goes even when it says the fact in
// other words than the item. A fact of the member on other lines, and one of someone else, stay.
func TestCorrectionPurgesPilotFactsOfItsLines(t *testing.T) {
	mina, joon := store.PersonRef("101"), store.PersonRef("102")
	for _, how := range []string{"wrong", "forget"} {
		t.Run(how, func(t *testing.T) {
			h := newPBHost(t)
			s := pbService(t, h, "anthropic")
			pbSeed(t, s, 70)
			_, run := pbPiloted(t, s, nil)
			var target, minaOther, control episodes.PersonFact
			for _, f := range pbStoredFacts(t, s, run.ID) {
				own := f.About == mina && slices.Equal(f.By, []string{mina}) && strings.Contains(f.Text, " likes ")
				switch {
				case target.Text == "" && own:
					target = f
				case minaOther.Text == "" && own && target.Text != "" && !slices.ContainsFunc(f.Ev, func(id string) bool { return slices.Contains(target.Ev, id) }):
					minaOther = f
				case control.Text == "" && f.About == joon && slices.Equal(f.By, []string{joon}) && strings.Contains(f.Text, " likes "):
					control = f
				}
			}
			if target.Text == "" || minaOther.Text == "" || control.Text == "" {
				t.Fatal("the stored pilot answers lack the facts the test needs")
			}
			item := target
			item.Text = "{@" + mina + "}는 그것의 열렬한 팬임" // the same lines, other words
			pbItemFor(t, s, "pb-item", item)
			items := s.myItems(s.Store, "g1", mina, []string{"pb-item"}, 1)
			if len(items) != 1 {
				t.Fatalf("the item is not Mina's: %+v", items)
			}
			if how == "wrong" {
				if n, err := s.markWrong(t.Context(), s.Store, items); err != nil || n != 1 {
					t.Fatalf("wrong: %d %v", n, err)
				}
			} else {
				p := s.forgetPreview(s.Store, "g1", mina, []string{"pb-item"})
				if p.err != nil || p.count != 1 {
					t.Fatalf("preview: %+v", p)
				}
				if n, err := s.applyForget(t.Context(), s.Store, "g1", mina, p, store.SuppressBySelf); err != nil || n != 1 {
					t.Fatalf("forget: %d %v", n, err)
				}
			}
			if pbHasFact(t, s, run.ID, target.Text) {
				t.Errorf("the pilot copy keeps a fact about the member on the corrected line, in other words: %q", target.Text)
			}
			if !pbHasFact(t, s, run.ID, minaOther.Text) {
				t.Error("control: the member's fact on another line went with it")
			}
			if !pbHasFact(t, s, run.ID, control.Text) {
				t.Error("control: someone else's fact went with it")
			}
		})
	}
}

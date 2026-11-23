package application

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"masamong/internal/features/privacy"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
)

// The stored pilot answers are re-checked inside the writer transaction of every deleted or edited
// message and of every withdrawal. These tests pin what those re-checks reach, that every wired path
// purges the copy, and that a failing re-check never holds back a member's own change.

// pbNativeOf is the Discord message id of an observation.
func pbNativeOf(t *testing.T, s *Service, obs string) string {
	t.Helper()
	var native string
	if err := s.Store.DB.QueryRow(`SELECT native_message_id FROM memory_observations WHERE observation_id=?`, obs).Scan(&native); err != nil {
		t.Fatal(err)
	}
	return native
}

// pbWindowsHolding counts the stored pilot windows of a run whose lines include obs, and lists every line
// of those windows.
func pbWindowsHolding(t *testing.T, s *Service, runID, obs string) (holding, all int, lines []string) {
	t.Helper()
	ws, err := store.BackfillWindows(s.Store.Reader(), runID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if len(w.Result) == 0 {
			continue
		}
		all++
		if slices.Contains(w.Obs, obs) {
			holding++
			lines = append(lines, w.Obs...)
		}
	}
	return holding, all, lines
}

// pbOutside reports whether a fact cites none of lines.
func pbOutside(f episodes.PersonFact, lines []string) bool {
	return !slices.ContainsFunc(f.Ev, func(id string) bool { return slices.Contains(lines, id) })
}

// pbStale makes a stored pilot fact fail the gate without any hook running: its line is marked deleted
// straight in the table. Only a re-check that reaches its window or its people takes it out.
func pbStale(t *testing.T, s *Service, f episodes.PersonFact) {
	t.Helper()
	if _, err := s.Store.DB.Exec(`UPDATE memory_observations SET deleted_at=? WHERE observation_id=?`, time.Now().UTC().Format(time.RFC3339), f.Ev[0]); err != nil {
		t.Fatal(err)
	}
}

// One deleted message re-checks only the pilot windows that hold it: the fact citing it is purged, a
// fact elsewhere that would fail the gate is not touched by this deletion (the full pass of a hide still
// takes it out), and the deletion stays well inside the writer budget.
func TestSourceChangeRechecksOnlyItsPilotWindows(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	_, run := pbPiloted(t, s, nil)
	facts := pbStoredFacts(t, s, run.ID)
	if len(facts) < 3 {
		t.Fatalf("stored facts: %d", len(facts))
	}
	ev := facts[0].Ev[0]
	holding, all, held := pbWindowsHolding(t, s, run.ID, ev)
	var stale episodes.PersonFact
	for _, f := range facts {
		if pbOutside(f, held) {
			stale = f
			break
		}
	}
	if stale.Text == "" {
		t.Fatal("no fact outside the deleted line's windows")
	}
	if holding == 0 || holding >= all {
		t.Fatalf("the deleted line is held by %d of %d stored windows", holding, all)
	}
	got, err := store.BackfillAnsweredFor(s.Store.Reader(), "g1", []string{ev}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != holding {
		t.Fatalf("the re-check reads %d windows for one deleted line, want the %d that hold it", len(got), holding)
	}
	pbStale(t, s, stale)
	began := time.Now()
	if err := s.MessagesDeleted("g1", "c1", []string{pbNativeOf(t, s, ev)}); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	for _, f := range pbStoredFacts(t, s, run.ID) {
		if slices.Contains(f.Ev, ev) {
			t.Fatal("a fact citing the deleted message stays in the stored pilot answers")
		}
	}
	if !pbHasFact(t, s, run.ID, stale.Text) {
		t.Fatal("the deletion re-checked a pilot window that does not hold the deleted line")
	}
	// the full pass (hide, forget) still reaches it: the stale fact is one the gate refuses
	if err := s.Store.WithWriter(func(tx *sql.Tx) error { return s.pbRecheckPilotTx(tx, "g1") }); err != nil {
		t.Fatal(err)
	}
	if pbHasFact(t, s, run.ID, stale.Text) {
		t.Fatal("control: the full re-check keeps a fact whose line is deleted")
	}
	// the hook alone, narrowed to one line, in a writer transaction
	const rounds = 20
	began = time.Now()
	for range rounds {
		if err := s.Store.WithWriter(func(tx *sql.Tx) error { return s.pbRecheckLinesTx(tx, "g1", []string{ev}) }); err != nil {
			t.Fatal(err)
		}
	}
	per := time.Since(began) / rounds
	t.Logf("stored pilot facts: %d in %d windows; one deletion: %s; the narrowed hook: %s per call", len(pbStoredFacts(t, s, run.ID)), all,
		took.Round(time.Millisecond), per.Round(10*time.Microsecond))
	// generous for a shared CI runner under -race: the full pass took about 100 ms on a laptop
	if took > 5*time.Second || per > time.Second {
		t.Errorf("one deletion took %s (the hook %s per call)", took, per)
	}
}

// A withdrawal re-checks only the stored facts that name the person (about them, said by them, or naming
// them in the text), in every server.
func TestWithdrawalRechecksOnlyFactsNamingThePerson(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	_, run := pbPiloted(t, s, nil)
	mina, joon := store.PersonRef("101"), store.PersonRef("102")
	var stale episodes.PersonFact
	for _, f := range pbStoredFacts(t, s, run.ID) {
		if !pbNames(f, joon) && f.About == mina {
			stale = f
			break
		}
	}
	if stale.Text == "" {
		t.Fatal("no fact of Mina that does not name Joon")
	}
	pbStale(t, s, stale)
	if _, err := s.setConsent("102", privacy.Profile, privacy.Withdrawn, "test"); err != nil {
		t.Fatal(err)
	}
	for _, f := range pbStoredFacts(t, s, run.ID) {
		if pbNames(f, joon) {
			t.Fatalf("a fact naming the withdrawn person stays: %+v", f)
		}
	}
	if !pbHasFact(t, s, run.ID, stale.Text) {
		t.Fatal("the withdrawal re-checked a fact that does not name the person")
	}
	if err := s.Store.WithWriter(func(tx *sql.Tx) error { return s.pbRecheckPilotTx(tx, "g1") }); err != nil {
		t.Fatal(err)
	}
	if pbHasFact(t, s, run.ID, stale.Text) {
		t.Fatal("control: the full re-check keeps a fact whose line is deleted")
	}
}

// pbTargetAndControl picks a stored self statement of Mina and an unrelated one of Joon, in different
// windows.
func pbTargetAndControl(t *testing.T, s *Service, runID string) (target, control episodes.PersonFact) {
	t.Helper()
	mina, joon := store.PersonRef("101"), store.PersonRef("102")
	for _, f := range pbStoredFacts(t, s, runID) {
		switch {
		case target.Text == "" && f.About == mina && slices.Equal(f.By, []string{mina}) && !pbNames(f, joon):
			target = f
		case target.Text != "" && control.Text == "" && f.About == joon && slices.Equal(f.By, []string{joon}) && !pbNames(f, mina):
			if _, _, held := pbWindowsHolding(t, s, runID, target.Ev[0]); pbOutside(f, held) {
				control = f
			}
		}
	}
	if target.Text == "" || control.Text == "" {
		t.Fatal("the stored pilot answers lack the facts the test needs")
	}
	return target, control
}

// The wired paths purge the pilot copy: a hide of the claim that says the same thing, and the real message
// edit and delete handlers of the line it rests on. In each, an unrelated fact stays.
func TestPilotPurgeWiredOnHideEditAndDelete(t *testing.T) {
	for _, how := range []string{"hide", "edit", "delete"} {
		t.Run(how, func(t *testing.T) {
			h := newPBHost(t)
			s := pbService(t, h, "anthropic")
			pbSeed(t, s, 70)
			_, run := pbPiloted(t, s, nil)
			target, control := pbTargetAndControl(t, s, run.ID)
			switch how {
			case "hide":
				c := store.PersonClaim{GuildID: "g1", SubjectRef: target.About, SpeakerRef: target.By[0], Kind: target.Kind, Topic: target.Topic,
					Text: target.Text, Evidence: target.Ev, Source: "backfill:" + run.ID, Link: target.Link}
				if err := s.Store.WithWriter(func(tx *sql.Tx) error {
					n, err := s.savePersonClaims(t.Context(), tx, []store.PersonClaim{c})
					if err == nil && n != 1 {
						t.Fatalf("control write: %d", n)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				id := store.PersonClaimID("g1", c.SubjectRef, c.SpeakerRef, c.Kind, store.PersonTopic(c.Topic), c.Evidence)
				if err := s.Store.WithWriter(func(tx *sql.Tx) error {
					ok, err := s.hideClaimTx(tx, id, store.SuppressBySelf)
					if err == nil && !ok {
						t.Fatal("no claim to hide")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "edit":
				if err := s.MessageEdited("g1", "c1", pbNativeOf(t, s, target.Ev[0]), "something else entirely now", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := s.MessagesDeleted("g1", "c1", []string{pbNativeOf(t, s, target.Ev[0])}); err != nil {
					t.Fatal(err)
				}
			}
			if pbHasFact(t, s, run.ID, target.Text) {
				t.Fatalf("after the %s the stored pilot answers keep the statement", how)
			}
			if !pbHasFact(t, s, run.ID, control.Text) {
				t.Fatalf("control: an unrelated pilot fact went with the %s", how)
			}
		})
	}
}

// A re-check that fails (here: the run table cannot be read) never holds back a member's deletion or
// withdrawal: the change commits and the failure is only logged. Hide and forget-all, which an operator or
// a member asks for and can retry, still return the error.
func TestPilotRecheckFailureDoesNotBlockDeletionOrWithdrawal(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	_, run := pbPiloted(t, s, nil)
	target, _ := pbTargetAndControl(t, s, run.ID)
	native := pbNativeOf(t, s, target.Ev[0])
	c := store.PersonClaim{GuildID: "g1", SubjectRef: target.About, SpeakerRef: target.By[0], Kind: target.Kind, Topic: target.Topic,
		Text: target.Text, Evidence: target.Ev, Source: "backfill:" + run.ID, Link: target.Link}
	if err := s.Store.WithWriter(func(tx *sql.Tx) error {
		_, err := s.savePersonClaims(t.Context(), tx, []store.PersonClaim{c})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id := store.PersonClaimID("g1", c.SubjectRef, c.SpeakerRef, c.Kind, store.PersonTopic(c.Topic), c.Evidence)
	if _, err := s.Store.DB.Exec(`ALTER TABLE people_backfill_run RENAME TO people_backfill_run_gone`); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.WithWriter(func(tx *sql.Tx) error { return s.pbRecheckLinesTx(tx, "g1", target.Ev) }); err == nil {
		t.Fatal("control: the re-check does not fail")
	}
	if err := s.Store.WithWriter(func(tx *sql.Tx) error { _, err := s.hideClaimTx(tx, id, store.SuppressBySelf); return err }); err == nil {
		t.Error("a hide whose pilot re-check failed reported success")
	}
	if err := s.Store.WithWriter(func(tx *sql.Tx) error {
		_, err := s.forgetPersonTx(tx, "g1", store.PersonRef("101"), "self")
		return err
	}); err == nil {
		t.Error("a forget-all whose pilot re-check failed reported success")
	}
	if err := s.MessagesDeleted("g1", "c1", []string{native}); err != nil {
		t.Fatalf("a failing pilot re-check held back the deletion: %v", err)
	}
	var tomb int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM privacy_tombstones WHERE kind='source' AND source_id=?`, native).Scan(&tomb); err != nil || tomb == 0 {
		t.Fatalf("the deletion did not commit: %d %v", tomb, err)
	}
	if _, err := s.setConsent("102", privacy.Memory, privacy.Withdrawn, "test"); err != nil {
		t.Fatalf("a failing pilot re-check held back the withdrawal: %v", err)
	}
	var status string
	if err := s.Store.DB.QueryRow(`SELECT status FROM consent_state WHERE user_id='102' AND scope=?`, privacy.Memory).Scan(&status); err != nil || status != privacy.Withdrawn {
		t.Fatalf("the withdrawal did not commit: %q %v", status, err)
	}
}

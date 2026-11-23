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
// message and of every withdrawal. These tests pin what those re-checks reach.

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

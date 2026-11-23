package application_test

import (
	"context"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/memory/store"
)

// Sol H3, Astra 11: a pending claim whose classifier call outlives the KV cache and fails must not make
// the writer wait on itself; the pass ends, other writes go through, and the claim waits for the next
// pass.
func TestClassifyPendingSlowFailure(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	strict(t, s, config.GuildPrivacy{NoSensitive: true})
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	f.fact(t, "g", "c", "slow-seed", "101", "101", f.source(t, "g", "c", "slow-seed-line", "101", "민수", f.at))
	f.PeoplePassForTest(t.Context()) // the one-time walk ends: live claims are classified from now on
	a := store.PersonRef("101")
	c := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "like", Topic: "음식", Text: "{@" + a + "}는 국수를 좋아함",
		Evidence: []string{f.source(t, "g", "c", "slow-line", "101", "민수", f.at)}, Source: "extract:seg-slow", Link: store.LinkSelf}
	if n, err := s.SavePersonClaimsForTest(t.Context(), c); err != nil || n != 1 || personClaimCount(t, s, "text=? AND state=?", c.Text, store.PersonPending) != 1 {
		t.Fatalf("a pending claim: %d (%v)", n, err)
	}
	m.mu.Lock()
	m.sensitive = func(string) string { time.Sleep(2500 * time.Millisecond); return "not json" }
	m.mu.Unlock()
	finishes := func(what string, run func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { defer close(done); run() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not finish: the writer waits on itself", what)
		}
	}
	finishes("the classification pass", func() { f.PeoplePassForTest(context.Background()) })
	other := c
	other.Topic, other.Text, other.Source = "면", "{@"+a+"}는 냉면도 좋아함", "extract:seg-other"
	other.Evidence = []string{f.source(t, "g", "c", "slow-other-line", "101", "민수", f.at)}
	finishes("a later write", func() {
		if n, err := s.SavePersonClaimsForTest(context.Background(), other); err != nil || n != 1 {
			t.Errorf("a later write: %d (%v)", n, err)
		}
	})
	if n := personClaimCount(t, s, "text=? AND state=?", c.Text, store.PersonPending); n != 1 {
		t.Fatalf("a claim the classifier did not answer for left pending: %d", n)
	}
	// the control: a classifier that answers makes it active on the next pass
	m.mu.Lock()
	m.sensitive = func(string) string { return `{"sensitive":[]}` }
	m.mu.Unlock()
	f.PeoplePassForTest(t.Context())
	if n := personClaimCount(t, s, "text=? AND state=?", c.Text, store.PersonActive); n != 1 {
		t.Fatalf("classified on the next pass: %d", n)
	}
}

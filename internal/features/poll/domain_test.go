package poll

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMultiSelectHandComputed(t *testing.T) {
	p, err := Create(CreateCmd{
		ID: "p1", ScopeKey: "room-a", Question: "lunch",
		Options: []Option{{"a", "A"}, {"b", "B"}, {"c", "C"}},
		MaxSelect: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p, err = Vote(p, VoteCmd{PollID: "p1", ScopeKey: "room-a", ExpectedRev: p.Revision, ParticipantKey: "u1", OptionIDs: []string{"a", "b"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	p, err = Vote(p, VoteCmd{PollID: "p1", ScopeKey: "room-a", ExpectedRev: p.Revision, ParticipantKey: "u2", OptionIDs: []string{"a"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	p, err = Vote(p, VoteCmd{PollID: "p1", ScopeKey: "room-a", ExpectedRev: p.Revision, ParticipantKey: "u3", OptionIDs: []string{"b", "c"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	r := Tally(p)
	// a: u1,u2 = 2; b: u1,u3 = 2; c: u3 = 1; unique 3; selections 5
	got := map[string]int{}
	for _, row := range r.ByOption {
		got[row.OptionID] = row.Votes
	}
	if got["a"] != 2 || got["b"] != 2 || got["c"] != 1 {
		t.Fatalf("tallies %+v", got)
	}
	if r.UniqueVoters != 3 || r.TotalSelections != 5 {
		t.Fatalf("unique=%d total=%d", r.UniqueVoters, r.TotalSelections)
	}
	if len(r.TieOptionIDs) != 2 {
		t.Fatalf("ties %v", r.TieOptionIDs)
	}
}

func TestVoteChangeReplaces(t *testing.T) {
	p, _ := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: []Option{{"a", "A"}, {"b", "B"}}, MaxSelect: 1})
	now := time.Now()
	p, _ = Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: p.Revision, ParticipantKey: "u", OptionIDs: []string{"a"}, Now: now})
	p, err := Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: p.Revision, ParticipantKey: "u", OptionIDs: []string{"b"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	r := Tally(p)
	if r.ByOption[0].OptionID != "b" || r.ByOption[0].Votes != 1 {
		t.Fatalf("%+v", r.ByOption)
	}
	if Tally(p).TotalSelections != 1 {
		t.Fatal("old vote must be gone")
	}
}

func TestDuplicateSubmitDoesNotDoubleCount(t *testing.T) {
	p, _ := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: []Option{{"a", "A"}, {"b", "B"}}, MaxSelect: 1})
	now := time.Now()
	p, _ = Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: 1, ParticipantKey: "u", OptionIDs: []string{"a"}, Now: now})
	rev := p.Revision
	p, err := Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: rev, ParticipantKey: "u", OptionIDs: []string{"a"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if p.Revision != rev {
		t.Fatalf("revision bumped on identical resubmit: %d", p.Revision)
	}
	if Tally(p).TotalSelections != 1 {
		t.Fatal("double count")
	}
}

func TestCloseBoundary(t *testing.T) {
	closeAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p, _ := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: []Option{{"a", "A"}, {"b", "B"}}, MaxSelect: 1, ClosesAt: &closeAt})
	if _, err := Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: 1, ParticipantKey: "u", OptionIDs: []string{"a"}, Now: closeAt}); err == nil {
		t.Fatal("vote at close time must be rejected")
	}
	if _, err := Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: 1, ParticipantKey: "u", OptionIDs: []string{"a"}, Now: closeAt.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
}

func TestWrongScopeAndStaleRevisionLeaveState(t *testing.T) {
	p, _ := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: []Option{{"a", "A"}, {"b", "B"}}, MaxSelect: 1})
	orig, _ := Marshal(p)
	if _, err := Vote(p, VoteCmd{ScopeKey: "other", ExpectedRev: 1, ParticipantKey: "u", OptionIDs: []string{"a"}, Now: time.Now()}); err == nil {
		t.Fatal("expected scope error")
	}
	if _, err := Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: 99, ParticipantKey: "u", OptionIDs: []string{"a"}, Now: time.Now()}); err == nil {
		t.Fatal("expected stale")
	}
	after, _ := Marshal(p)
	if string(orig) != string(after) {
		t.Fatal("state mutated on failure")
	}
}

func TestInputSliceMutationDoesNotAffectStore(t *testing.T) {
	opts := []Option{{"a", "A"}, {"b", "B"}}
	p, _ := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: opts, MaxSelect: 1})
	opts[0].ID = "hacked"
	if p.Options[0].ID != "a" {
		t.Fatal("options alias")
	}
	sel := []string{"a"}
	p, _ = Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: 1, ParticipantKey: "u", OptionIDs: sel, Now: time.Now()})
	sel[0] = "b"
	if p.Votes["u"][0] != "a" {
		t.Fatal("selection alias")
	}
}

func TestDiscordPollLimits(t *testing.T) {
	longQ := strings.Repeat("가", 301)
	if _, err := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: longQ, Options: []Option{{"a", "A"}, {"b", "B"}}, MaxSelect: 1}); err == nil {
		t.Fatal("expected question length error")
	}
	if _, err := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: []Option{{"a", strings.Repeat("x", 56)}, {"b", "B"}}, MaxSelect: 1}); err == nil {
		t.Fatal("expected answer length error")
	}
	tooMany := make([]Option, 11)
	for i := range tooMany {
		tooMany[i] = Option{ID: fmt.Sprintf("%d", i), Label: "x"}
	}
	if _, err := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: tooMany, MaxSelect: 1}); err == nil {
		t.Fatal("expected max 10 answers")
	}
}

func TestRepeatedCloseIdempotent(t *testing.T) {
	p, _ := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: []Option{{"a", "A"}, {"b", "B"}}, MaxSelect: 1})
	p, _ = Close(p, CloseCmd{ScopeKey: "s", ExpectedRev: p.Revision, Now: time.Now()})
	rev := p.Revision
	p2, err := Close(p, CloseCmd{ScopeKey: "s", ExpectedRev: rev, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if p2.Revision != rev || Tally(p).TotalSelections != Tally(p2).TotalSelections {
		t.Fatal("close changed results")
	}
}

func TestSameDisplayNameDifferentParticipants(t *testing.T) {
	p, _ := Create(CreateCmd{ID: "p", ScopeKey: "s", Question: "q", Options: []Option{{"a", "A"}, {"b", "B"}}, MaxSelect: 1})
	now := time.Now()
	p, _ = Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: p.Revision, ParticipantKey: "acct-1", OptionIDs: []string{"a"}, Now: now})
	p, _ = Vote(p, VoteCmd{ScopeKey: "s", ExpectedRev: p.Revision, ParticipantKey: "acct-2", OptionIDs: []string{"a"}, Now: now})
	if Tally(p).UniqueVoters != 2 || Tally(p).ByOption[0].Votes != 2 {
		t.Fatalf("%+v", Tally(p))
	}
}

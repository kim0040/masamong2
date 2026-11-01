// Package poll contains the pure poll tally and state rules used by every transport.
// Persistence, Discord buttons, and ACL live in adapters. Time is always an argument.
package poll

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"masamong/internal/errcode"
)

type Status string

const (
	Open   Status = "open"
	Closed Status = "closed"
)

type Poll struct {
	ID          string
	ScopeKey    string
	Question    string
	Options     []Option
	MaxSelect   int
	ClosesAt    *time.Time
	Status      Status
	Revision    int
	Votes       map[string][]string // participant -> option IDs
}

type Option struct {
	ID    string
	Label string
}

type Result struct {
	Poll            Poll
	ByOption        []OptionTally
	UniqueVoters    int
	TotalSelections int
	TieOptionIDs    []string
}

type OptionTally struct {
	OptionID string
	Label    string
	Votes    int
	Ratio    float64 // denominator = TotalSelections; 0 if none
}

type CreateCmd struct {
	ID        string
	ScopeKey  string
	Question  string
	Options   []Option
	MaxSelect int
	ClosesAt  *time.Time
}

type VoteCmd struct {
	PollID         string
	ScopeKey       string
	ExpectedRev    int
	ParticipantKey string
	OptionIDs      []string
	Now            time.Time
}

type CloseCmd struct {
	PollID      string
	ScopeKey    string
	ExpectedRev int
	Now         time.Time
}

func Create(cmd CreateCmd) (Poll, error) {
	if cmd.ID == "" || cmd.ScopeKey == "" || cmd.Question == "" {
		return Poll{}, fmt.Errorf("%s: id, scope, question required", errcode.InvalidInput)
	}
	if runes(cmd.Question) > 300 {
		return Poll{}, fmt.Errorf("%s: question max 300 characters (Discord poll)", errcode.InvalidInput)
	}
	if cmd.MaxSelect < 1 {
		cmd.MaxSelect = 1
	}
	seen := map[string]struct{}{}
	opts := make([]Option, 0, len(cmd.Options))
	for _, o := range cmd.Options {
		if o.ID == "" {
			return Poll{}, fmt.Errorf("%s: option id required", errcode.InvalidInput)
		}
		if _, ok := seen[o.ID]; ok {
			return Poll{}, fmt.Errorf("%s: duplicate option %s", errcode.InvalidInput, o.ID)
		}
		if runes(o.Label) > 55 {
			return Poll{}, fmt.Errorf("%s: answer max 55 characters (Discord poll)", errcode.InvalidInput)
		}
		seen[o.ID] = struct{}{}
		opts = append(opts, Option{ID: o.ID, Label: o.Label})
	}
	if len(opts) < 2 {
		return Poll{}, fmt.Errorf("%s: at least two options", errcode.InvalidInput)
	}
	if len(opts) > 10 {
		return Poll{}, fmt.Errorf("%s: at most 10 answers (Discord poll)", errcode.InvalidInput)
	}
	if cmd.MaxSelect > len(opts) {
		return Poll{}, fmt.Errorf("%s: max_select exceeds options", errcode.InvalidInput)
	}
	return Poll{
		ID: cmd.ID, ScopeKey: cmd.ScopeKey, Question: cmd.Question,
		Options: opts, MaxSelect: cmd.MaxSelect, ClosesAt: cloneTime(cmd.ClosesAt),
		Status: Open, Revision: 1, Votes: map[string][]string{},
	}, nil
}

func Vote(p Poll, cmd VoteCmd) (Poll, error) {
	if err := guard(p, cmd.ScopeKey, cmd.ExpectedRev); err != nil {
		return p, err
	}
	if p.Status != Open {
		return p, fmt.Errorf("%s: poll closed", errcode.InvalidInput)
	}
	if p.ClosesAt != nil && !cmd.Now.Before(*p.ClosesAt) {
		return p, fmt.Errorf("%s: poll closed by schedule", errcode.InvalidInput)
	}
	if cmd.ParticipantKey == "" {
		return p, fmt.Errorf("%s: participant required", errcode.InvalidInput)
	}
	if len(cmd.OptionIDs) == 0 || len(cmd.OptionIDs) > p.MaxSelect {
		return p, fmt.Errorf("%s: selection size", errcode.InvalidInput)
	}
	allowed := map[string]struct{}{}
	for _, o := range p.Options {
		allowed[o.ID] = struct{}{}
	}
	uniq := map[string]struct{}{}
	sel := make([]string, 0, len(cmd.OptionIDs))
	for _, id := range cmd.OptionIDs {
		if _, ok := allowed[id]; !ok {
			return p, fmt.Errorf("%s: unknown option %s", errcode.InvalidInput, id)
		}
		if _, dup := uniq[id]; dup {
			continue
		}
		uniq[id] = struct{}{}
		sel = append(sel, id)
	}
	sort.Strings(sel)
	next := clone(p)
	prev := next.Votes[cmd.ParticipantKey]
	same := len(prev) == len(sel)
	if same {
		for i := range sel {
			if prev[i] != sel[i] {
				same = false
				break
			}
		}
	}
	next.Votes[cmd.ParticipantKey] = sel
	if !same {
		next.Revision++
	}
	return next, nil
}

func Close(p Poll, cmd CloseCmd) (Poll, error) {
	if err := guard(p, cmd.ScopeKey, cmd.ExpectedRev); err != nil {
		return p, err
	}
	next := clone(p)
	if next.Status == Closed {
		return next, nil // idempotent close does not bump revision or change tallies
	}
	next.Status = Closed
	next.Revision++
	return next, nil
}

func Tally(p Poll) Result {
	counts := map[string]int{}
	total := 0
	for _, sel := range p.Votes {
		for _, id := range sel {
			counts[id]++
			total++
		}
	}
	by := make([]OptionTally, 0, len(p.Options))
	max := 0
	for _, o := range p.Options {
		n := counts[o.ID]
		if n > max {
			max = n
		}
		ratio := 0.0
		if total > 0 {
			ratio = float64(n) / float64(total)
		}
		by = append(by, OptionTally{OptionID: o.ID, Label: o.Label, Votes: n, Ratio: ratio})
	}
	sort.Slice(by, func(i, j int) bool {
		if by[i].Votes != by[j].Votes {
			return by[i].Votes > by[j].Votes
		}
		return by[i].OptionID < by[j].OptionID
	})
	var ties []string
	if max > 0 {
		for _, row := range by {
			if row.Votes == max {
				ties = append(ties, row.OptionID)
			}
		}
	}
	sort.Strings(ties)
	if len(ties) == 1 {
		ties = nil
	}
	return Result{Poll: p, ByOption: by, UniqueVoters: len(p.Votes), TotalSelections: total, TieOptionIDs: ties}
}

func Marshal(p Poll) ([]byte, error) { return json.Marshal(p) }

func Unmarshal(b []byte) (Poll, error) {
	var p Poll
	if err := json.Unmarshal(b, &p); err != nil {
		return Poll{}, err
	}
	if p.Votes == nil {
		p.Votes = map[string][]string{}
	}
	return p, nil
}

func guard(p Poll, scope string, rev int) error {
	if p.ScopeKey != scope {
		return fmt.Errorf("%s: wrong scope", errcode.ScopeMismatch)
	}
	if p.Revision != rev {
		return fmt.Errorf("%s: expected %d", errcode.StaleRevision, p.Revision)
	}
	return nil
}

func clone(p Poll) Poll {
	opts := append([]Option(nil), p.Options...)
	votes := map[string][]string{}
	for k, v := range p.Votes {
		votes[k] = append([]string(nil), v...)
	}
	return Poll{
		ID: p.ID, ScopeKey: p.ScopeKey, Question: p.Question, Options: opts,
		MaxSelect: p.MaxSelect, ClosesAt: cloneTime(p.ClosesAt), Status: p.Status,
		Revision: p.Revision, Votes: votes,
	}
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	x := *t
	return &x
}

func runes(s string) int {
	return len([]rune(s))
}

// Unvote removes a participant's ballot (they took back their last choice in the Discord poll).
func Unvote(p Poll, cmd VoteCmd) (Poll, error) {
	if err := guard(p, cmd.ScopeKey, cmd.ExpectedRev); err != nil {
		return p, err
	}
	if _, ok := p.Votes[cmd.ParticipantKey]; !ok {
		return p, nil
	}
	next := clone(p)
	delete(next.Votes, cmd.ParticipantKey)
	next.Revision++
	return next, nil
}

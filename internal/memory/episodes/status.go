package episodes

import "time"

// Plan statuses (docs/plan-memory-conversation.md §5.4.3, following OpenClaw's standing intents):
//
//	open → decided (needs decision evidence) → done (the date passed and a later mention says so)
//	     ↘ dropped (explicit cancellation only) ↘ expired (due+7d, 30d since the last mention, undecided proposals 14d)
//
// A cancellation is never inferred: only a close operation that cites the cancelling line drops a plan.
// Expiry is decided in code by the background pass, never by the model.
const (
	Open    = "open"
	Decided = "decided"
	Done    = "done"
	Dropped = "dropped"
	Expired = "expired"
)

// Expiry windows.
const (
	DueGrace        = 7 * 24 * time.Hour
	SilentExpiry    = 30 * 24 * time.Hour
	ProposalExpiry  = 14 * 24 * time.Hour
	RegenerateShare = 0.4 // hidden items at or above this share of an episode queue it for regeneration
)

// Terminal reports whether a status no longer changes.
func Terminal(status string) bool { return status == Done || status == Dropped || status == Expired }

// Change is what an operation asks for, with the facts the status machine needs.
type Change struct {
	Kind        string    // the episode's kind
	Op          string    // create | update | close
	Want        string    // the status asked for ("" keeps the current one)
	HasDecision bool      // the episode has (or this operation adds) a valid decision item
	Due         time.Time // zero when unknown
	EvidenceAt  time.Time // the latest evidence of this operation
}

// Next is the status after a change. Statuses only move forward; a terminal status stays.
func Next(current string, c Change) string {
	if current == "" {
		current = Open
	}
	if Terminal(current) {
		return current
	}
	switch c.Want {
	case Decided:
		if c.HasDecision {
			return Decided
		}
	case Done:
		if c.Kind != "plan" {
			return Done // something that happened, a settled fact
		}
		if !c.Due.IsZero() && c.EvidenceAt.After(c.Due) {
			return Done
		}
	case Dropped:
		if c.Op == "close" {
			return Dropped
		}
	}
	return current
}

// ExpiresAt is when an episode in this state expires (zero: it does not). Only plans and decisions that
// are still open or decided expire: a dated one seven days after its date, an undated undecided proposal
// 14 days after it was last mentioned, any other 30 days after.
func ExpiresAt(kind, status string, due time.Time, lastSeen time.Time) time.Time {
	if Terminal(status) || (kind != "plan" && kind != "decision") {
		return time.Time{}
	}
	if !due.IsZero() {
		return due.Add(DueGrace) // a dated plan lives until its day has passed
	}
	if kind == "plan" && status == Open {
		return lastSeen.Add(ProposalExpiry)
	}
	return lastSeen.Add(SilentExpiry)
}

// Expire returns the status at now (Expired when ExpiresAt has passed).
func Expire(kind, status string, due, lastSeen, now time.Time) string {
	at := ExpiresAt(kind, status, due, lastSeen)
	if !at.IsZero() && !now.Before(at) {
		return Expired
	}
	return status
}

// NeedsRegeneration decides whether an episode whose items lost evidence is made again from the raw
// messages: 40% or more of its items hidden, or a hidden decision.
func NeedsRegeneration(total, hidden int, decisionHidden bool) bool {
	if total == 0 || hidden == 0 {
		return false
	}
	return decisionHidden || float64(hidden)/float64(total) >= RegenerateShare
}

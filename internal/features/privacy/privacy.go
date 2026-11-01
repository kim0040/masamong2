// Package privacy holds the purpose-scoped consent policies (V1 utils/privacy_consent.py, 19장 A36).
//
// Two kinds of scope:
//   - opt-in (fortune, school_notice, transfer_notice): the bot stores data a person gives it for one
//     feature only after that person agreed to the current policy version. Withdrawing stops use and
//     delivery at once; the stored data stays until the person deletes it with the feature's command,
//     and agreeing again resumes with it.
//   - opt-out (memory, participation): whether a channel's conversation is remembered is the operator's per-channel
//     capture setting; a person can withdraw for themselves. Their new messages are then not stored
//     and the ones already stored are removed from memory (a deletion, not a pause: text the bot keeps
//     "but does not use" would still be quoted by search). Withdrawing participation means the bot
//     never joins in on that person's messages unasked (12); asking it directly still works.
//
// Policy texts are localized (locales/*.json privacy.<scope>.notice); a text change that changes what
// is collected or why must come with a new Version, which makes earlier opt-in consent stale.
package privacy

import (
	"strings"
)

// Consent states.
const (
	Granted   = "granted"
	Withdrawn = "withdrawn"
)

// Scopes.
const (
	Memory         = "memory"
	Fortune        = "fortune"
	SchoolNotice   = "school_notice"
	TransferNotice = "transfer_notice"
	Participation  = "participation"
	// Profile is "don't analyze me": no person card about them, and their lines stay out of the room
	// style analysis. Withdrawing deletes their cards.
	Profile = "profile"
)

// Policy is one purpose.
type Policy struct {
	Scope   string `json:"scope"`
	Version string `json:"version"`
	// OptOut scopes are allowed until the person withdraws; the others need a current grant.
	OptOut bool `json:"opt_out"`
}

var policies = []Policy{
	{Scope: Memory, Version: "2026-09-28.v1", OptOut: true},
	// the V1 versions are kept so consent given to the previous bot stays valid if it is migrated
	{Scope: Fortune, Version: "2026-07-28.v1"},
	{Scope: SchoolNotice, Version: "2026-07-28.v2"},
	{Scope: TransferNotice, Version: "2026-07-28.v1"},
	{Scope: Participation, Version: "2026-09-28.v1", OptOut: true},
	{Scope: Profile, Version: "2026-10-11.v1", OptOut: true},
}

// Policies lists the scopes in display order.
func Policies() []Policy { return append([]Policy(nil), policies...) }

// Get returns the policy of a scope.
func Get(scope string) (Policy, bool) {
	for _, p := range policies {
		if p.Scope == scope {
			return p, true
		}
	}
	return Policy{}, false
}

// Normalize maps a scope ID or one of the words people type for it to the scope. words holds the
// localized words (privacy.scope.<scope>.aliases in the message catalogs, lower-cased); IDs and their
// hyphenated forms are always understood.
func Normalize(s string, words map[string]string) (string, bool) {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	for _, p := range policies {
		if key == p.Scope || key == strings.ReplaceAll(p.Scope, "_", "-") {
			return p.Scope, true
		}
	}
	v, ok := words[key]
	return v, ok
}

// State is one person's recorded decision for one scope.
type State struct {
	Scope       string `json:"scope"`
	Status      string `json:"status"` // granted | withdrawn | "" (never decided)
	Version     string `json:"policy_version,omitempty"`
	GrantedAt   string `json:"granted_at,omitempty"`
	WithdrawnAt string `json:"withdrawn_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// Allows reports whether the recorded state lets the feature use the person's data now.
func (p Policy) Allows(s State) bool {
	if p.OptOut {
		return s.Status != Withdrawn
	}
	return s.Status == Granted && s.Version == p.Version
}

// Stale reports a grant given to an earlier version of an opt-in policy (it must be asked again).
func (p Policy) Stale(s State) bool {
	return !p.OptOut && s.Status == Granted && s.Version != p.Version
}

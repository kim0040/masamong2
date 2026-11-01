package application

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"masamong/internal/providers/openai"
)

// Key credit: an OpenRouter key can carry its own spending limit, often a lifetime one that never
// resets. The bot's monthly budget knows nothing of it: on 2026-10-14 the key ran out at night while
// the month still had room, and every call failed with 403 "Key limit exceeded". The keys' limits are
// read in the background at most every creditEvery (a page view never waits on OpenRouter) and the
// console warns when what is left on a key is less than what the month's budget still allows.

const (
	creditEvery   = 15 * time.Minute
	creditKeep    = time.Hour // a key whose read fails keeps its last figure this long (no warning flicker)
	creditTimeout = 10 * time.Second
	creditDanger  = 0.5 // USD left on a key: it fails any moment
)

type creditCache struct {
	mu      sync.Mutex
	at      time.Time
	running bool
	keys    map[string]openai.KeyCredit // provider ID → the key's credit; only keys with a limit
	read    map[string]time.Time        // provider ID → when its figure was read
}

// providerCredit returns the last known credit of the configured OpenRouter keys and starts a refresh
// when it is older than creditEvery. The first call after start returns nothing.
func (s *Service) providerCredit() map[string]openai.KeyCredit {
	c := &s.credit
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running && !s.practice && s.Clock.Now().Sub(c.at) >= creditEvery {
		c.running = true
		s.ownedGo("key credit", s.refreshCredit)
	}
	out := make(map[string]openai.KeyCredit, len(c.keys))
	for id, k := range c.keys {
		out[id] = k
	}
	return out
}

// creditRead is one key's answer: ok is false when the read failed (a timeout, a 5xx).
type creditRead struct {
	k  openai.KeyCredit
	ok bool
}

// refreshCredit reads every configured OpenRouter key.
func (s *Service) refreshCredit(ctx context.Context) {
	got := map[string]creditRead{}
	for id, a := range s.Registry().LLM {
		if a == nil || a.API != openai.APIOpenRouter || !a.Configured() || s.Doc().Providers[id].Disabled {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, creditTimeout)
		k, err := a.Credit(cctx)
		cancel()
		got[id] = creditRead{k, err == nil}
	}
	s.credit.store(got, s.Clock.Now())
}

// store replaces the cache with this round's reads. A key whose read failed keeps its last figure for
// creditKeep (no warning that comes and goes with OpenRouter's hiccups); a key that answers without a
// limit, or that is no longer configured, drops out.
func (c *creditCache) store(got map[string]creditRead, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys, read := map[string]openai.KeyCredit{}, map[string]time.Time{}
	for id, r := range got {
		switch {
		case r.ok && r.k.Limit != nil && r.k.Remaining != nil:
			keys[id], read[id] = r.k, now
		case !r.ok && now.Sub(c.read[id]) < creditKeep:
			if old, had := c.keys[id]; had {
				keys[id], read[id] = old, c.read[id]
			}
		}
	}
	c.keys, c.read, c.at, c.running = keys, read, now, false
}

// creditAttention warns about a key whose remaining credit is below what the month's budget still
// allows (or below creditDanger when no budget is set).
func (s *Service) creditAttention(spent, budget float64) []attention {
	credit := s.providerCredit()
	ids := make([]string, 0, len(credit))
	for id := range credit {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var att []attention
	for _, id := range ids {
		k := credit[id]
		left := *k.Remaining
		args := map[string]any{"provider": id, "left": fmtUSD(left), "limit": fmtUSD(*k.Limit)}
		switch {
		case left <= creditDanger:
			att = append(att, attention{"danger", "key_credit_out", args, "/connections"})
		case budget > 0 && left < budget-spent:
			args["room"] = fmtUSD(budget - spent)
			att = append(att, attention{"warn", "key_credit_low", args, "/usage"})
		}
	}
	return att
}

func fmtUSD(v float64) string {
	return fmt.Sprintf("%.2f", max(v, 0))
}

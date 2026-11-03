package application

import (
	"sync"
	"time"
)

// ttlCache is a small keyed cache for process-lifetime lookups (geocoded places, per-person cooldowns).
// Entries older than ttl are not returned and are removed when the cache fills; if it is still full the
// oldest go, so memory is bounded by max whatever people type. There is no timer: the cost is paid by the
// writes that would grow it.
type ttlCache[V any] struct {
	mu  sync.Mutex
	m   map[string]ttlEntry[V]
	max int
	ttl time.Duration
}

type ttlEntry[V any] struct {
	v  V
	at time.Time
}

func newTTLCache[V any](max int, ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{m: map[string]ttlEntry[V]{}, max: max, ttl: ttl}
}

// get returns the value and when it was stored.
func (c *ttlCache[V]) get(key string, now time.Time) (V, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || now.Sub(e.at) >= c.ttl {
		var zero V
		return zero, time.Time{}, false
	}
	return e.v, e.at, true
}

func (c *ttlCache[V]) put(key string, v V, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(key, v, now)
}

// putUnlessWithin is the check and the set of a cooldown in one step, under one lock: when the key was
// stored less than cooldown ago (and not in the future) it stores nothing and returns when it was; else
// it stores v at now and returns ok. Two callers racing for the same key cannot both get ok.
func (c *ttlCache[V]) putUnlessWithin(key string, v V, now time.Time, cooldown time.Duration) (last time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, found := c.m[key]; found && now.Sub(e.at) < c.ttl && cooldown > 0 && now.Sub(e.at) < cooldown && !now.Before(e.at) {
		return e.at, false
	}
	c.putLocked(key, v, now)
	return now, true
}

func (c *ttlCache[V]) putLocked(key string, v V, now time.Time) {
	if _, replace := c.m[key]; !replace && len(c.m) >= c.max {
		for k, e := range c.m {
			if now.Sub(e.at) >= c.ttl {
				delete(c.m, k)
			}
		}
		for len(c.m) >= c.max {
			var oldest string
			var oldestAt time.Time
			first := true
			for k, e := range c.m {
				if first || e.at.Before(oldestAt) {
					oldest, oldestAt, first = k, e.at, false
				}
			}
			delete(c.m, oldest)
		}
	}
	c.m[key] = ttlEntry[V]{v: v, at: now}
}

func (c *ttlCache[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

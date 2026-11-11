package providers

import (
	"sync"
	"time"
)

// QuotaStore keeps the day's counts outside the process, so a restart or a config change (which
// rebuilds the registry) does not hand out a fresh daily allowance.
type QuotaStore interface {
	Counts(day string) (map[string]int, error)
	Bump(day, provider string) error
}

// Quota is a daily call counter per provider (10장). The day is the caller's calendar day (Day, the
// instance time zone); with a Store the count carries across restarts within that day.
type Quota struct {
	mu     sync.Mutex
	limits map[string]int
	counts map[string]int
	day    string
	Now    func() time.Time
	// Day maps an instant to its counting day (YYYY-MM-DD). Nil counts UTC days.
	Day   func(time.Time) string
	Store QuotaStore
	// OnError reports store failures; counting continues in memory.
	OnError func(error)
}

func NewQuota(limits map[string]int) *Quota {
	return &Quota{limits: limits, counts: map[string]int{}, Now: time.Now}
}

func (q *Quota) today() string {
	now := q.Now()
	if q.Day != nil {
		return q.Day(now)
	}
	return now.UTC().Format("2006-01-02")
}

// roll switches to day, loading its stored counts. Caller holds mu.
func (q *Quota) roll(day string) {
	if day == q.day {
		return
	}
	q.day = day
	q.counts = map[string]int{}
	if q.Store == nil {
		return
	}
	stored, err := q.Store.Counts(day)
	if err != nil {
		q.fail(err)
		return
	}
	for k, v := range stored {
		q.counts[k] = v
	}
}

func (q *Quota) fail(err error) {
	if q.OnError != nil && err != nil {
		q.OnError(err)
	}
}

// Reserve consumes one call for provider on the current day. It returns false when the limit is reached.
func (q *Quota) Reserve(provider string) (used, limit int, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll(q.today())
	limit = q.limits[provider]
	if limit > 0 && q.counts[provider] >= limit {
		return q.counts[provider], limit, false
	}
	q.counts[provider]++
	if q.Store != nil {
		q.fail(q.Store.Bump(q.day, provider))
	}
	return q.counts[provider], limit, true
}

// ReserveN consumes up to n units for provider on the current day and returns how many it granted (n,
// or what is left of the limit; 0 when it is used up). A search that reads several pages counts each
// page, not the one call.
func (q *Quota) ReserveN(provider string, n int) (granted int) {
	if n <= 0 {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll(q.today())
	granted = n
	if limit := q.limits[provider]; limit > 0 {
		granted = max(0, min(n, limit-q.counts[provider]))
	}
	q.counts[provider] += granted
	if q.Store != nil {
		for range granted {
			q.fail(q.Store.Bump(q.day, provider))
		}
	}
	return granted
}

// Snapshot returns today's counts.
func (q *Quota) Snapshot() map[string]int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.roll(q.today())
	out := map[string]int{}
	for k, v := range q.counts {
		out[k] = v
	}
	return out
}

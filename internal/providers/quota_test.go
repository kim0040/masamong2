package providers

import (
	"errors"
	"testing"
	"time"
)

type memQuotaStore struct {
	rows map[string]map[string]int
	fail bool
}

func (m *memQuotaStore) Counts(day string) (map[string]int, error) {
	if m.fail {
		return nil, errors.New("db down")
	}
	out := map[string]int{}
	for k, v := range m.rows[day] {
		out[k] = v
	}
	return out, nil
}

func (m *memQuotaStore) Bump(day, p string) error {
	if m.fail {
		return errors.New("db down")
	}
	if m.rows[day] == nil {
		m.rows[day] = map[string]int{}
	}
	m.rows[day][p]++
	return nil
}

// A restarted process (a new Quota over the same store) continues the day's count; the next day in
// the configured zone starts over.
func TestQuotaSurvivesRestartWithinTheDay(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 24, 23, 30, 0, 0, seoul)
	store := &memQuotaStore{rows: map[string]map[string]int{}}
	mk := func() *Quota {
		q := NewQuota(map[string]int{"kma": 3})
		q.Now = func() time.Time { return now }
		q.Day = func(t time.Time) string { return t.In(seoul).Format("2006-01-02") }
		q.Store = store
		return q
	}
	a := mk()
	for i := 0; i < 2; i++ {
		if _, _, ok := a.Reserve("kma"); !ok {
			t.Fatal("under the limit")
		}
	}
	b := mk() // restart
	if got := b.Snapshot()["kma"]; got != 2 {
		t.Fatalf("restart lost the count: %d", got)
	}
	if _, _, ok := b.Reserve("kma"); !ok {
		t.Fatal("third call allowed")
	}
	if used, limit, ok := b.Reserve("kma"); ok || used != 3 || limit != 3 {
		t.Fatalf("fourth call must be refused: %d/%d %v", used, limit, ok)
	}
	// 00:30 the next day in Seoul (still the 24th in UTC) is a new day
	now = now.Add(time.Hour)
	if got := b.Snapshot()["kma"]; got != 0 {
		t.Fatalf("new local day must start at 0, got %d", got)
	}
	if _, _, ok := b.Reserve("kma"); !ok {
		t.Fatal("new day allows calls")
	}
}

func TestQuotaKeepsCountingWhenTheStoreFails(t *testing.T) {
	store := &memQuotaStore{rows: map[string]map[string]int{}, fail: true}
	var errs int
	q := NewQuota(map[string]int{"x": 2})
	q.Store, q.OnError = store, func(error) { errs++ }
	q.Reserve("x")
	q.Reserve("x")
	if _, _, ok := q.Reserve("x"); ok {
		t.Fatal("the in-memory limit still applies while the store is down")
	}
	if errs == 0 {
		t.Fatal("store failures must be reported")
	}
}

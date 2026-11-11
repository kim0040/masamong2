package providers

import "testing"

type countStore struct{ bumps map[string]int }

func (s *countStore) Counts(day string) (map[string]int, error) { return map[string]int{}, nil }
func (s *countStore) Bump(day, provider string) error           { s.bumps[provider]++; return nil }

// ReserveN counts units, not calls: a search that reads five pages takes five.
func TestReserveNGrantsWhatIsLeft(t *testing.T) {
	st := &countStore{bumps: map[string]int{}}
	q := NewQuota(map[string]int{"fetch": 5})
	q.Store = st
	if got := q.ReserveN("fetch", 3); got != 3 {
		t.Fatalf("first: %d", got)
	}
	if got := q.ReserveN("fetch", 3); got != 2 {
		t.Fatalf("only two left: %d", got)
	}
	if got := q.ReserveN("fetch", 1); got != 0 {
		t.Fatalf("used up: %d", got)
	}
	if got := q.ReserveN("fetch", 0); got != 0 || q.Snapshot()["fetch"] != 5 || st.bumps["fetch"] != 5 {
		t.Fatalf("counted %d, stored %d", q.Snapshot()["fetch"], st.bumps["fetch"])
	}
	if got := q.ReserveN("free", 100); got != 100 { // no limit: everything
		t.Fatalf("unlimited: %d", got)
	}
}

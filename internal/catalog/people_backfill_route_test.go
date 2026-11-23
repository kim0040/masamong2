package catalog

import "testing"

// The backfill's console panel lives on the memory page (/memory?tab=person): both actions route there.
func TestPeopleBackfillRoutesToMemory(t *testing.T) {
	for _, id := range []string{"memory.people.backfill", "memory.people.backfill.review"} {
		d, ok := Lookup(id)
		if !ok || d.GUIRoute != "/memory" {
			t.Fatalf("%s: route %q (%v), want /memory", id, d.GUIRoute, ok)
		}
	}
}

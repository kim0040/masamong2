package catalog

import "testing"

// All used to build the registry on every call; it is now built once and cloned. Repeated calls
// must return the same entries in the same order.
func TestAllSameOrderAndContentAcrossCalls(t *testing.T) {
	first := All()
	for i := 0; i < 3; i++ {
		next := All()
		if len(next) != len(first) {
			t.Fatalf("All() length changed between calls: %d then %d", len(first), len(next))
		}
		for j := range first {
			if next[j] != first[j] {
				t.Fatalf("All() entry %d differs between calls: %+v then %+v", j, first[j], next[j])
			}
		}
	}
}

// Each All() call returns a fresh slice: mutating a result must not corrupt the registry.
func TestAllReturnsIndependentCopy(t *testing.T) {
	mutated := All()
	mutated[0].ID = "mutated.test"
	if All()[0].ID == "mutated.test" {
		t.Fatal("All() shares its backing array with callers")
	}
}

// Lookup used to scan All(); it now answers from an index. Every entry All() returns must resolve
// to the same descriptor.
func TestLookupParityWithAll(t *testing.T) {
	for _, want := range All() {
		got, ok := Lookup(want.ID)
		if !ok {
			t.Fatalf("Lookup(%q) misses an entry All() returns", want.ID)
		}
		if got != want {
			t.Fatalf("Lookup(%q) = %+v, want %+v", want.ID, got, want)
		}
		if IsExecutable(want.ID) != (want.Availability == Executable) {
			t.Fatalf("IsExecutable(%q) disagrees with Availability %q", want.ID, want.Availability)
		}
	}
	if _, ok := Lookup("no.such.action"); ok {
		t.Fatal("Lookup found an action that does not exist")
	}
}

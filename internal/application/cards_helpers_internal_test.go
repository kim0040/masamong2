package application

import "testing"

func TestOverlap(t *testing.T) {
	if got := overlap(nil, map[string]bool{"a": true}); got != 0 {
		t.Fatalf("an empty side scores %f", got)
	}
	if got := overlap(map[string]bool{"a": true, "b": true}, map[string]bool{"a": true, "b": true}); got != 1 {
		t.Fatalf("identical sets score %f", got)
	}
	if got := overlap(map[string]bool{"a": true}, map[string]bool{"a": true, "b": true, "c": true, "d": true}); got != 1 {
		t.Fatalf("a full subset scores %f, not 1/4", got)
	}
	if got := overlap(map[string]bool{"a": true, "b": true}, map[string]bool{"b": true, "c": true}); got != 0.5 {
		t.Fatalf("one shared token scores %f", got)
	}
}

func TestRelationKeyOrdersBothDirections(t *testing.T) {
	a, b := relationKey("x", "y")
	if a != "x" || b != "y" {
		t.Fatalf("(%q, %q)", a, b)
	}
	a, b = relationKey("y", "x")
	if a != "x" || b != "y" {
		t.Fatalf("the same pair must order the same way: (%q, %q)", a, b)
	}
	a, b = relationKey("same", "same")
	if a != "same" || b != "same" {
		t.Fatalf("(%q, %q)", a, b)
	}
}

func TestRelCursorKey(t *testing.T) {
	if got := relCursorKey("g1", "c2"); got != "cards:relations:g1:c2" {
		t.Fatalf("%q", got)
	}
}

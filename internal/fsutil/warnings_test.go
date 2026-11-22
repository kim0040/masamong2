package fsutil

import "testing"

func TestWarningsAreRecordedOncePerKeySortedAndResettable(t *testing.T) {
	ResetWarnings()
	if got := Warnings(); len(got) != 0 {
		t.Fatalf("clean start: %v", got)
	}
	Warn("b-key", "second message")
	Warn("a-key", "first message")
	Warn("b-key", "second message again") // the latest text replaces, does not duplicate
	got := Warnings()
	if len(got) != 2 || got[0] != "first message" || got[1] != "second message again" {
		t.Fatalf("sorted by key, last write wins: %v", got)
	}
	ResetWarnings()
	if got := Warnings(); len(got) != 0 {
		t.Fatalf("reset did not clear: %v", got)
	}
}

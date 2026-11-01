package tui

import "testing"

func TestJSONNeverRich(t *testing.T) {
	if UseRich("json", true, false) {
		t.Fatal("json must stay plain")
	}
	if UseRich("table", true, true) {
		t.Fatal("no-color disables rich")
	}
	if !UseRich("table", true, false) {
		t.Fatal("tty table should be rich")
	}
}

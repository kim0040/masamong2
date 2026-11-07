package config

import (
	"strings"
	"testing"
)

// memory.warm_query: empty and "on" mean on, "off" is off, anything else is refused, and the default
// document writes nothing for it.
func TestWarmQuerySwitch(t *testing.T) {
	if !(MemoryFlags{}).WarmQueryOn() {
		t.Fatal("an unset memory.warm_query is not on")
	}
	d := Default("a", "/tmp/x")
	if !d.Memory.WarmQueryOn() {
		t.Fatal("the default document does not have warm_query on")
	}
	b, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "warm_query") {
		t.Fatalf("an unset field is written:\n%s", b)
	}
	for value, wantOn := range map[string]bool{"": true, "on": true, "off": false} {
		d.Memory.WarmQuery = value
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(b)
		if err != nil {
			t.Fatalf("%q refused: %v", value, err)
		}
		if got.Memory.WarmQuery != value || got.Memory.WarmQueryOn() != wantOn {
			t.Fatalf("%q read back as %q, on=%v, want on=%v", value, got.Memory.WarmQuery, got.Memory.WarmQueryOn(), wantOn)
		}
	}
	for _, bad := range []string{"true", "ON", "auto", "yes", "0"} {
		d.Memory.WarmQuery = bad
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(b); err == nil || !strings.Contains(err.Error(), "memory.warm_query must be on or off") {
			t.Fatalf("%q was not refused as expected: %v", bad, err)
		}
	}
}

package config

import "testing"

func TestLogLevelValidation(t *testing.T) {
	for _, lv := range []string{"", "debug", "info", "warn", "error"} {
		d := Default("a", "/tmp/x")
		d.Instance.LogLevel = lv
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(b)
		if err != nil || got.Instance.LogLevel != lv {
			t.Fatalf("%q: %v %q", lv, err, got.Instance.LogLevel)
		}
	}
	d := Default("a", "/tmp/x")
	d.Instance.LogLevel = "loud"
	b, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b); err == nil {
		t.Fatal("log_level loud accepted")
	}
}

package config

import (
	"strings"
	"testing"

	"masamong/internal/tools"
)

func TestParseCommittedSwitchesOffWhatThePolicyNowRefuses(t *testing.T) {
	d := Default("i", "/data")
	d.Providers["proxy"] = Provider{Kind: KindOpenAI, BaseURL: "https://u:p@gw.example/v1"}
	off := tools.Settings{Description: "x", HTTP: &tools.HTTP{URL: "http://localhost.internal/api"}}
	d.Tools = map[string]tools.Settings{"my.tool": off}
	raw, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), "user name or password") {
		t.Fatalf("a new change is refused with the reason: %v", err)
	}
	got, problems, err := ParseCommitted(raw)
	if err != nil {
		t.Fatalf("a committed document is loaded: %v", err)
	}
	if !got.Providers["proxy"].Disabled {
		t.Fatal("provider still on")
	}
	if e := got.Tools["my.tool"].Enabled; e == nil || *e {
		t.Fatal("tool still on")
	}
	if len(problems) != 2 {
		t.Fatalf("problems: %+v", problems)
	}
	// other errors still stop the load
	bad := strings.Replace(string(raw), "schema_version: 1", "schema_version: 9", 1)
	if _, _, err := ParseCommitted([]byte(bad)); err == nil {
		t.Fatal("an unsupported schema must still fail")
	}
}

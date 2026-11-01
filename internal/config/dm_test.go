package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDMDefaultsValidationAndSchema(t *testing.T) {
	d := Default("test", "/tmp/dm-config")
	raw, _ := Marshal(d)
	if strings.Contains(string(raw), "dm:") {
		t.Fatal("unset DM section must be omitted from YAML")
	}
	got, err := Parse(raw)
	if err != nil || got.DM.AllowMode() != "members" || got.DM.UserDaily() != 30 || got.DM.Daily() != 100 {
		t.Fatal("DM defaults")
	}
	for _, dm := range []DMSettings{{}, {Allow: "members", PerUserPerDay: 1, PerDay: 1}, {Allow: "operators", PerUserPerDay: -1, PerDay: -1}, {Allow: "off", PerUserPerDay: 10000, PerDay: 100000}} {
		d.DM = dm
		raw, _ = Marshal(d)
		if _, err := Parse(raw); err != nil {
			t.Fatal(err)
		}
		raw, _ = json.Marshal(d)
		if _, err := Parse(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		dm   DMSettings
		path string
	}{
		{DMSettings{Allow: "all"}, "dm.allow"}, {DMSettings{PerUserPerDay: -2}, "dm.per_user_per_day"}, {DMSettings{PerUserPerDay: 10001}, "dm.per_user_per_day"}, {DMSettings{PerDay: -2}, "dm.per_day"}, {DMSettings{PerDay: 100001}, "dm.per_day"},
	} {
		d.DM = tc.dm
		raw, _ = Marshal(d)
		if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Fatalf("expected validation for %s", tc.path)
		}
	}
	var node *SchemaNode
	for _, f := range Schema().Fields {
		if f.Key == "dm" {
			node = f.Node
		}
	}
	if node == nil || len(node.Fields) != 3 {
		t.Fatal("DM schema fields")
	}
	for _, f := range node.Fields {
		switch f.Key {
		case "allow":
			if len(f.Node.Enum) != 3 || f.Node.Implied != "members" {
				t.Fatal("DM allow schema")
			}
		case "per_user_per_day":
			if f.Node.Implied != 30 {
				t.Fatal("DM user default")
			}
		case "per_day":
			if f.Node.Implied != 100 {
				t.Fatal("DM global default")
			}
		}
	}
}

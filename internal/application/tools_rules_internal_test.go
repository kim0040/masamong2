package application

import (
	"reflect"
	"testing"

	"masamong/internal/tools"
)

func TestToolSurfaceAndInputRules(t *testing.T) {
	chatTool := tools.Resolved{ID: "x", Chat: true}
	commandTool := tools.Resolved{ID: "x", Commands: true}
	for _, tc := range []struct {
		name          string
		r             tools.Resolved
		via           string
		override, has bool
		ok            bool
		key           string
	}{
		{"channel turned it off", chatTool, viaChat, false, true, false, "tool.channel_off"},
		{"chat tool in chat", chatTool, viaChat, false, false, true, ""},
		{"command tool in chat", commandTool, viaChat, false, false, false, "tool.not_for_surface"},
		{"command tool turned on for the channel", commandTool, viaTask, true, true, true, ""},
		{"chat tool as a command", chatTool, "command", false, false, false, "tool.not_for_surface"},
		{"command tool as a command", commandTool, "command", false, false, true, ""},
		{"console", tools.Resolved{ID: "x"}, "api", false, false, true, ""},
	} {
		ok, key := toolSurfaceAllows(tc.r, tc.via, tc.override, tc.has)
		if ok != tc.ok || key != tc.key {
			t.Errorf("%s: %v %q", tc.name, ok, key)
		}
	}

	in := map[string]any{"region": "서울", "units": ""}
	fillToolDefaults(in, map[string]any{"region": "부산", "units": "metric", "days": 3})
	if want := map[string]any{"region": "서울", "units": "", "days": 3}; !reflect.DeepEqual(in, want) {
		t.Fatalf("set inputs keep their value: %v", in)
	}
	in = map[string]any{}
	fillToolDefaults(in, map[string]any{"region": "부산"})
	if want := map[string]any{"region": "부산", "region_source": "default"}; !reflect.DeepEqual(in, want) {
		t.Fatalf("a defaulted region is marked: %v", in)
	}

	if got := missingInputs(map[string]any{"a": "x", "b": ""}, []string{"b", "a", "c"}); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("missing: %v", got)
	}
}

package application

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/chat"
	"masamong/internal/lang"
	"masamong/internal/tools"
)

// The tool loop's function schemas come from the registry's inputs and the language pack's hints. The
// golden files pin what the model is shown for a few tools; MASAMONG_UPDATE_GOLDEN=1 rewrites them
// after an intended change.
func TestToolLoopSchemasMatchGolden(t *testing.T) {
	on := true
	all := tools.Resolve(map[string]tools.Settings{"web.read": {Enabled: &on}}, nil)
	ids := []string{"weather.query", "finance.quote", "places.search", "polls.create", "web.read", "search.web", "memory.search"}
	for _, tag := range []string{"ko", "en"} {
		p, _ := lang.Get(tag)
		ts := chat.Toolset{Known: map[string]bool{}}
		for _, r := range all {
			ts.Known[r.ID] = true
		}
		for _, id := range ids {
			r, ok := tools.Find(all, id)
			if !ok {
				t.Fatalf("%s missing", id)
			}
			ts.Known[id] = true
			ts.Enabled = append(ts.Enabled, toolInfo(r, p, nil))
		}
		specs := chat.ToolSchemas(ts)
		type shown struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		var got []shown
		for _, s := range specs {
			got = append(got, shown{s.Name, s.Description, s.Parameters})
			if strings.Contains(string(s.Parameters), `"scope"`) || strings.Contains(string(s.Parameters), `"provider"`) {
				t.Fatalf("%s offers a scope or provider key: %s", s.ID, s.Parameters)
			}
		}
		if len(got) != len(ids)-1 {
			t.Fatalf("%s: memory.search must stay with the planner: %d schemas", tag, len(got))
		}
		raw, _ := json.MarshalIndent(got, "", " ")
		path := filepath.Join("testdata", "tool_schemas_"+tag+".json")
		if os.Getenv("MASAMONG_UPDATE_GOLDEN") != "" {
			if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(want)) != string(raw) {
			t.Fatalf("%s schemas differ from %s:\n%s", tag, path, raw)
		}
	}
}

// The loop knows which tools bring text from the open web and which ones write.
func TestToolInfoMarksWebAndWritingTools(t *testing.T) {
	p, _ := lang.Get("en")
	all := tools.Resolve(map[string]tools.Settings{"x.y": {Description: "op tool", HTTP: &tools.HTTP{URL: "https://example.com/{q}"}, Params: []tools.Param{{Name: "q", Required: true, Description: "the q"}}}}, nil)
	want := map[string][2]bool{"search.web": {true, false}, "web.read": {true, false}, "polls.create": {false, true}, "reminders.propose": {false, true},
		"images.generate": {false, true}, "weather.query": {false, false}, "x.y": {true, false}}
	for id, w := range want {
		r, _ := tools.Find(all, id)
		info := toolInfo(r, p, nil)
		if info.Web != w[0] || info.Writes != w[1] {
			t.Fatalf("%s web=%v writes=%v", id, info.Web, info.Writes)
		}
	}
	r, _ := tools.Find(all, "x.y")
	info := toolInfo(r, p, []tools.Param{{Name: "q", Required: true, Description: "the q"}})
	if info.Purpose != "op tool" || len(info.Params) != 1 || info.Params[0].Hint != "the q" || !info.Params[0].Required {
		t.Fatalf("operator tool: %+v", info)
	}
}

package chat

import "masamong/internal/lang"

// catalogOrder is the product tool order the planner menu used before the tool registry existed.
var catalogOrder = []string{"memory.search", "weather.query", "disasters.query", "finance.quote", "finance.fx", "search.web",
	"images.generate", "polls.create", "help.version"}

var required = map[string][]string{"finance.quote": {"symbol"}, "finance.fx": {"base"}, "search.web": {"query"}, "polls.create": {"question"}}

func koVoice() Voice {
	p, _ := lang.Get("ko")
	return Voice{Pack: p, BotName: "마사몽"}
}

// goldenTools builds a Toolset from the Korean pack (nil = every tool).
func goldenTools(ids []string) Toolset {
	p, _ := lang.Get("ko")
	ts := Toolset{Known: map[string]bool{}}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, id := range catalogOrder {
		ts.Known[id] = true
		if ids == nil || want[id] {
			ts.Enabled = append(ts.Enabled, ToolInfo{ID: id, Description: p.Tools[id].Description, Route: p.Tools[id].Route, Required: required[id]})
		}
	}
	return ts
}

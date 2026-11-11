package lang

import (
	"strings"
	"testing"
)

// Every pack tells the tool loop how deep to search and what queries are (search.web params, read by the
// loop only), and has the deep search's answer line. The planner's fields of the tool stay as they were:
// description and route are pinned elsewhere (golden tests); here they must at least still be there.
func TestPacksCarryTheSearchDepthText(t *testing.T) {
	for _, tag := range Available() {
		p, _ := Get(tag)
		if strings.TrimSpace(p.Chat.ToolConclusionDeep) == "" {
			t.Errorf("%s: chat.tool_conclusion_deep is empty", tag)
		}
		sw := p.Tools["search.web"]
		for _, k := range []string{"depth", "queries"} {
			if strings.TrimSpace(sw.Params[k]) == "" {
				t.Errorf("%s: tools.search.web.params.%s is empty", tag, k)
			}
		}
		if sw.Description == "" || sw.Route == "" {
			t.Errorf("%s: search.web lost its planner text", tag)
		}
		if !strings.Contains(p.Chat.ToolPolicy, "queries") || !strings.Contains(p.Chat.ToolPolicy, "deep") {
			t.Errorf("%s: chat.tool_policy says nothing of deep searches and queries", tag)
		}
	}
}

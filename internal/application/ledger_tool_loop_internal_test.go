package application

import (
	"encoding/json"
	"strings"
	"testing"

	"masamong/internal/chat"
)

// logging.level off and errors keep the member's words out of the turn record, the tool loop's
// arguments included; ops keeps them.
func TestLoggingLevelDropsToolLoopParams(t *testing.T) {
	s := &Service{}
	body := func(mode string) map[string]any {
		trace := &chat.LoopTrace{Mode: mode, Rounds: 1, Stop: "answered", Steps: []chat.LoopStep{
			{Round: 1, Tool: "search.web", State: "ran", Params: map[string]any{"query": "canary-query about Someone"}}}}
		return map[string]any{"message": "canary-query", "answer": "canary-answer", toolLoopKey(trace): trace}
	}
	for _, c := range []struct{ level, mode, key string }{
		{"off", chat.ToolModeLoop, "tool_loop"},
		{"errors", chat.ToolModeLoop, "tool_loop"},
		{"off", chat.ToolModeShadow, "tool_shadow"},
	} {
		out := s.withLoggingLevel(c.level, "answered", nil, body(c.mode))
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "canary-query") || strings.Contains(string(raw), `"params"`) {
			t.Fatalf("level=%s mode=%s kept the arguments: %s", c.level, c.mode, raw)
		}
		// the calls themselves stay: tool, state and round are not the member's words
		if !strings.Contains(string(raw), `"`+c.key+`"`) || !strings.Contains(string(raw), `"tool":"search.web"`) || !strings.Contains(string(raw), `"state":"ran"`) {
			t.Fatalf("level=%s mode=%s lost the call itself: %s", c.level, c.mode, raw)
		}
	}
	// the same query sees the arguments when the level keeps the whole turn
	raw, _ := json.Marshal(s.withLoggingLevel("ops", "answered", nil, body(chat.ToolModeLoop)))
	if !strings.Contains(string(raw), "canary-query about Someone") {
		t.Fatalf("level=ops must keep the arguments: %s", raw)
	}
}

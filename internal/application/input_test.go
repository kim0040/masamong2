package application

import (
	"encoding/json"
	"testing"

	"masamong/internal/action"
)

// CLI flags arrive coerced (ints), GUI forms arrive as strings; string fields must read both,
// and 19-digit Discord snowflakes must survive untouched.
func TestStrValAcceptsNumbersAndKeepsSnowflakes(t *testing.T) {
	req := action.Request{Input: json.RawMessage(`{"guild_id":1003000000000000103,"channel_id":"1004000000000000104","limit":5,"ratio":0.5}`)}
	in := inputMap(req)
	if got := strVal(in, "guild_id", ""); got != "1003000000000000103" {
		t.Fatalf("snowflake lost: %q", got)
	}
	if got := strVal(in, "channel_id", ""); got != "1004000000000000104" {
		t.Fatalf("string id: %q", got)
	}
	if got := strVal(in, "limit", ""); got != "5" {
		t.Fatalf("int as string: %q", got)
	}
	if got := intVal(in, "limit", 0); got != 5 {
		t.Fatalf("intVal: %d", got)
	}
	if got := strVal(in, "missing", "dflt"); got != "dflt" {
		t.Fatalf("fallback: %q", got)
	}
}

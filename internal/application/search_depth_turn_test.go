package application

import (
	"strings"
	"testing"

	"masamong/internal/chat"
)

func searchResult(status string, ok bool, items ...any) chat.ToolResult {
	return chat.ToolResult{Tool: "search.web", OK: ok, Status: status, Data: map[string]any{"status": status, "results": items}}
}

// A turn can search several times at once. A search that failed or came back empty does not keep the reply
// out of the conversation while another search of the same query succeeded. A failed search for a different
// query does, and so does any other tool's failure. Results that name no query share the empty query.
func TestReplyKeptWhenOneSearchFailedAndAnotherSucceeded(t *testing.T) {
	good := searchResult("ok", true, map[string]any{"title": "t"})
	failed := chat.ToolResult{Tool: "search.web", OK: false, Status: "error", Error: "timeout"}
	empty := searchResult("empty", true) // what search.web answers when no service found anything
	weatherDown := chat.ToolResult{Tool: "weather.query", OK: true, Status: "unconfigured"}
	same := func(r chat.ToolResult, query string) chat.ToolResult {
		data := map[string]any{}
		for k, v := range r.Data {
			data[k] = v
		}
		data["query"] = query
		r.Data = data
		return r
	}
	for _, tc := range []struct {
		name    string
		results []chat.ToolResult
		drop    bool
	}{
		{"no tools", nil, false},
		{"one search that worked", []chat.ToolResult{good}, false},
		{"one search that failed", []chat.ToolResult{failed}, true},
		{"one search that found nothing", []chat.ToolResult{empty}, true},
		// a query that is not known proves nothing: the failure still counts
		{"a failed search beside a good one, queries unknown", []chat.ToolResult{failed, good}, true},
		{"a refused search beside a good one, queries unknown", []chat.ToolResult{{Tool: "search.web", OK: false, Status: "refused"}, same(good, "apple")}, true},
		{"an empty search for the same query is no recovery", []chat.ToolResult{same(failed, "apple"), same(empty, "apple")}, true},
		{"an empty search, then a good one for the same query", []chat.ToolResult{same(empty, "apple"), same(good, "apple")}, false},
		{"only failed searches", []chat.ToolResult{failed, empty}, true},
		{"a good search but another tool failed", []chat.ToolResult{good, failed, weatherDown}, true},
		{"another tool failed alone", []chat.ToolResult{weatherDown}, true},
		{"the same query, aside from case and space", []chat.ToolResult{same(failed, "  Apple "), same(good, "apple")}, false},
		{"a failed search for another subject", []chat.ToolResult{same(good, "alpha"), same(failed, "beta")}, true},
		{"an empty search for another subject", []chat.ToolResult{same(good, "alpha"), same(empty, "beta")}, true},
		{"one subject found, another refused", []chat.ToolResult{same(good, "alpha"), same(chat.ToolResult{Tool: "search.web", OK: false, Status: "refused"}, "beta")}, true},
	} {
		if got := toolFailureToReplay(tc.results); got != tc.drop {
			t.Errorf("%s: toolFailureToReplay = %v, want %v", tc.name, got, tc.drop)
		}
	}
}

// A failed web search carries the query it was asked, so it can be matched with a later search for the same
// words; other tools and results that already say their query are left alone.
func TestFailedSearchKeepsItsQuery(t *testing.T) {
	failed := withSearchQuery(chat.ToolResult{Tool: "search.web", Status: "unavailable"}, "search.web", map[string]any{"query": "Apple"})
	if q, _ := failed.Data["query"].(string); q != "Apple" {
		t.Fatalf("failed search lost its query: %v", failed.Data)
	}
	kept := withSearchQuery(chat.ToolResult{Tool: "search.web", OK: true, Data: map[string]any{"query": "own"}}, "search.web", map[string]any{"query": "asked"})
	if kept.Data["query"] != "own" {
		t.Fatalf("query replaced: %v", kept.Data)
	}
	if w := withSearchQuery(chat.ToolResult{Tool: "weather.query"}, "weather.query", map[string]any{"query": "x"}); w.Data != nil {
		t.Fatalf("other tool changed: %v", w.Data)
	}
	if !toolFailureToReplay([]chat.ToolResult{failed}) || toolFailureToReplay([]chat.ToolResult{failed, withSearchQuery(chat.ToolResult{Tool: "search.web", OK: true, Data: map[string]any{"results": []any{1}}}, "search.web", map[string]any{"query": " apple"})}) {
		t.Fatal("a failed search recovered by the same query must not drop the reply")
	}
}

// The turn record keeps 200 characters of each page's text and its length, so a deep search does not put
// ten thousand characters of other people's pages into every record; what the model saw is not touched.
func TestTurnRecordClipsPassages(t *testing.T) {
	long := strings.Repeat("가", 900)
	short := "짧은 문장"
	asAny := searchResult("ok", true,
		map[string]any{"title": "a", "url": "https://example.test/a", "snippet": "s", "passage": long, "read": "ok"},
		map[string]any{"title": "b", "url": "https://example.test/b", "snippet": "s", "passage": short, "read": "ok"},
		map[string]any{"title": "c", "url": "https://example.test/c", "snippet": "s"},
	)
	asMaps := chat.ToolResult{Tool: "search.web", OK: true, Data: map[string]any{"results": []map[string]any{
		{"title": "a", "passage": long}, {"title": "b"},
	}}}
	other := chat.ToolResult{Tool: "weather.query", OK: true, Data: map[string]any{"results": []any{map[string]any{"passage": long}}}}
	in := []chat.ToolResult{asAny, asMaps, other}
	out := recordTools(in)

	first := out[0].Data["results"].([]any)[0].(map[string]any)
	if got := []rune(first["passage"].(string)); len(got) != recordPassageRunes+1 || got[len(got)-1] != '…' || first["passage_runes"] != 900 {
		t.Fatalf("long passage: %d runes, passage_runes %v", len(got), first["passage_runes"])
	}
	if first["title"] != "a" || first["url"] != "https://example.test/a" || first["read"] != "ok" {
		t.Fatalf("the other fields must stay: %+v", first)
	}
	second := out[0].Data["results"].([]any)[1].(map[string]any)
	if second["passage"] != short || second["passage_runes"] != nil {
		t.Fatalf("a short passage must stay whole: %+v", second)
	}
	if third := out[0].Data["results"].([]any)[2].(map[string]any); third["passage"] != nil || third["passage_runes"] != nil {
		t.Fatalf("a result without a passage gained one: %+v", third)
	}
	typed := out[1].Data["results"].([]map[string]any)[0]
	if typed["passage_runes"] != 900 {
		t.Fatalf("typed result list: %+v", typed)
	}
	if out[2].Data["results"].([]any)[0].(map[string]any)["passage"] != long {
		t.Fatal("only search results are clipped")
	}
	// the model's copy is untouched
	if in[0].Data["results"].([]any)[0].(map[string]any)["passage"] != long || in[1].Data["results"].([]map[string]any)[0]["passage"] != long {
		t.Fatal("recordTools changed the turn's own tool results")
	}
}

// A search that read no pages has nothing to clip: the very same slice comes back, so its record is
// byte-identical to what it was.
func TestTurnRecordOfPagelessSearchIsUntouched(t *testing.T) {
	in := []chat.ToolResult{searchResult("ok", true, map[string]any{"title": "a", "snippet": "s", "url": "u", "datetime": "d"}),
		{Tool: "memory.search", OK: true}}
	out := recordTools(in)
	if len(out) != len(in) || &out[0] != &in[0] {
		t.Fatal("results without passages were copied")
	}
	if recordTools(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}

// The names the bot is called by are taken out of what is searched (the web knows nothing of them); a
// query that is only the name, or a word that merely starts with it, is left as it is.
func TestSearchLeavesOutTheBotsName(t *testing.T) {
	names := []string{"마사몽", "masamong"}
	for _, c := range []struct{ in, want string }{
		{"마사몽 젠슨황 불륜 논란", "젠슨황 불륜 논란"},
		{"홍민석 혁명 마사몽", "홍민석 혁명"},
		{`"마사몽" "짜장면" 전주`, `"짜장면" 전주`},
		{"마사몽아, 스페이스X 주가", "스페이스X 주가"},
		{"Masamong SpaceX stock", "SpaceX stock"},
		{"마사몽", "마사몽"},
		{"마사몽2 출시", "마사몽2 출시"},
		{"마사몽의 뜻", "마사몽의 뜻"},
		{"SpaceX 주가", "SpaceX 주가"},
	} {
		if got := withoutBotNames(c.in, names); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
	in := []string{"마사몽 a", "b"}
	if out := withoutBotNamesAll(in, names); out[0] != "a" || in[0] != "마사몽 a" {
		t.Fatalf("reworded queries: %v (the call's list must stay as it was: %v)", out, in)
	}
}

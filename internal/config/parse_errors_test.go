package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// This file pins what parse says about an invalid configuration: the text of every error and the
// order in which independent faults are reported. The faults are listed in the order parse checks
// them; a refactor of parse (one function per section) must keep the golden file and the order
// test unchanged. Regenerate the golden file only for a deliberate change of wording:
//
//	go test ./internal/config -run TestParseErrorsPinned -update
var updateParseGolden = flag.Bool("update", false, "rewrite testdata/parse_errors.golden.json")

type tree = map[string]any

// set writes v at keys, creating the maps on the way.
func set(t tree, v any, keys ...string) {
	cur := t
	for _, k := range keys[:len(keys)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	cur[keys[len(keys)-1]] = v
}

// fault is one way to make the default configuration invalid. Faults on different entries of the
// same map (two providers, two roles) are checked in map order, which is random; entry names the
// entry so the order test only compares faults that share one.
type fault struct {
	name  string
	entry string
	apply func(t tree)
}

// slots groups faults that write the same value (two lists, two halves of one pair): a document
// can hold only one of them, so the order test skips pairs inside a group.
var slots = map[string]string{
	"fallback": "fallbacks", "tool description": "tool", "tool url policy": "tool", "memory.person_aliases": "aliases",
	"participation quiet": "quiet", "admin.web.hosts": "hosts", "web_search.order": "order", "channel attachment": "attachments",
}

// lineNo hides the line number of a decode error: it depends on the other faults in the document.
var lineNo = regexp.MustCompile(`line \d+`)

func noLines(s string) string { return lineNo.ReplaceAllString(s, "line N") }

func slotOf(name string) string {
	for prefix, slot := range slots {
		if strings.HasPrefix(name, prefix) {
			return slot
		}
	}
	return ""
}

func channelFault(name string, v any, keys ...string) fault {
	return fault{name, "guilds:g1/c1", func(t tree) { set(t, v, append([]string{"guilds", "g1", "channels", "c1"}, keys...)...) }}
}

func validAttachment(id, hash string) map[string]any {
	return map[string]any{"id": id, "content_hash": hash, "filename": "f.txt"}
}

func many(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("h%d.example", i)
	}
	return out
}

func parseFaults() []fault {
	prov := func(name string, v any, keys ...string) fault {
		return fault{name, "providers:brave", func(t tree) { set(t, v, append([]string{"providers", "brave"}, keys...)...) }}
	}
	model := func(name string, v any, keys ...string) fault {
		return fault{name, "models:response", func(t tree) { set(t, v, append([]string{"models", "response"}, keys...)...) }}
	}
	fb := func(name string, list ...map[string]any) fault {
		items := make([]any, len(list))
		for i, m := range list {
			items[i] = m
		}
		return model(name, items, "fallbacks")
	}
	top := func(name string, v any, keys ...string) fault {
		return fault{name, "", func(t tree) { set(t, v, keys...) }}
	}
	okFB := func() map[string]any { return map[string]any{"provider": "openrouter", "model": "m-1"} }
	aliases := map[string]any{}
	for i := 0; i < 501; i++ {
		aliases[fmt.Sprintf("p%d", i)] = []string{"a"}
	}
	custom := func(mut func(m map[string]any)) func(t tree) {
		return func(t tree) {
			m := map[string]any{"description": "demo", "http": map[string]any{"url": "https://api.example/x"}}
			mut(m)
			set(t, m, "tools", "ops.demo")
		}
	}
	return []fault{
		top("decode unknown key", 1, "bogus"),
		top("dm.allow", "all", "dm", "allow"),
		top("dm.per_user_per_day", 10001, "dm", "per_user_per_day"),
		top("dm.per_day", 100001, "dm", "per_day"),
		top("schema_version", 2, "schema_version"),
		top("instance.id", "", "instance", "id"),
		top("admin.web.hosts count", many(9), "admin", "web", "hosts"),
		top("admin.web.hosts name", []string{"bad host"}, "admin", "web", "hosts"),
		prov("provider kind", "nope", "kind"),
		prov("provider api", "responses", "api"),
		prov("provider byok_only", map[string]string{"a": "b"}, "byok_only"),
		prov("provider credential_ref", "sk-not-a-reference", "credential_ref"),
		prov("provider base_url", "ftp://example.test", "base_url"),
		top("web_search.order unknown", []string{"nope"}, "web_search", "order"),
		top("web_search.order twice", []string{"brave", "brave"}, "web_search", "order"),
		{"model role", "models:bogus", func(t tree) { set(t, map[string]any{"provider": "openrouter", "model": "m"}, "models", "bogus") }},
		model("model price", -1, "price_in_per_mtok"),
		model("model timeout", 3, "timeout_seconds"),
		{"model effort role", "models:embed", func(t tree) { set(t, "high", "models", "embed", "effort") }},
		model("model effort value", "bogus", "effort"),
		model("model provider", "nope", "provider"),
		{"fallbacks role", "models:embed", func(t tree) { set(t, []any{okFB()}, "models", "embed", "fallbacks") }},
		fb("fallbacks count", okFB(), okFB(), okFB(), okFB(), okFB(), okFB(), okFB(), okFB(), okFB(), okFB()),
		fb("fallback model", map[string]any{"provider": "openrouter", "model": " "}),
		fb("fallback provider", map[string]any{"provider": "nope", "model": "m"}),
		fb("fallback provider kind", map[string]any{"provider": "brave", "model": "m"}),
		fb("fallback duplicate", okFB(), okFB()),
		fb("fallback price", map[string]any{"provider": "openrouter", "model": "m", "price_in_per_mtok": 1001}),
		fb("fallback timeout", map[string]any{"provider": "openrouter", "model": "m", "timeout_seconds": 4}),
		fb("fallback effort", map[string]any{"provider": "openrouter", "model": "m", "effort": "bogus"}),
		top("memory.retrieve", "bogus", "memory", "retrieve"),
		top("chat.tool_model", "bogus", "chat", "tool_model"),
		top("chat.loop_final", "bogus", "chat", "loop_final"),
		top("chat.tool_mode", "bogus", "chat", "tool_mode"),
		top("chat.vision", "bogus", "chat", "vision"),
		top("chat.progress", "bogus", "chat", "progress"),
		top("chat.transcript_chars", 999, "chat", "transcript_chars"),
		top("web_search.read_pages", 11, "web_search", "read_pages"),
		top("web_search.deep_pages", 11, "web_search", "deep_pages"),
		top("web_search.fetch", "bogus", "web_search", "fetch"),
		top("web_search.fetch_daily_limit", 5001, "web_search", "fetch_daily_limit"),
		top("memory.languages", []string{"xx"}, "memory", "languages"),
		{"tool not built in", "tools:nope", func(t tree) { set(t, map[string]any{"description": "x"}, "tools", "nope") }},
		{"tool description", "tools:ops.demo", custom(func(m map[string]any) { m["description"] = " " })},
		{"tool url policy", "tools:ops.demo", custom(func(m map[string]any) { m["http"] = map[string]any{"url": "http://example.test/x"} })},
		top("instance.bot_names", many(9), "instance", "bot_names"),
		top("instance.log_level", "bogus", "instance", "log_level"),
		top("backup.schedule", "bogus", "backup", "schedule"),
		top("backup.hour", 24, "backup", "hour"),
		top("backup.weekday", 7, "backup", "weekday"),
		top("backup.keep", 366, "backup", "keep"),
		top("limits.budget_alert_percent", 101, "limits", "budget_alert_percent"),
		top("limits.max_concurrent_turns", 65, "limits", "max_concurrent_turns"),
		top("limits.max_external_attempts", 21, "limits", "max_external_attempts"),
		top("memory.backfill_days", 31, "memory", "backfill_days"),
		top("memory.index_interval_min", 1441, "memory", "index_interval_min"),
		top("memory.index_workers", 100000, "memory", "index_workers"),
		top("memory.person_aliases count", aliases, "memory", "person_aliases"),
		top("memory.person_aliases entry", map[string]any{"p": []string{}}, "memory", "person_aliases"),
		top("alerts.min_severity", "bogus", "alerts", "min_severity"),
		top("disaster_alerts magnitude", 11, "disaster_alerts", "earthquake_min_domestic"),
		channelFault("channel disaster_alerts", "maybe", "disaster_alerts"),
		channelFault("channel update_news", "maybe", "update_news"),
		channelFault("channel watch_level", map[string]int{"r": 9}, "watch_level"),
		top("participation.mode", "bogus", "participation", "mode"),
		top("participation.min_score", 2, "participation", "min_score"),
		top("participation quiet format", "25:00", "participation", "quiet_start"),
		top("participation quiet pair", "01:00", "participation", "quiet_start"),
		top("memory.day_boundary", "25:00", "memory", "day_boundary"),
		top("memory.derived_budget_usd", "lots", "memory", "derived_budget_usd"),
		top("memory.episodes", "maybe", "memory", "episodes"),
		top("memory.leaf_split", "bogus", "memory", "leaf_split"),
		top("discord.token_ref", "not-a-reference", "discord", "token_ref"),
		{"guild id", "guilds:", func(t tree) { set(t, map[string]any{}, "guilds", "") }},
		{"guild log.channel", "guilds:g1", func(t tree) { set(t, "nope", "guilds", "g1", "log", "channel") }},
		{"channel id", "guilds:g1/", func(t tree) { set(t, map[string]any{}, "guilds", "g1", "channels", "") }},
		channelFault("channel respond", "bogus", "respond"),
		channelFault("channel logging.level", "bogus", "logging", "level"),
		channelFault("channel participation", "bogus", "participation"),
		channelFault("channel follow reply_only", "reply_only", "conversation", "follow"),
		channelFault("channel name_call", "bogus", "name_call"),
		channelFault("channel attribution", "bogus", "attribution"),
		channelFault("channel thread", "bogus", "conversation", "thread"),
		channelFault("channel room_style", "bogus", "conversation", "room_style"),
		channelFault("channel conversation number", 601, "conversation", "follow_window_sec"),
		channelFault("channel retain_days", 3651, "logging", "retain_days"),
		channelFault("channel unknown tool", map[string]bool{"nope.tool": true}, "tools"),
		channelFault("channel attachment fields", []any{map[string]any{"id": "a"}}, "rag_attachments"),
		channelFault("channel attachment stored_name", []any{map[string]any{"id": "a", "content_hash": "h", "filename": "f", "stored_name": "../x"}}, "rag_attachments"),
		channelFault("channel attachment duplicate", []any{validAttachment("a", "h"), validAttachment("b", "h")}, "rag_attachments"),
		top("persona id", map[string]any{"": map[string]any{}}, "personas"),
	}
}

func baseTree(t *testing.T) tree {
	t.Helper()
	raw, err := Marshal(Default("x", "/tmp/x"))
	if err != nil {
		t.Fatal(err)
	}
	var tr tree
	if err := yaml.Unmarshal(raw, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func faulty(t *testing.T, fs ...fault) []byte {
	t.Helper()
	tr := baseTree(t)
	for _, f := range fs {
		f.apply(tr)
	}
	raw, err := yaml.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// outcome is what both entry points say about one document.
type outcome struct {
	Strict   string   `json:"strict"`
	Lenient  string   `json:"lenient"`
	Problems []string `json:"problems,omitempty"`
}

func outcomeOf(raw []byte) outcome {
	var o outcome
	if _, err := Parse(raw); err != nil {
		o.Strict = err.Error()
	}
	_, problems, err := ParseCommitted(raw)
	if err != nil {
		o.Lenient = err.Error()
	}
	for _, p := range problems {
		o.Problems = append(o.Problems, p.Path+" | "+p.Reason+" | "+p.Off)
	}
	return o
}

const parseGolden = "testdata/parse_errors.golden.json"

func TestParseErrorsPinned(t *testing.T) {
	got := map[string]outcome{}
	for _, f := range parseFaults() {
		if _, dup := got[f.name]; dup {
			t.Fatalf("duplicate fault name %q", f.name)
		}
		got[f.name] = outcomeOf(faulty(t, f))
	}
	// raw documents the tree helper cannot build: broken text, JSON input, wrong types
	for name, raw := range map[string]string{
		"raw not yaml":       "schema_version: [1",
		"raw wrong type":     "schema_version: one\n",
		"raw json bad field": `{"schema_version":1,"instance":{"id":"x"},"bogus":true}`,
		"raw empty":          "",
	} {
		got["raw: "+name] = outcomeOf([]byte(raw))
	}
	for name, o := range got {
		if o.Strict == "" && !strings.HasPrefix(name, "channel follow") && o.Lenient == "" && len(o.Problems) == 0 {
			t.Errorf("%s: an invalid document was accepted by both entry points", name)
		}
	}
	if *updateParseGolden {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parseGolden, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(parseGolden)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	var want map[string]outcome
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Errorf("golden has %d cases, the catalogue %d", len(want), len(got))
	}
	for name, o := range got {
		w, ok := want[name]
		if !ok {
			t.Errorf("%s: missing from the golden file", name)
			continue
		}
		if o.Strict != w.Strict || o.Lenient != w.Lenient || !slices.Equal(o.Problems, w.Problems) {
			t.Errorf("%s changed:\n got  %+v\n want %+v", name, o, w)
		}
	}
}

// Two independent faults in one document: parse reports the one it checks first. The catalogue is in
// check order, so for faults i < j the strict error is i's. Faults on different entries of one map
// are reported in map order, so either of the two is accepted there. A committed document reports the
// first fault that still fails; a fault it only repairs (a Problem) does not stop the later ones.
func TestParseErrorOrder(t *testing.T) {
	fs := parseFaults()
	single := make([]outcome, len(fs))
	for i, f := range fs {
		single[i] = outcomeOf(faulty(t, f))
		if single[i].Strict == "" {
			t.Fatalf("%s: a candidate with this fault was accepted", f.name)
		}
	}
	for i := range fs {
		for j := i + 1; j < len(fs); j++ {
			a, b := fs[i], fs[j]
			if slot := slotOf(a.name); slot != "" && slot == slotOf(b.name) {
				continue
			}
			o := outcomeOf(faulty(t, a, b))
			sameMapDifferentEntry := a.entry != "" && b.entry != "" && a.entry != b.entry
			okStrict := noLines(o.Strict) == noLines(single[i].Strict) || sameMapDifferentEntry && noLines(o.Strict) == noLines(single[j].Strict)
			if !okStrict {
				t.Errorf("strict: %q before %q: got %q, want %q", a.name, b.name, o.Strict, single[i].Strict)
			}
			wantLenient := single[i].Lenient
			if wantLenient == "" {
				wantLenient = single[j].Lenient
			}
			okLenient := noLines(o.Lenient) == noLines(wantLenient) || sameMapDifferentEntry && (noLines(o.Lenient) == noLines(single[i].Lenient) || noLines(o.Lenient) == noLines(single[j].Lenient))
			if !okLenient {
				t.Errorf("lenient: %q before %q: got %q, want %q", a.name, b.name, o.Lenient, wantLenient)
			}
		}
	}
}

package lang

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPacksLoadAndCoverTheSameKeys(t *testing.T) {
	av := Available()
	if len(av) < 2 {
		t.Fatalf("packs: %v", av)
	}
	ko, _ := Get("ko")
	for _, tag := range av {
		p, _ := Get(tag)
		for id := range ko.Tools {
			if p.Tools[id].Description == "" {
				t.Errorf("%s: tool %s has no description", tag, id)
			}
		}
		for k := range ko.Chat.RagNotes {
			if _, ok := p.Chat.RagNotes[k]; !ok {
				t.Errorf("%s: rag note %s missing", tag, k)
			}
		}
	}
}

func TestParseRejectsPartialPack(t *testing.T) {
	if _, err := Parse("xx", []byte(`{"name":"x","chat":{},"memory":{},"search":{},"tools":{}}`)); err == nil || !strings.Contains(err.Error(), "chat.planner_header") {
		t.Fatalf("partial pack accepted: %v", err)
	}
	if _, err := Parse("xx", []byte(`{"name":"x","bogus":1}`)); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestResolution(t *testing.T) {
	if For("ko-KR").Tag != "ko" || For("", "en_US").Tag != "en" || For("tlh").Tag != "en" {
		t.Fatal("resolution")
	}
}

func TestPackLookupNormalizesChineseFilename(t *testing.T) {
	if _, ok := Get("zh-Hans"); !ok {
		t.Fatal("the bundled Simplified Chinese pack must load")
	}
	raw, err := packsFS.ReadFile("packs/en.json")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadPacks(fstest.MapFS{
		"packs/en.json":      {Data: raw},
		"packs/zh-Hans.json": {Data: raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded["zh-Hans"]; !ok {
		t.Fatalf("missing normalized pack: %v", loaded)
	}
	original := packs
	packs = loaded
	defer func() { packs = original }()
	if p, ok := Get("zh-Hans-CN-u-nu-hanidec"); !ok || p.Tag != "zh-Hans" {
		t.Fatalf("Get(zh-Hans-CN) = %v, %v", p, ok)
	}
	if For("zh-CN").Tag != "zh-Hans" || For("zh-Hant-u-nu-hanidec").Tag != "en" || For("fr-CA").Tag != "en" || For("fr-u").Tag != "en" {
		t.Fatal("pack alias/fallback mismatch")
	}
	if _, err := loadPacks(fstest.MapFS{"packs/zh.json": {Data: raw}, "packs/zh-Hans.json": {Data: raw}}); err == nil {
		t.Fatal("duplicate normalized pack accepted")
	}
}

func TestTranslatedPackRejectsPlaceholderAndSchemaDrift(t *testing.T) {
	raw, err := packsFS.ReadFile("packs/en.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, field, value, errorPart string
	}{
		{"missing placeholder", "answer_template", "No {who} or {message}", "chat.answer_template"},
		{"extra placeholder", "user_label_named", "{name} {name}", "chat.user_label_named"},
		{"wrong schema key", "plan_output_schema", "wrong shape", "chat.plan_output_schema"},
		{"invalid schema", "plan_output_schema", "{invalid", "chat.plan_output_schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			chat := data["chat"].(map[string]any)
			if tc.value == "wrong shape" {
				chat[tc.field] = strings.Replace(chat[tc.field].(string), `"tool_plan"`, `"wrong_plan"`, 1)
			} else {
				chat[tc.field] = tc.value
			}
			bad, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadPacks(fstest.MapFS{"packs/en.json": {Data: raw}, "packs/fr.json": {Data: bad}}); err == nil || !strings.Contains(err.Error(), tc.errorPart) {
				t.Fatalf("invalid translation accepted or wrong error: %v", err)
			}
		})
	}
}

func TestTranslatedPackProtectsPlannerEnumsAndToolRoutes(t *testing.T) {
	raw, err := packsFS.ReadFile("packs/en.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, before, after string
	}{
		{"memory requirement", `"none|optional|required"`, `"aucun|facultatif|obligatoire"`},
		{"time scope", `"recent|any"`, `"recent|tout"`},
		{"time kind", `"any|recent|today|yesterday|last_week|this_month|before|after|between"`, `"any|recent|today|yesterday|last_week|this_month|before|after|between|tomorrow"`},
		{"question kind", `"person_intro|person_fact|event|casual|other"`, `"intro|person_fact|event|casual|other"`},
		{"tool name sentinel", `"tool_name":"..."`, `"tool_name":"translated"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			chat := data["chat"].(map[string]any)
			schema := chat["plan_output_schema"].(string)
			changed := strings.Replace(schema, tc.before, tc.after, 1)
			if changed == schema {
				t.Fatalf("schema example missing %s", tc.before)
			}
			chat["plan_output_schema"] = changed
			bad, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse("fr", bad); err == nil || !strings.Contains(err.Error(), "chat.plan_output_schema") {
				t.Fatalf("changed protocol value accepted: %v", err)
			}
		})
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	chat := data["chat"].(map[string]any)
	chat["plan_output_schema"] = strings.Replace(chat["plan_output_schema"].(string), "names or nicknames the question refers to, spelled as in the chat", "noms et surnoms", 1)
	localized, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("fr", localized); err != nil {
		t.Fatalf("localized prose example rejected: %v", err)
	}

	for _, tc := range []struct{ name, id, route string }{
		{"dropped", "weather.query", ""},
		{"mangled", "weather.query", "weather.querying"},
		{"other tool", "finance.fx", "route: finance.quote"},
	} {
		t.Run(tc.name+" route", func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			tool := data["tools"].(map[string]any)[tc.id].(map[string]any)
			tool["route"] = tc.route
			bad, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse("fr", bad); err == nil || !strings.Contains(err.Error(), "tools."+tc.id+".route") {
				t.Fatalf("invalid route accepted: %v", err)
			}
		})
	}
}

func TestFillIsSinglePass(t *testing.T) {
	got := Fill(`{"a":{...}} {who}: {message}`, "who", "철수", "message", "{who} {memory}")
	if got != `{"a":{...}} 철수: {who} {memory}` {
		t.Fatal(got)
	}
}

func TestMergeSearchUnions(t *testing.T) {
	s := MergeSearch([]string{"ko", "en"}, []string{"마사몽"})
	has := func(list []string, w string) bool {
		for _, x := range list {
			if x == w {
				return true
			}
		}
		return false
	}
	if !has(s.Pronouns, "걔") || !has(s.Pronouns, "he") || !has(s.Pronouns, "마사몽") || !s.LatinWords || s.Markers.HeadCut != "…(앞 생략) " {
		t.Fatalf("%+v", s)
	}
	if d := MergeSearch(nil, nil); d.Particles == "" {
		t.Fatal("empty list must fall back to the default pack")
	}
}

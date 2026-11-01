package lang

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"masamong/internal/i18n"
)

// validateTranslation compares shared text fields against the English pack.
// Lists (notably search words/scripts and placeholder_words) are intentionally
// language-specific; absent optional map entries and routes without an English
// counterpart are allowed.
func validateTranslation(p *Pack) error {
	raw, err := packsFS.ReadFile("packs/en.json")
	if err != nil {
		return err
	}
	var english Pack
	if err := json.Unmarshal(raw, &english); err != nil {
		return fmt.Errorf("english pack: %w", err)
	}
	if err := compareText("", reflect.ValueOf(english), reflect.ValueOf(*p)); err != nil {
		return err
	}
	for id, tool := range english.Tools {
		if tool.Route == "" {
			continue
		}
		route := p.Tools[id].Route
		// A suffix such as weather.querying is not the weather.query tool ID.
		idPattern := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(id) + `($|[^A-Za-z0-9_.-])`)
		if !idPattern.MatchString(route) {
			return fmt.Errorf("tools.%s.route: must contain tool ID %q", id, id)
		}
	}
	return comparePlanSchema(english.Chat.PlanOutputSchema, p.Chat.PlanOutputSchema)
}

func compareText(path string, base, translated reflect.Value) error {
	switch base.Kind() {
	case reflect.Struct:
		for i := 0; i < base.NumField(); i++ {
			field := base.Type().Field(i)
			name := strings.SplitN(field.Tag.Get("json"), ",", 2)[0]
			if name == "-" {
				continue
			}
			if err := compareText(strings.TrimPrefix(path+"."+name, "."), base.Field(i), translated.Field(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		var keys []string
		for _, key := range base.MapKeys() {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		for _, key := range keys {
			other := translated.MapIndex(reflect.ValueOf(key))
			if other.IsValid() {
				if err := compareText(path+"."+key, base.MapIndex(reflect.ValueOf(key)), other); err != nil {
					return err
				}
			}
		}
	case reflect.String:
		// Required empty strings are rejected by missing; optional empty fields
		// are absent translations rather than mismatched templates.
		if translated.String() != "" && !i18n.SamePlaceholders(base.String(), translated.String()) {
			return fmt.Errorf("%s: placeholders differ from English", path)
		}
	}
	return nil
}

// The planner schema is a JSON example except for its documented {...}
// shorthand. Keys, nesting, types, enum values and the tool-name sentinel
// are protocol; other example strings are translatable prose.
func comparePlanSchema(base, translated string) error {
	decode := func(s string) (any, error) {
		var v any
		err := json.Unmarshal([]byte(strings.ReplaceAll(s, "{...}", "{}")), &v)
		return v, err
	}
	original, err := decode(base)
	if err != nil {
		return fmt.Errorf("english chat.plan_output_schema: %w", err)
	}
	candidate, err := decode(translated)
	if err != nil {
		return fmt.Errorf("chat.plan_output_schema: %w", err)
	}
	if !sameSchema("", original, candidate) {
		return fmt.Errorf("chat.plan_output_schema: shape or fixed values differ from English")
	}
	return nil
}

func sameSchema(path string, a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for key, value := range x {
			other, exists := y[key]
			if !exists || !sameSchema(strings.TrimPrefix(path+"."+key, "."), value, other) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameSchema(fmt.Sprintf("%s[%d]", path, i), x[i], y[i]) {
				return false
			}
		}
		return true
	default:
		if reflect.TypeOf(a) != reflect.TypeOf(b) {
			return false
		}
		switch path {
		case "memory_requirement", "time_scope", "time.kind", "question_kind", "tool_plan[0].tool_name":
			return a == b
		}
		return true
	}
}

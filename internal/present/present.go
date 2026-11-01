// Package present shapes an Action result for humans without knowing the Action: scalar fields
// become key/value rows, lists of objects become tables, nested objects flatten one level, and the
// raw JSON stays available. Both the web console and the rich CLI render from this so they never
// disagree about what a result contains (ADR-15).
package present

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"masamong/internal/action"
)

type View struct {
	OK       bool
	Action   string
	Code     string
	Message  string
	Fields   []KV
	Tables   []Table
	Raw      string
	Revision int
}

type KV struct{ Key, Value string }

type Table struct {
	Key     string
	Columns []string
	Rows    [][]string
}

// skipped keys are rendered elsewhere (message as a sentence, yaml as a block).
var skipped = map[string]bool{"message": true, "yaml": true}

func Build(res action.Result) View {
	v := View{OK: res.OK, Action: res.Action, Revision: res.Revision}
	if !res.OK {
		if res.Error != nil {
			v.Code = res.Error.Code
			v.Message = res.Error.Message
			if len(res.Error.Args) > 0 {
				raw, _ := json.MarshalIndent(res.Error.Args, "", "  ")
				v.Raw = string(raw)
			}
		}
		return v
	}
	if msg, ok := res.Output["message"].(string); ok {
		v.Message = msg
	}
	for _, k := range sortedKeys(res.Output) {
		if skipped[k] {
			continue
		}
		switch x := res.Output[k].(type) {
		case nil:
			continue
		case string, bool, float64, int, int64:
			v.Fields = append(v.Fields, KV{Key: k, Value: fmt.Sprint(x)})
		case []string:
			v.Fields = append(v.Fields, KV{Key: k, Value: strings.Join(x, ", ")})
		case []any:
			if t, ok := tableFromList(k, x); ok {
				v.Tables = append(v.Tables, t)
			} else if len(x) > 0 {
				parts := make([]string, 0, len(x))
				for _, e := range x {
					parts = append(parts, fmt.Sprint(e))
				}
				v.Fields = append(v.Fields, KV{Key: k, Value: strings.Join(parts, ", ")})
			}
		case []map[string]any:
			anys := make([]any, 0, len(x))
			for _, e := range x {
				anys = append(anys, e)
			}
			if t, ok := tableFromList(k, anys); ok {
				v.Tables = append(v.Tables, t)
			}
		case map[string]any:
			// features map (doctor) and similar: map of objects → table keyed by name.
			if t, ok := tableFromMap(k, x); ok {
				v.Tables = append(v.Tables, t)
				continue
			}
			for _, sk := range sortedKeys(x) {
				if IsScalar(x[sk]) {
					v.Fields = append(v.Fields, KV{Key: k + "." + sk, Value: fmt.Sprint(x[sk])})
				}
			}
		default:
			v.Fields = append(v.Fields, KV{Key: k, Value: fmt.Sprint(x)})
		}
	}
	raw, _ := json.MarshalIndent(res.Output, "", "  ")
	v.Raw = string(raw)
	return v
}

func IsScalar(v any) bool {
	switch v.(type) {
	case string, bool, float64, int, int64:
		return true
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func tableFromList(key string, list []any) (Table, bool) {
	if len(list) == 0 {
		return Table{}, false
	}
	rows := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return Table{}, false
		}
		rows = append(rows, m)
	}
	return tableFromRows(key, nil, rows)
}

// tableFromMap handles {"name": {"status": ..}} shapes; every value must be an object.
func tableFromMap(key string, m map[string]any) (Table, bool) {
	if len(m) == 0 {
		return Table{}, false
	}
	names := sortedKeys(m)
	rows := make([]map[string]any, 0, len(m))
	for _, n := range names {
		sub, ok := m[n].(map[string]any)
		if !ok {
			return Table{}, false
		}
		rows = append(rows, sub)
	}
	return tableFromRows(key, names, rows)
}

func tableFromRows(key string, names []string, rows []map[string]any) (Table, bool) {
	colSet := map[string]bool{}
	var cols []string
	for _, m := range rows {
		for k, val := range m {
			if IsScalar(val) && !colSet[k] {
				colSet[k] = true
				cols = append(cols, k)
			}
		}
	}
	if len(cols) == 0 {
		return Table{}, false
	}
	sort.Strings(cols)
	// Conventional first columns keep tables readable: id/name, then status.
	sort.SliceStable(cols, func(i, j int) bool { return rank(cols[i]) < rank(cols[j]) })
	t := Table{Key: key, Columns: cols}
	if names != nil {
		t.Columns = append([]string{"name"}, cols...)
	}
	for i, m := range rows {
		row := make([]string, 0, len(t.Columns))
		if names != nil {
			row = append(row, names[i])
		}
		for _, c := range cols {
			if val, ok := m[c]; ok && val != nil {
				row = append(row, fmt.Sprint(val))
			} else {
				row = append(row, "")
			}
		}
		t.Rows = append(t.Rows, row)
	}
	return t, true
}

func rank(col string) int {
	switch col {
	case "id", "option_id", "symbol", "region":
		return 0
	case "name", "label", "type", "kind", "question":
		return 1
	case "status":
		return 2
	case "progress_pct", "progress_label", "eta_label", "remaining":
		return 3
	}
	return 5
}

package present

import (
	"strings"
	"testing"

	"masamong/internal/action"
)

func result(output map[string]any) action.Result {
	return action.Result{OK: true, Action: "test.action", Output: output}
}

func TestBuildFailure(t *testing.T) {
	v := Build(action.Result{
		OK:     false,
		Action: "x.y",
		Error:  &action.Error{Code: "IO", Message: "failed", Args: map[string]any{"path": "/tmp/x"}},
	})
	if v.OK || v.Code != "IO" || v.Message != "failed" {
		t.Fatalf("%+v", v)
	}
	if !strings.Contains(v.Raw, "/tmp/x") {
		t.Fatalf("error args not in raw view: %q", v.Raw)
	}
}

func TestBuildScalarFieldsSortedAndSkippedKeys(t *testing.T) {
	v := Build(result(map[string]any{
		"message": "done",
		"yaml":    "a: 1\n",
		"zeta":    "last",
		"alpha":   3.5,
		"flag":    true,
		"nilval":  nil,
	}))
	if v.Message != "done" {
		t.Fatalf("message %q", v.Message)
	}
	var keys []string
	for _, f := range v.Fields {
		keys = append(keys, f.Key)
	}
	want := []string{"alpha", "flag", "zeta"}
	if len(keys) != len(want) {
		t.Fatalf("fields %v (yaml and message must be skipped, nil dropped)", keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("fields %v, want %v", keys, want)
		}
	}
	if !strings.Contains(v.Raw, "zeta") {
		t.Fatal("raw keeps everything")
	}
}

func TestBuildStringListAndScalarList(t *testing.T) {
	v := Build(result(map[string]any{
		"names":  []string{"a", "b"},
		"mixed":  []any{1, "two", true},
		"empty":  []any{},
		"object": map[string]any{"inner": "x", "nested": map[string]any{"deep": 1}},
	}))
	got := map[string]string{}
	for _, f := range v.Fields {
		got[f.Key] = f.Value
	}
	if got["names"] != "a, b" {
		t.Fatalf("names %q", got["names"])
	}
	if got["mixed"] != "1, two, true" {
		t.Fatalf("non-table list must join scalars: %q", got["mixed"])
	}
	if _, ok := got["empty"]; ok {
		t.Fatal("an empty list must not become a field")
	}
	if got["object.inner"] != "x" {
		t.Fatalf("nested scalar flattened: %v", got)
	}
	if _, ok := got["object.nested"]; ok {
		t.Fatal("a nested object must not flatten deeper than one level")
	}
}

func TestBuildTableFromObjectList(t *testing.T) {
	v := Build(result(map[string]any{
		"rows": []any{
			map[string]any{"id": "r2", "status": "ok", "note": "y"},
			map[string]any{"id": "r1", "status": "fail"},
		},
	}))
	if len(v.Tables) != 1 {
		t.Fatalf("tables %v", v.Tables)
	}
	tb := v.Tables[0]
	wantCols := []string{"id", "status", "note"} // rank order: id, then status, then the rest alphabetical
	if len(tb.Columns) != len(wantCols) {
		t.Fatalf("columns %v", tb.Columns)
	}
	for i := range wantCols {
		if tb.Columns[i] != wantCols[i] {
			t.Fatalf("columns %v, want %v", tb.Columns, wantCols)
		}
	}
	if len(tb.Rows) != 2 || tb.Rows[0][0] != "r2" || tb.Rows[0][2] != "y" || tb.Rows[1][2] != "" {
		t.Fatalf("rows %v", tb.Rows)
	}
}

func TestBuildTableFromNamedMap(t *testing.T) {
	v := Build(result(map[string]any{
		"features": map[string]any{
			"bfeature": map[string]any{"status": "on"},
			"afeature": map[string]any{"status": "off"},
		},
	}))
	if len(v.Tables) != 1 {
		t.Fatalf("tables %v", v.Tables)
	}
	tb := v.Tables[0]
	if tb.Columns[0] != "name" || tb.Columns[1] != "status" {
		t.Fatalf("a named map gets a name column first: %v", tb.Columns)
	}
	if tb.Rows[0][0] != "afeature" || tb.Rows[1][0] != "bfeature" {
		t.Fatalf("names sort: %v", tb.Rows)
	}
}

func TestIsScalar(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want bool
	}{
		{"s", true}, {3, true}, {int64(3), true}, {2.5, true}, {true, true},
		{nil, false}, {[]string{"a"}, false}, {map[string]any{}, false},
	} {
		if got := IsScalar(tc.v); got != tc.want {
			t.Errorf("IsScalar(%v) = %v", tc.v, got)
		}
	}
}

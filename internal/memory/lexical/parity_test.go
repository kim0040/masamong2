package lexical

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestSandboxFTS5Parity compares this index with the sandbox's SQLite FTS5 output on real leaves.
// It runs only when a parity dump exists (it contains chat text, so it never lives in the repo):
//
//	MASAMONG_PARITY_DIR=<dir with lexical.json> MASAMONG_SANDBOX_DATA=<test-sandbox/data> go test ./internal/memory/lexical/
func TestSandboxFTS5Parity(t *testing.T) {
	dir, data := os.Getenv("MASAMONG_PARITY_DIR"), os.Getenv("MASAMONG_SANDBOX_DATA")
	if dir == "" || data == "" {
		t.Skip("MASAMONG_PARITY_DIR / MASAMONG_SANDBOX_DATA not set")
	}
	var leaves []struct {
		DocID string `json:"doc_id"`
		Body  string `json:"body"`
	}
	var summaries []struct {
		DocID   string `json:"doc_id"`
		Summary string `json:"summary"`
	}
	readJSON(t, filepath.Join(data, "leaves.json"), &leaves)
	readJSON(t, filepath.Join(data, "summaries.json"), &summaries)
	var dump struct {
		Queries []struct {
			Q       string   `json:"q"`
			Leaf    [][2]any `json:"leaf"`
			Summary [][2]any `json:"summary"`
		} `json:"queries"`
	}
	readJSON(t, filepath.Join(dir, "lexical.json"), &dump)
	li, _ := NewIndex(2)
	si, _ := NewIndex(2)
	var ld, sd []Doc
	for _, l := range leaves {
		ld = append(ld, Doc{ID: l.DocID, Text: l.Body})
	}
	for _, s := range summaries {
		sd = append(sd, Doc{ID: s.DocID, Text: s.Summary})
	}
	if err := li.AddMany(ld); err != nil {
		t.Fatal(err)
	}
	if err := si.AddMany(sd); err != nil {
		t.Fatal(err)
	}
	check := func(name string, ix *Index, q string, want [][2]any) (orderOK bool) {
		got := ix.Search(q, 0)
		if len(got) != len(want) {
			t.Errorf("%s %q: %d hits, want %d", name, q, len(got), len(want))
			return false
		}
		orderOK = true
		for i := range got {
			wid, wsc := want[i][0].(string), want[i][1].(float64)
			if math.Abs(got[i].Score-wsc) > 1e-9*math.Max(1, math.Abs(wsc)) {
				t.Errorf("%s %q #%d: score %.12f want %.12f", name, q, i, got[i].Score, wsc)
				return false
			}
			if got[i].DocID != wid {
				t.Errorf("%s %q #%d: %s want %s", name, q, i, got[i].DocID, wid)
				return false
			}
		}
		return orderOK
	}
	hits := 0
	for _, qq := range dump.Queries {
		check("leaf", li, qq.Q, qq.Leaf)
		check("summary", si, qq.Q, qq.Summary)
		hits += len(qq.Leaf) + len(qq.Summary)
	}
	t.Logf("queries=%d ranked hits compared=%d", len(dump.Queries), hits)
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

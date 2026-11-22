package lexical

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/testutil"
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
	testutil.LoadJSON(t, filepath.Join(data, "leaves.json"), &leaves)
	testutil.LoadJSON(t, filepath.Join(data, "summaries.json"), &summaries)
	var dump struct {
		Queries []struct {
			Q       string   `json:"q"`
			Leaf    [][2]any `json:"leaf"`
			Summary [][2]any `json:"summary"`
		} `json:"queries"`
	}
	testutil.LoadJSON(t, filepath.Join(dir, "lexical.json"), &dump)
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
	tied := 0 // groups of equal bm25 scores that came back in a different order
	check := func(name string, ix *Index, q string, want [][2]any) (orderOK bool) {
		got := ix.Search(q, 0)
		if len(got) != len(want) {
			t.Errorf("%s %q: %d hits, want %d", name, q, len(got), len(want))
			return false
		}
		sameScore := func(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }
		for i := range got {
			if wsc := want[i][1].(float64); !sameScore(got[i].Score, wsc) {
				t.Errorf("%s %q #%d: score %.12f want %.12f", name, q, i, got[i].Score, wsc)
				return false
			}
		}
		// SQL leaves the order of equal scores open and the two SQLite builds may order them differently,
		// so inside a group of equal scores the documents must be the same set; across groups the order is exact.
		for i := 0; i < len(got); {
			j := i + 1
			for j < len(got) && sameScore(want[j][1].(float64), want[i][1].(float64)) {
				j++
			}
			same := true
			inGroup := map[string]int{}
			for k := i; k < j; k++ {
				inGroup[want[k][0].(string)]++
				inGroup[got[k].DocID]--
				same = same && got[k].DocID == want[k][0].(string)
			}
			for id, n := range inGroup {
				if n != 0 {
					t.Errorf("%s %q #%d-%d: %s differs from the sandbox's documents", name, q, i, j-1, id)
					return false
				}
			}
			if !same {
				tied++
			}
			i = j
		}
		return true
	}
	hits := 0
	for _, qq := range dump.Queries {
		check("leaf", li, qq.Q, qq.Leaf)
		check("summary", si, qq.Q, qq.Summary)
		hits += len(qq.Leaf) + len(qq.Summary)
	}
	t.Logf("queries=%d ranked hits compared=%d tie groups in another order=%d", len(dump.Queries), hits, tied)
}

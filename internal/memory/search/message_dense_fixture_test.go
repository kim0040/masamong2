package search

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

// TestMessageDenseFixture replays testdata/message_dense/fixture.json, the small synthetic case the
// sandbox (experimental/rag-leaf-parent) reads too, so both sides answer the same ranking questions:
// the message run, the final fusion, the reserve slots, the short-message rule and the source family.
// testdata/message_dense/README.md describes the format.
func TestMessageDenseFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/message_dense/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Leaves         []string    `json:"leaves"`
		LeafVectors    [][]float32 `json:"leaf_vectors"`
		MessageVectors []struct {
			Leaf string    `json:"leaf"`
			Vec  []float32 `json:"vec"`
		} `json:"message_vectors"`
		Query []float32 `json:"query"`
		Cases []struct {
			Name      string    `json:"name"`
			Allowed   *[]string `json:"allowed"`
			Limit     int       `json:"limit"`
			RRFWeight float64   `json:"rrf_weight"`
			Expected  struct {
				Scored      int                `json:"messages_scored"`
				MessageIDs  []string           `json:"message_dense_ids"`
				MessageSc   map[string]float64 `json:"message_dense_scores"`
				LeafDense   []string           `json:"leaf_dense_ids"`
				FusedIDs    []string           `json:"fused_ids"`
				FusedScores map[string]float64 `json:"fused_scores"`
			} `json:"expected"`
		} `json:"cases"`
		Guarantee []struct {
			Name           string              `json:"name"`
			Pool           int                 `json:"pool"`
			PerSourceMin   int                 `json:"per_source_min"`
			ReserveMaxFrac float64             `json:"reserve_max_frac"`
			Ordered        []string            `json:"ordered"`
			Runs           map[string][]string `json:"runs"`
			Expected       struct {
				Guaranteed []string            `json:"guaranteed"`
				BySource   map[string][]string `json:"by_source"`
			} `json:"expected"`
		} `json:"guarantee_pool"`
		Eligibility []struct {
			Text       string   `json:"text"`
			Codepoints []string `json:"codepoints"` // hex, for text that cannot be shown (joiners, selectors)
			Note       string   `json:"note"`
			Runes      int      `json:"meaningful_runes"`
			Eligible   bool     `json:"eligible"`
		} `json:"eligibility"`
		SourceFamily map[string]string `json:"source_family"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}

	// the index: two messages per leaf, in the fixture's order
	var msgs []corpus.Message
	for i := range fx.MessageVectors {
		at := time.Date(2026, 9, 20, 10, i, 0, 0, time.UTC)
		msgs = append(msgs, corpus.Message{ObservationID: fmt.Sprintf("fx-o%02d", i), Room: "fx", RoomOriginal: "fx", ActorLabel: "speaker",
			ActorAccount: "a", Content: fmt.Sprintf("zz%02d", i), ParsedTime: at.Format(time.RFC3339)})
	}
	lv := leaves.SlicePerRoom(msgs, leaves.Recipe{Size: 2, Split: leaves.SplitFixed, WithPrefix: true}, 1000, "leaf")
	if len(lv) != len(fx.Leaves) {
		t.Fatalf("%d leaves for %d names", len(lv), len(fx.Leaves))
	}
	name := map[string]string{} // fixture name → doc id
	byID := map[string]string{}
	for i, l := range lv {
		name[fx.Leaves[i]], byID[l.DocID] = l.DocID, fx.Leaves[i]
	}
	lm := Matrix{Dims: len(fx.LeafVectors[0])}
	for _, v := range fx.LeafVectors {
		Normalize(v)
		lm.Data = append(lm.Data, v...)
	}
	ix, err := Build(Default(), msgs, lv, lm, nil, Matrix{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ix.Close)
	mm := Matrix{Dims: len(fx.MessageVectors[0].Vec)}
	var rowOf, leafOf []int32
	for _, m := range fx.MessageVectors {
		Normalize(m.Vec)
		rowOf = append(rowOf, int32(len(rowOf)))
		mm.Data = append(mm.Data, m.Vec...)
		for i, n := range fx.Leaves {
			if n == m.Leaf {
				leafOf = append(leafOf, int32(i))
			}
		}
	}
	if err := ix.SetMessageVectors(mm, rowOf, leafOf); err != nil {
		t.Fatal(err)
	}
	names := func(ids []string) []string {
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = byID[id]
		}
		return out
	}
	closeTo := func(got, want map[string]float64, tol float64) bool {
		if len(got) != len(want) {
			return false
		}
		for k, v := range want {
			g, ok := got[k]
			if !ok || math.Abs(g-v) > tol {
				return false
			}
		}
		return true
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var allowed Set
			if c.Allowed != nil {
				allowed = Set{}
				for _, n := range *c.Allowed {
					allowed[name[n]] = struct{}{}
				}
			}
			ids, scores, scored := ix.messageDense(fx.Query, allowed, c.Limit)
			got := map[string]float64{}
			for id, s := range scores {
				got[byID[id]] = s
			}
			if g := names(ids); !reflect.DeepEqual(g, nilToEmpty(c.Expected.MessageIDs)) && !(len(g) == 0 && len(c.Expected.MessageIDs) == 0) {
				t.Fatalf("message run %v, want %v", g, c.Expected.MessageIDs)
			}
			if scored != c.Expected.Scored || !closeTo(got, c.Expected.MessageSc, 1e-4) {
				t.Fatalf("scored %d (want %d), scores %v want %v", scored, c.Expected.Scored, got, c.Expected.MessageSc)
			}
			on := true
			ix.Cfg.Retrieval.MessageDense = MessageDenseConfig{Enabled: &on, Limit: c.Limit, RRFWeight: c.RRFWeight}
			g := ix.generate(context.Background(), nil, "unrelated words", fx.Query, Intent{}, allowed, false, 100, 200, 150, 1.0, 1.0, nil)
			if gi := names(g.fusion.IDs()); !reflect.DeepEqual(gi, nilToEmpty(c.Expected.FusedIDs)) && !(len(gi) == 0 && len(c.Expected.FusedIDs) == 0) {
				t.Fatalf("fused order %v, want %v", gi, c.Expected.FusedIDs)
			}
			fs := map[string]float64{}
			for _, cand := range g.fusion.Ordered {
				fs[byID[cand.DocID]] = cand.Score
			}
			if !closeTo(fs, c.Expected.FusedScores, 1e-7) {
				t.Fatalf("fused scores %v, want %v", fs, c.Expected.FusedScores)
			}
			if len(c.Expected.LeafDense) > 0 {
				if ld := names(g.fusion.Runs["leaf_dense"]); !reflect.DeepEqual(ld, c.Expected.LeafDense) {
					t.Fatalf("leaf dense order %v, want %v", ld, c.Expected.LeafDense)
				}
			}
		})
	}

	for _, c := range fx.Guarantee {
		t.Run("guarantee: "+c.Name, func(t *testing.T) {
			f := &Fusion{Ordered: cands(c.Ordered...), Runs: c.Runs}
			_, extras, by := guaranteePool(f, c.Pool, c.PerSourceMin, c.ReserveMaxFrac)
			if !reflect.DeepEqual(extras, c.Expected.Guaranteed) || !reflect.DeepEqual(by, c.Expected.BySource) {
				t.Fatalf("guaranteed %v by %v, want %v by %v", extras, by, c.Expected.Guaranteed, c.Expected.BySource)
			}
		})
	}
	for _, c := range fx.Eligibility {
		text := c.Text
		for _, h := range c.Codepoints {
			cp, err := strconv.ParseInt(h, 16, 32)
			if err != nil {
				t.Fatalf("code point %q: %v", h, err)
			}
			text += string(rune(cp))
		}
		if got := MeaningfulRunes(text); got != c.Runes || MessageEligible(text) != c.Eligible {
			t.Errorf("%q (%s): %d runes, eligible %v; fixture says %d, %v", text, c.Note, got, MessageEligible(text), c.Runes, c.Eligible)
		}
	}
	for src, want := range fx.SourceFamily {
		if got := SourceFamily(src); got != want {
			t.Errorf("SourceFamily(%q) = %q, want %q", src, got, want)
		}
	}
}

func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// The canonical message→leaf mapping (ProjectMessages) and the run over it: one vector per observation,
// projected onto every leaf that holds it, in leaf order then message order, ties to the first pair. The
// matrix is kept in observation-ID order on purpose (the order a database scan returns), which differs
// from the canonical order, so the result proves the run never depends on it.
func TestMessageProjectionFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/message_dense/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Projection struct {
			Leaves []struct {
				Name         string   `json:"name"`
				Observations []string `json:"observations"`
			} `json:"leaves"`
			Vectors       map[string][]float32 `json:"observation_vectors"`
			Query         []float32            `json:"query"`
			ExpectedPairs [][2]string          `json:"expected_pairs"`
			Cases         []struct {
				Name     string    `json:"name"`
				Allowed  *[]string `json:"allowed"`
				Limit    int       `json:"limit"`
				Expected struct {
					Scored     int                `json:"messages_scored"`
					MessageIDs []string           `json:"message_dense_ids"`
					MessageSc  map[string]float64 `json:"message_dense_scores"`
				} `json:"expected"`
			} `json:"cases"`
		} `json:"projection"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	pr := fx.Projection
	if len(pr.Leaves) == 0 || len(pr.Cases) == 0 {
		t.Fatal("the fixture has no projection section")
	}
	leafObs := make([][]string, len(pr.Leaves))
	ix := &Index{}
	for i, l := range pr.Leaves {
		leafObs[i] = l.Observations
		ix.Leaves = append(ix.Leaves, leaves.Leaf{DocID: l.Name})
	}
	ids, leafOf := ProjectMessages(leafObs, func(id string) bool { _, ok := pr.Vectors[id]; return ok })
	var pairs [][2]string
	for k, id := range ids {
		pairs = append(pairs, [2]string{id, pr.Leaves[leafOf[k]].Name})
	}
	if !reflect.DeepEqual(pairs, pr.ExpectedPairs) {
		t.Fatalf("pairs %v, want %v", pairs, pr.ExpectedPairs)
	}
	// the matrix rows in observation-ID order
	var order []string
	for id := range pr.Vectors {
		order = append(order, id)
	}
	sort.Strings(order)
	row := map[string]int32{}
	mm := Matrix{Dims: len(pr.Vectors[order[0]])}
	for i, id := range order {
		v := append([]float32(nil), pr.Vectors[id]...)
		Normalize(v)
		mm.Data = append(mm.Data, v...)
		row[id] = int32(i)
	}
	rowOf := make([]int32, len(ids))
	for k, id := range ids {
		rowOf[k] = row[id]
	}
	if err := ix.SetMessageVectors(mm, rowOf, leafOf); err != nil {
		t.Fatal(err)
	}
	for _, c := range pr.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var allowed Set
			if c.Allowed != nil {
				allowed = Set{}
				for _, n := range *c.Allowed {
					allowed[n] = struct{}{}
				}
			}
			got, scores, scored := ix.messageDense(pr.Query, allowed, c.Limit)
			if !reflect.DeepEqual(nilToEmpty(got), nilToEmpty(c.Expected.MessageIDs)) || scored != c.Expected.Scored {
				t.Fatalf("run %v (scored %d), want %v (%d)", got, scored, c.Expected.MessageIDs, c.Expected.Scored)
			}
			for id, want := range c.Expected.MessageSc {
				if math.Abs(scores[id]-want) > 1e-4 {
					t.Fatalf("score of %s: %g, want %g", id, scores[id], want)
				}
			}
		})
	}
}

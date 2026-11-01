package search

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// 실데이터 역전 쌍의 코사인 차이는 Go 1.32e-9, NumPy 2.38e-7이었다.
// 상대 1e-6은 코사인 0.68에서 약 11 ULP이며 이 차이에 약 2.9배 여유다.
// RRF에는 적용하지 않는다: 원점수의 근접 순열을 증명한 뒤 RRF를 다시 계산한다.
const parityRelativeEpsilon = 1e-6

// dense 코사인(이름이 "_dense"로 끝나는 출처)에만 절대 하한을 더한다. float32 내적의 절대 오차는
// 점수 크기와 거의 무관해서, 상대 ε만 쓰면 낮은 코사인에서 더 빡빡해지고 다른 BLAS(Mac Accelerate)에서
// 거짓 실패가 날 수 있다. Windows/OpenBLAS에서 같은 항목의 양쪽 dense 차이 최댓값은 5.52e-7이었다
// (상대 1e-6은 코사인 0.68에서 6.8e-7, 여유 약 1.2배). 2e-6은 그 약 3.6배이고, 제품 검색 동작이 바뀌는
// 회귀(정규화·잘림·양자화 변경)는 1e-3 이상이다. bm25·RRF·재순위 점수에는 쓰지 않는다.
const parityDenseAbsFloor = 2e-6

type parityTree struct {
	Runs []parityTreeRun               `json:"runs"`
	Raw  map[string]map[string]float64 `json:"raw"`
}
type parityTreeRun struct {
	Name   string      `json:"name"`
	IDs    []string    `json:"ids"`
	Weight float64     `json:"weight"`
	Child  *parityTree `json:"child"`
}

func parityNear(a, b float64) bool {
	return parityNearFloor(a, b, 0)
}

func parityNearFloor(a, b, floor float64) bool {
	return !math.IsNaN(a) && !math.IsNaN(b) && !math.IsInf(a, 0) && !math.IsInf(b, 0) &&
		math.Abs(a-b) <= math.Max(parityRelativeEpsilon*math.Max(math.Abs(a), math.Abs(b)), floor)
}

// parityNearSource is parityNear with the dense floor for dense sources.
func parityNearSource(source string, a, b float64) bool {
	if strings.HasSuffix(source, "_dense") {
		return parityNearFloor(a, b, parityDenseAbsFloor)
	}
	return parityNear(a, b)
}

func paritySameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, id := range a {
		if seen[id] {
			return false
		}
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			return false
		}
		delete(seen, id)
	}
	return len(seen) == 0
}

// 모든 역전 쌍을 양쪽 점수로 검사한다. 인접 비교로만 연결하면 ε 사슬이
// ε 밖의 첫 항목/끝 항목 역전을 숨길 수 있다. 집합/누락/중복은 항상 실패한다.
func parityOrder(a, b []string, sa, sb map[string]float64) bool {
	return parityOrderNear(a, b, sa, sb, parityNear)
}

func parityOrderNear(a, b []string, sa, sb map[string]float64, near func(x, y float64) bool) bool {
	if !paritySameSet(a, b) {
		return false
	}
	if reflect.DeepEqual(nonEmpty(a), nonEmpty(b)) {
		return true
	}
	pos := map[string]int{}
	for i, id := range b {
		pos[id] = i
	}
	for i, x := range a {
		for _, y := range a[i+1:] {
			if pos[x] < pos[y] {
				continue
			}
			ax, ok1 := sa[x]
			ay, ok2 := sa[y]
			bx, ok3 := sb[x]
			by, ok4 := sb[y]
			if !ok1 || !ok2 || !ok3 || !ok4 || !near(ax, ay) || !near(bx, by) || !near(ax, bx) || !near(ay, by) {
				return false
			}
		}
	}
	return true
}

func TestParityOrderRejectsRegressions(t *testing.T) {
	a := []string{"a", "b", "c"}
	near := map[string]float64{"a": 0.7, "b": 0.7 + 1e-8, "c": 0.7 + 2e-8}
	cases := []struct {
		name   string
		b      []string
		sa, sb map[string]float64
		ok     bool
	}{
		{"near permutation", []string{"c", "b", "a"}, near, near, true},
		{"outside epsilon", []string{"b", "a", "c"}, map[string]float64{"a": 0.8, "b": 0.7, "c": 0.6}, near, false},
		{"outside epsilon on sandbox", []string{"b", "a", "c"}, near, map[string]float64{"a": 0.8, "b": 0.7, "c": 0.6}, false},
		{"changed set", []string{"a", "b", "d"}, near, near, false},
		{"missing item", []string{"a", "b"}, near, near, false},
		{"duplicate", []string{"a", "a", "c"}, near, near, false},
		{"missing score", []string{"b", "a", "c"}, near, map[string]float64{"a": 0.7}, false},
		{"epsilon chain", []string{"c", "b", "a"}, map[string]float64{"a": 1, "b": 1 + 0.75e-6, "c": 1 + 1.5e-6}, map[string]float64{"a": 1, "b": 1 + 0.75e-6, "c": 1 + 1.5e-6}, false},
		{"non finite", []string{"b", "a", "c"}, map[string]float64{"a": math.NaN(), "b": 0.7, "c": 0.7}, near, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parityOrder(a, tc.b, tc.sa, tc.sb); got != tc.ok {
				t.Fatalf("accepted=%v want=%v", got, tc.ok)
			}
		})
	}
	// the dense floor: a float32 last-bit difference at a low cosine passes, a real change does not,
	// and other sources keep the relative rule only
	if !parityNearSource("leaf_dense", 0.30, 0.30+1.5e-6) || parityNearSource("leaf_dense", 0.30, 0.30+1e-4) ||
		parityNearSource("leaf_lexical", 0.30, 0.30+1.5e-6) || parityNearSource("message_dense", 1e-8, 5e-3) {
		t.Fatal("dense floor")
	}
	if parityNear(1e-8, 2e-8) {
		t.Fatal("absolute epsilon would hide small-score regression")
	}
	if parityOrder([]string{"a", "a"}, []string{"a", "a"}, near, near) {
		t.Fatal("identical duplicates accepted")
	}
}

func TestParityProofRejectsRRFRegression(t *testing.T) {
	raw := map[string]map[string]float64{"leaf_dense": {"a": 0.7, "b": 0.7 + 1e-8}}
	tree := &parityTree{Runs: []parityTreeRun{{Name: "leaf_dense", IDs: []string{"b", "a"}, Weight: 4}}, Raw: raw}
	expected, err := parityReplay(tree, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := dumpMode{Tree: tree, Scores: map[string]parityScore{}}
	for _, c := range expected.Ordered {
		want.Scores[c.DocID] = parityScore{c.Score, c.Raw, c.Sources}
	}
	actual := fuse([]Run{{"leaf_dense", []string{"a", "b"}, 4}}, nil, raw)
	if !parityProof(actual, want) {
		t.Fatal("near source permutation rejected")
	}
	actual.Ordered[0].Score += 1e-8
	if parityProof(actual, want) {
		t.Fatal("RRF arithmetic regression accepted")
	}
	actual.Ordered[0].Score -= 1e-8
	want.Scores["a"] = parityScore{Score: 42}
	if parityProof(actual, want) {
		t.Fatal("corrupt dump score accepted")
	}
}

func TestParityHitOrderRejectsChanges(t *testing.T) {
	raw := map[string]map[string]float64{"leaf_dense": {"a": 0.7, "b": 0.7 + 1e-8}}
	actual := fuse([]Run{{"leaf_dense", []string{"a", "b"}, 4}}, nil, raw)
	expected := fuse([]Run{{"leaf_dense", []string{"b", "a"}, 4}}, nil, raw)
	want := dumpMode{Applied: "hybrid", Scores: map[string]parityScore{}}
	for _, c := range expected.Ordered {
		want.Scores[c.DocID] = parityScore{c.Score, c.Raw, c.Sources}
	}
	if !parityHitOrder([]string{"a", "b"}, []string{"b", "a"}, actual, want, true, nil) {
		t.Fatal("proved hit permutation rejected")
	}
	for _, ids := range [][]string{{"a"}, {"a", "c"}, {"a", "a"}} {
		if parityHitOrder(ids, []string{"b", "a"}, actual, want, true, nil) {
			t.Fatal("hit set regression accepted")
		}
	}
	if parityHitOrder([]string{"a", "b"}, []string{"b", "a"}, actual, want, false, nil) {
		t.Fatal("unproved permutation accepted")
	}
	// lexical 순위는 고정이고 근접 dense 순위만 바뀐다. RRF 값은 단순
	// 맞교환이 아니어도 트리 재생/원점수 증명으로 정당한 hit 순열이다.
	mixedRaw := map[string]map[string]float64{"leaf_dense": {"a": 0.7, "b": 0.7 + 1e-8}, "leaf_lexical": {"a": -24, "b": -24}}
	mixed := fuse([]Run{{"leaf_lexical", []string{"a", "b"}, 1}, {"leaf_dense", []string{"a", "b"}, 4}}, nil, mixedRaw)
	mixedTree := &parityTree{Runs: []parityTreeRun{{Name: "leaf_lexical", IDs: []string{"a", "b"}, Weight: 1}, {Name: "leaf_dense", IDs: []string{"b", "a"}, Weight: 4}}, Raw: mixedRaw}
	mixedExpected, _ := parityReplay(mixedTree, nil, false)
	mixedWant := dumpMode{Applied: "hybrid", Tree: mixedTree, Scores: map[string]parityScore{}}
	for _, c := range mixedExpected.Ordered {
		mixedWant.Scores[c.DocID] = parityScore{c.Score, c.Raw, c.Sources}
	}
	if !parityProof(mixed, mixedWant) || !parityHitOrder([]string{"a", "b"}, []string{"b", "a"}, mixed, mixedWant, true, nil) {
		t.Fatal("mixed RRF near permutation rejected")
	}
	if parityHitOrder([]string{"a", "a"}, []string{"a", "a"}, mixed, mixedWant, true, nil) {
		t.Fatal("identical duplicate hits accepted")
	}
	far := mixedWant.Scores["b"]
	far.Raw = map[string]float64{"leaf_dense": 0.3, "leaf_lexical": -24}
	mixedWant.Scores["b"] = far
	if parityHitOrder([]string{"a", "b"}, []string{"b", "a"}, mixed, mixedWant, true, nil) {
		t.Fatal("hit inversion outside raw epsilon accepted")
	}
	want.Applied = "rerank"
	if parityHitOrder([]string{"a", "b"}, []string{"b", "a"}, actual, want, true, map[string]float64{"a": 0.8, "b": 0.4}) {
		t.Fatal("rerank regression accepted")
	}
}

// 덤프의 RRF 트리를 재생한다. canonical=false는 덤프 자체 점수 검증,
// true는 ε 안의 원점수 순열만 Go 순서로 치환한 엄격한 RRF 검증이다.
func parityReplay(tree *parityTree, actual *Fusion, canonical bool) (*Fusion, error) {
	if tree == nil {
		return nil, fmt.Errorf("score dump missing")
	}
	var runs []Run
	children := map[string]*Fusion{}
	for _, run := range tree.Runs {
		ids := run.IDs
		if run.Child != nil {
			original, err := parityReplay(run.Child, actual, false)
			if err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(nonEmpty(original.IDs()), nonEmpty(ids)) {
				return nil, fmt.Errorf("child order mismatch")
			}
			child := original
			if canonical {
				child, err = parityReplay(run.Child, actual, true)
				if err != nil {
					return nil, err
				}
			}
			children[run.Name] = child
			ids = child.IDs()
		} else if canonical {
			got, ok := actual.Runs[run.Name]
			if !ok {
				return nil, fmt.Errorf("source missing")
			}
			scores := map[string]float64{}
			for _, c := range actual.Ordered {
				if v, ok := c.Raw[run.Name]; ok {
					scores[c.DocID] = v
				}
			}
			name := run.Name
			if !parityOrderNear(got, ids, scores, tree.Raw[name], func(x, y float64) bool { return parityNearSource(name, x, y) }) {
				return nil, fmt.Errorf("source order outside epsilon")
			}
			ids = got
		}
		runs = append(runs, Run{run.Name, ids, run.Weight})
	}
	return fuse(runs, children, tree.Raw), nil
}

func parityProof(actual *Fusion, want dumpMode) bool {
	original, err := parityReplay(want.Tree, actual, false)
	if err != nil {
		return false
	}
	if len(original.Ordered) != len(want.Scores) {
		return false
	}
	for _, c := range original.Ordered {
		s, ok := want.Scores[c.DocID]
		if !ok || c.Score != s.Score || !reflect.DeepEqual(c.Sources, s.Sources) {
			return false
		}
	}
	corrected, err := parityReplay(want.Tree, actual, true)
	if err != nil || !reflect.DeepEqual(corrected.IDs(), actual.IDs()) {
		return false
	}
	for i, c := range actual.Ordered {
		other := corrected.Ordered[i]
		if c.Score != other.Score || !reflect.DeepEqual(c.Sources, other.Sources) {
			return false
		}
	}
	return true
}

func parityHitOrder(got, want []string, actual *Fusion, expected dumpMode, proof bool, finalScores map[string]float64) bool {
	if !paritySameSet(got, want) {
		return false
	}
	if reflect.DeepEqual(got, want) {
		return true
	}
	if !proof {
		return false
	}
	if expected.Applied == "rerank" {
		return parityOrder(got, want, finalScores, expected.hitScores())
	}
	// R0는 출처별 순위가 RRF로 전파됐음을 검사한다. dense의 근접 순열이
	// lexical 순위와 합쳐지면 최종 RRF 값은 단순 맞교환조차 아닐 수 있다.
	// 전체 트리 증명에 더해 최종 역전 쌍에도 근접 원점수/출처 역전이 필요하다.
	pos := map[string]int{}
	for i, id := range want {
		pos[id] = i
	}
	for i, x := range got {
		for _, y := range got[i+1:] {
			if pos[x] < pos[y] {
				continue
			}
			ax, ay := actual.ByID(x), actual.ByID(y)
			bx, ok1 := expected.Scores[x]
			by, ok2 := expected.Scores[y]
			if ax == nil || ay == nil || !ok1 || !ok2 {
				return false
			}
			if ax.Score < ay.Score || bx.Score > by.Score {
				return false
			}
			near := parityNear(ax.Score, ay.Score) && parityNear(bx.Score, by.Score)
			for source, xRank := range ax.Sources {
				yRank, ok1 := ay.Sources[source]
				wx, ok2 := bx.Sources[source]
				wy, ok3 := by.Sources[source]
				ga, ok4 := ax.Raw[source]
				gb, ok5 := ay.Raw[source]
				wa, ok6 := bx.Raw[source]
				wb, ok7 := by.Raw[source]
				if ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7 && (xRank-yRank)*(wx-wy) < 0 &&
					parityNearSource(source, ga, gb) && parityNearSource(source, wa, wb) && parityNearSource(source, ga, wa) && parityNearSource(source, gb, wb) {
					near = true
				}
			}
			if !near {
				return false
			}
		}
	}
	return true
}

func (d dumpMode) hitScores() map[string]float64 {
	out := map[string]float64{}
	for _, h := range d.Hits {
		if h.Score != nil {
			out[h.DocID] = *h.Score
		}
	}
	return out
}

// 순서별 포장 oracle는 Python이 독립적으로 만든다. 선택하기 전에 hits
// 순열은 전체 RRF 증명을 통과해야 하며 같은 근거 집합이어야 한다.
func parityExpectedHits(ids []string, want dumpMode, proof bool) ([]dumpHit, bool) {
	hitIDs := func(hits []dumpHit) []string {
		var out []string
		for _, h := range hits {
			out = append(out, h.DocID)
		}
		return out
	}
	original := hitIDs(want.Hits)
	if reflect.DeepEqual(nonEmpty(ids), nonEmpty(original)) {
		return want.Hits, true
	}
	if !proof || !paritySameSet(ids, original) {
		return nil, false
	}
	for _, variant := range want.PackingVariants {
		if reflect.DeepEqual(ids, hitIDs(variant)) {
			return variant, true
		}
	}
	return nil, false
}

func TestParityPackingVariantRequiresProofAndMembership(t *testing.T) {
	want := dumpMode{Hits: []dumpHit{{DocID: "a", Excerpt: "first"}, {DocID: "b", Excerpt: "second"}}, PackingVariants: [][]dumpHit{
		{{DocID: "b", Excerpt: "repacked first"}, {DocID: "a", Excerpt: "repacked second"}},
		{{DocID: "b"}, {DocID: "c"}},
	}}
	got, ok := parityExpectedHits([]string{"b", "a"}, want, true)
	if !ok || got[0].Excerpt != "repacked first" {
		t.Fatal("independent repacking not selected")
	}
	if _, ok := parityExpectedHits([]string{"b", "a"}, want, false); ok {
		t.Fatal("unproved packing variant accepted")
	}
	if _, ok := parityExpectedHits([]string{"b", "c"}, want, true); ok {
		t.Fatal("changed membership accepted")
	}
	want.PackingVariants = nil
	if _, ok := parityExpectedHits([]string{"b", "a"}, want, true); ok {
		t.Fatal("missing packing oracle accepted")
	}
}

type parityScore struct {
	Score   float64            `json:"score"`
	Raw     map[string]float64 `json:"raw"`
	Sources map[string]int     `json:"sources"`
}

func parityGenerate(ix *Index, query string, vector []float32, it Intent, scope Scope, emb QueryEmbedder, res Result) *Fusion {
	cfg := ix.Cfg
	recovered, _ := res.Trace["recovery_performed"].(bool)
	lexLim, denLim := cfg.searchLimits()
	timeScale, limitScale := 1.0, 1.0
	if recovered {
		limitScale = floatOr(cfg.Retrieval.Recovery.LimitScale, 1.5)
		lexLim, denLim = int(float64(lexLim)*limitScale), int(float64(denLim)*limitScale)
		timeScale = floatOr(cfg.Retrieval.Recovery.TimeWindowScale, 2.0)
	}
	q := FitQuery(vector, ix.LeafVecs.Dims)
	allowed := ix.scopeAllowed(scope)
	gen := ix.generate(context.Background(), emb, query, q, it, allowed, true,
		cfg.poolSize(5, recovered), lexLim, denLim, timeScale, limitScale, nil)
	lexQuery, _ := ix.lexicalQueryExpanded(query, it)
	leaf := ix.hybrid("leaf", ix.leafLex, ix.LeafVecs, ix.leafIDs, query, q, cfg.weights(), gen.allowed, lexLim, denLim, lexQuery, nil)
	for _, c := range gen.fusion.Ordered {
		if l := leaf.ByID(c.DocID); l != nil {
			for k, v := range l.Raw {
				c.Raw[k] = v
			}
		}
	}
	return gen.fusion
}

func parityDiagnose(t *testing.T, ix *Index, query string, vector []float32, it Intent, scope Scope, emb QueryEmbedder, res Result, want dumpMode) {
	f := parityGenerate(ix, query, vector, it, scope, emb, res)
	got := asStrings(res.Trace["fusion_ids"])
	for i, id := range got {
		if i >= len(want.FusionIDs) || id == want.FusionIDs[i] {
			continue
		}
		a, b := f.ByID(id), want.Scores[id]
		if a == nil {
			continue
		}
		finalGo, finalNP := a.Score, b.Score
		if want.Applied == "rerank" {
			if ids, ok := res.Trace["rerank_order"].([]string); ok {
				if scores, ok := res.Trace["rerank_scores"].([]float64); ok {
					for k, candidate := range ids {
						if candidate == id && k < len(scores) {
							finalGo = scores[k]
						}
					}
				}
			}
			if v, ok := want.FinalScores[id]; ok {
				finalNP = v
			}
			for _, h := range res.Hits {
				if h.DocID == id && h.RerankScore != nil {
					finalGo = *h.RerankScore
				}
			}
			if v, ok := want.hitScores()[id]; ok {
				finalNP = v
			}
		}
		t.Logf("numeric rank=%d dense_go=%.17g dense_np=%.17g bm25_go=%.17g bm25_np=%.17g rrf_go=%.17g rrf_np=%.17g final_go=%.17g final_np=%.17g", i+1, a.Raw["leaf_dense"], b.Raw["leaf_dense"], a.Raw["leaf_lexical"], b.Raw["leaf_lexical"], a.Score, b.Score, finalGo, finalNP)
	}
}

type parityRunDiagnostic struct {
	Question int                 `json:"question"`
	Mode     string              `json:"mode"`
	Proof    bool                `json:"proof"`
	FusionGo []int               `json:"fusion_go"`
	FusionNP []int               `json:"fusion_np"`
	HitsGo   []int               `json:"hits_go"`
	HitsNP   []int               `json:"hits_np"`
	Scores   map[int]parityScore `json:"scores_go"`
	Expected map[int]parityScore `json:"scores_np"`
	FinalGo  map[int]float64     `json:"final_go"`
	FinalNP  map[int]float64     `json:"final_np"`
}

func parityNumbers(ix *Index, question int, mode string, f *Fusion, res Result, want dumpMode, proof bool) parityRunDiagnostic {
	d := parityRunDiagnostic{Question: question, Mode: mode, Proof: proof, Scores: map[int]parityScore{}, Expected: map[int]parityScore{}, FinalGo: map[int]float64{}, FinalNP: map[int]float64{}}
	numbers := func(ids []string) []int {
		var out []int
		for _, id := range ids {
			out = append(out, ix.leafPos[id]+1)
		}
		return out
	}
	d.FusionGo, d.FusionNP = numbers(asStrings(res.Trace["fusion_ids"])), numbers(want.FusionIDs)
	for _, c := range f.Ordered {
		d.FinalGo[ix.leafPos[c.DocID]+1] = c.Score
	}
	for id, s := range want.Scores {
		d.FinalNP[ix.leafPos[id]+1] = s.Score
	}
	if ids, ok := res.Trace["rerank_order"].([]string); ok {
		if scores, ok := res.Trace["rerank_scores"].([]float64); ok {
			for k, id := range ids {
				if k < len(scores) {
					d.FinalGo[ix.leafPos[id]+1] = scores[k]
				}
			}
		}
	}
	for id, s := range want.FinalScores {
		d.FinalNP[ix.leafPos[id]+1] = s
	}
	for _, c := range f.Ordered {
		d.Scores[ix.leafPos[c.DocID]+1] = parityScore{c.Score, c.Raw, c.Sources}
	}
	for id, s := range want.Scores {
		d.Expected[ix.leafPos[id]+1] = s
	}
	for _, h := range res.Hits {
		n := ix.leafPos[h.DocID] + 1
		d.HitsGo = append(d.HitsGo, n)
		if h.RerankScore != nil {
			d.FinalGo[n] = *h.RerankScore
		} else if c := f.ByID(h.DocID); c != nil {
			d.FinalGo[n] = c.Score
		}
	}
	for _, h := range want.Hits {
		n := ix.leafPos[h.DocID] + 1
		d.HitsNP = append(d.HitsNP, n)
		if h.Score != nil {
			d.FinalNP[n] = *h.Score
		}
	}
	return d
}

package search

import (
	"math"
	"math/rand/v2"
	"testing"
)

// The loaded index keeps vectors as int8 rows. Cosine scores must stay close to the float32 ones and
// dense top-k must pick (nearly) the same leaves, including for near-duplicate chat messages.
func TestQuantizedMatrixKeepsScoresAndRanking(t *testing.T) {
	const dims, rows, queries, k = 4096, 3000, 50, 10
	r := rand.New(rand.NewPCG(1, 2))
	// embeddings share a common direction and cluster by topic, like real chat embeddings
	common := randUnit(r, dims)
	topics := make([][]float32, 40)
	for i := range topics {
		topics[i] = randUnit(r, dims)
	}
	mix := func() []float32 {
		v := make([]float32, dims)
		t, u := topics[r.IntN(len(topics))], randUnit(r, dims)
		for i := range v {
			v[i] = 0.5*common[i] + 0.6*t[i] + 0.35*u[i]
		}
		Normalize(v)
		return v
	}
	full := Matrix{Dims: dims}
	q8 := NewQuantized(dims, rows)
	for i := 0; i < rows; i++ {
		v := mix()
		full.Data = append(full.Data, v...)
		q8.AppendQuantized(v)
	}
	if q8.Rows() != rows || q8.Bytes()*3 > full.Bytes() {
		t.Fatalf("rows %d, bytes %d vs float32 %d", q8.Rows(), q8.Bytes(), full.Bytes())
	}
	ids := make([]string, rows)
	for i := range ids {
		ids[i] = string(rune('a'+i%26)) + string(rune(i))
	}
	var worst float64
	overlap := 0
	for n := 0; n < queries; n++ {
		q := mix()
		for i := 0; i < rows; i++ {
			worst = math.Max(worst, math.Abs(full.Dot(i, q)-q8.Dot(i, q)))
		}
		want := map[string]bool{}
		for _, s := range denseTop(full, ids, q, nil, k) {
			want[s.ID] = true
		}
		for _, s := range denseTop(q8, ids, q, nil, k) {
			if want[s.ID] {
				overlap++
			}
		}
	}
	if worst > 2e-3 {
		t.Errorf("largest score error %.5f", worst)
	}
	if recall := float64(overlap) / (queries * k); recall < 0.95 {
		t.Errorf("top-%d recall %.3f", k, recall)
	}
	t.Logf("largest score error %.5f, top-%d recall %.3f, %d KB vs %d KB", worst, k, float64(overlap)/(queries*k), q8.Bytes()>>10, full.Bytes()>>10)
}

func TestQuantizeEdgeCases(t *testing.T) {
	m := NewQuantized(3, 2)
	m.AppendQuantized([]float32{0, 0, 0})
	m.AppendQuantized([]float32{-1, 0, 0})
	if got := m.Dot(0, []float32{1, 1, 1}); got != 0 {
		t.Fatalf("zero row: %v", got)
	}
	if got := m.Dot(1, []float32{1, 0, 0}); math.Abs(got+1) > 1e-6 {
		t.Fatalf("negative unit row: %v", got)
	}
}

func randUnit(r *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	Normalize(v)
	return v
}

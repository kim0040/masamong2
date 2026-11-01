package search

import (
	"runtime"
	"sync"
)

// QuantizedRow owns one immutable row; loaders can attach it without a second copy.
type QuantizedRow struct {
	Q     []int8
	Scale float32
}

// QuantizeFloat16Rows converts a bounded loader batch in its original order. Workers
// only own scratch space and distinct output slots: appending to a shared matrix
// would race and change tie order. One-core machines take the direct serial path
// without goroutines or queues; more cores add one float32 scratch row per worker.
func QuantizeFloat16Rows(raw [][]byte) []QuantizedRow {
	out := make([]QuantizedRow, len(raw))
	workers := min(runtime.GOMAXPROCS(0), len(raw))
	run := func(lo, hi int) {
		var scratch []float32
		for i := lo; i < hi; i++ {
			n := len(raw[i]) / 2
			if cap(scratch) < n {
				scratch = make([]float32, n)
			}
			v := DecodeFloat16(raw[i], scratch[:n])
			q := make([]int8, n)
			// WHY share the hot-row encoder: rounding/scale changes must never
			// make a cold rebuild produce different bytes from AppendQuantized.
			out[i] = QuantizedRow{Q: q, Scale: quantizeRow(v, q)}
		}
	}
	if workers <= 1 {
		run(0, len(raw))
		return out
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := len(raw)*w/workers, len(raw)*(w+1)/workers
		wg.Add(1)
		go func() { defer wg.Done(); run(lo, hi) }()
	}
	wg.Wait()
	return out
}

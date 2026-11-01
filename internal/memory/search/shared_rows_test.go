package search

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"
)

func TestSharedMatrixDenseTopParity(t *testing.T) {
	before := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(before)
	const dims, rows = 31, 2100
	old := NewQuantized(dims, rows)
	shared := NewSharedQuantized(dims, rows)
	ids := make([]string, rows)
	for i := 0; i < rows; i++ {
		v := make([]float32, dims)
		for j := range v {
			v[j] = float32((i%17+j*7)%29-14) / 127
		}
		old.AppendQuantized(v)
		shared.AppendQuantized(v)
		ids[i] = fmt.Sprintf("row-%d", i)
	}
	next := NewSharedQuantized(dims, rows)
	for i := 0; i < shared.Rows(); i++ {
		q, s := shared.RowQ(i)
		next.ShareRowQ(q, s)
	}
	q := make([]float32, dims)
	for j := range q {
		q[j] = float32(j%11-5) / 31
	}
	for _, allowed := range []Set{nil, {}, setOf(ids[8], ids[17], ids[37], ids[72])} {
		want := denseTop(old, ids, q, allowed, 40)
		got := denseTop(next, ids, q, allowed, 40)
		if !reflect.DeepEqual(want, got) {
			t.Fatal("score bits, ordering or ties changed")
		}
	}
	// Fresh decoded rows must copy caller scratch; published rows are the only ones shared.
	scratch := []int8{1, 2, 3}
	m := NewSharedQuantized(3, 1)
	m.AppendRowQ(scratch, 1)
	scratch[0] = 99
	row, _ := m.RowQ(0)
	if row[0] != 1 {
		t.Fatal("mutable decoding scratch was retained")
	}
}

package search

import (
	"reflect"
	"runtime"
	"testing"
)

func TestParallelLoadQuantizationIsBitIdentical(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	for _, dims := range []int{1, 16, 1024} {
		var blobs [][]byte
		ref := NewSharedQuantized(dims, 503)
		for row := 0; row < 503; row++ {
			v := make([]float32, dims)
			for j := range v {
				v[j] = float32((row*11+j*7)%29-14) / 127
			}
			raw := EncodeFloat16(v)
			blobs = append(blobs, raw)
			ref.AppendQuantized(DecodeFloat16(raw, nil))
		}
		serial := QuantizeFloat16Rows(blobs)
		runtime.GOMAXPROCS(4)
		parallel := QuantizeFloat16Rows(blobs)
		runtime.GOMAXPROCS(1)
		if !reflect.DeepEqual(serial, parallel) {
			t.Fatal("worker count changed bytes or scales")
		}
		for i, row := range parallel {
			q, scale := ref.RowQ(i)
			if !reflect.DeepEqual(row.Q, q) || row.Scale != scale {
				t.Fatal("old quantization changed")
			}
		}
	}
}

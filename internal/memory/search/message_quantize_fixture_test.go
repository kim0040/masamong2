package search

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// TestQuantizeFixture replays testdata/message_dense/quantize_fixture.json, which the sandbox
// (experimental/rag-leaf-parent, FixtureParityTests) reads too: rows with half-way values (+-0.5, +-1.5, ...
// after scaling), the clip and a zero row must come out as the same int8 values and the same float32 scale
// bytes here and there, from the float32 values and from the float16 words an earlier version stored.
func TestQuantizeFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/message_dense/quantize_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Dims int `json:"dims"`
		Rows []struct {
			Note     string    `json:"note"`
			Values   []float32 `json:"values"`
			Float16  string    `json:"float16_hex"`
			Q        []int8    `json:"q"`
			ScaleHex string    `json:"scale_hex"`
			BlobHex  string    `json:"blob_hex"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Rows) < 5 {
		t.Fatalf("%d rows", len(fx.Rows))
	}
	check := func(t *testing.T, got []byte, wantBlob string, wantQ []int8, wantScale string) {
		t.Helper()
		if h := hex.EncodeToString(got); h != wantBlob {
			t.Fatalf("encoded row %s, want %s", h, wantBlob)
		}
		q, scale, ok := DecodeQuantizedRow(got, nil)
		if !ok || len(q) != len(wantQ) {
			t.Fatalf("decode: ok %v, %d components", ok, len(q))
		}
		for i := range q {
			if q[i] != wantQ[i] {
				t.Fatalf("component %d: %d, want %d", i, q[i], wantQ[i])
			}
		}
		if h := hex.EncodeToString(got[len(got)-4:]); h != wantScale {
			t.Fatalf("scale bytes %s (%g), want %s", h, scale, wantScale)
		}
	}
	for _, r := range fx.Rows {
		t.Run(r.Note, func(t *testing.T) {
			if len(r.Values) != fx.Dims || len(r.Q) != fx.Dims {
				t.Fatalf("row width %d/%d, dims %d", len(r.Values), len(r.Q), fx.Dims)
			}
			check(t, EncodeQuantizedRow(r.Values), r.BlobHex, r.Q, r.ScaleHex)
			// the matrix the index loads holds the same numbers as the stored bytes
			m := NewQuantized(fx.Dims, 1)
			m.AppendQuantized(r.Values)
			q, scale := m.RowQ(0)
			for i := range q {
				if q[i] != r.Q[i] {
					t.Fatalf("matrix component %d: %d, want %d", i, q[i], r.Q[i])
				}
			}
			if _, want, _ := DecodeQuantizedRow(EncodeQuantizedRow(r.Values), nil); scale != want {
				t.Fatalf("matrix scale %g, want %g", scale, want)
			}
			// a row an earlier version stored as float16 is quantised, after the widening to float32, the same way
			f16, err := hex.DecodeString(r.Float16)
			if err != nil {
				t.Fatal(err)
			}
			check(t, EncodeQuantizedRow(DecodeFloat16(f16, nil)), r.BlobHex, r.Q, r.ScaleHex)
		})
	}
}

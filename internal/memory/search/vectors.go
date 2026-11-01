package search

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"sync"
)

// Matrix holds L2-normalized vectors (one row per leaf, summary or message). Float32 rows (Data)
// and contiguous int8 rows (Q, Scale) are used by fixtures; loaded indexes use individually owned int8
// rows with one scale each, so a rebuild can share unchanged rows. Quantization takes about a quarter
// of float32 vector memory, with cosine scores within about 1e-3 of the float32 ones.
type Matrix struct {
	Dims int
	Data []float32

	Q     []int8
	Scale []float32

	// Loaded indexes own small immutable rows instead of one large allocation. A rebuild can share
	// a row without copying it or retaining the other rows of a retired matrix after an erasure.
	qRows [][]int8
}

// Rows is the number of vectors.
func (m Matrix) Rows() int {
	if m.Dims == 0 {
		return 0
	}
	if m.qRows != nil {
		return len(m.qRows)
	}
	if m.Q != nil {
		return len(m.Q) / m.Dims
	}
	return len(m.Data) / m.Dims
}

// Row returns float32 vector i (shares memory). Quantized matrices have no float32 rows; use Dot.
func (m Matrix) Row(i int) []float32 { return m.Data[i*m.Dims : (i+1)*m.Dims] }

// Dot is row i · q.
func (m Matrix) Dot(i int, q []float32) float64 {
	if m.Q != nil {
		row, scale := m.RowQ(i)
		return dotInt8(row, q) * float64(scale)
	}
	return dot(m.Row(i), q)
}

// Bytes is the memory the vectors take.
func (m Matrix) Bytes() int {
	return 4*len(m.Data) + len(m.Q) + 4*len(m.Scale) + len(m.qRows)*(m.Dims+3*strconv.IntSize/8)
}

// NewQuantized returns an empty int8 matrix with room for rows vectors of dims.
func NewQuantized(dims, rows int) Matrix {
	return Matrix{Dims: dims, Q: make([]int8, 0, rows*dims), Scale: make([]float32, 0, rows)}
}

// NewSharedQuantized builds an index whose unchanged rows can survive a swap without being copied.
// Each new row owns its allocation: erasing one does not pin an otherwise unused large vector block.
// Q remains non-nil to identify an int8 matrix; access rows through RowQ, never Q directly.
func NewSharedQuantized(dims, rows int) Matrix {
	return Matrix{Dims: dims, Q: []int8{}, qRows: make([][]int8, 0, rows), Scale: make([]float32, 0, rows)}
}

// RowQ returns int8 row i and its scale (quantized matrices only; shares memory).
func (m Matrix) RowQ(i int) ([]int8, float32) {
	if m.qRows != nil {
		return m.qRows[i], m.Scale[i]
	}
	return m.Q[i*m.Dims : (i+1)*m.Dims], m.Scale[i]
}

// ShareRowQ attaches a published, immutable row. Call only while building a shared matrix, before
// publishing it; neither the old nor the new index may change the row afterwards.
func (m *Matrix) ShareRowQ(q []int8, scale float32) {
	if m.qRows == nil || len(q) != m.Dims {
		panic("search: shared row width or matrix layout mismatch")
	}
	m.qRows = append(m.qRows, q[:len(q):len(q)])
	m.Scale = append(m.Scale, scale)
}

// AppendRowQ adds an already quantized row (one carried over from a previous index).
func (m *Matrix) AppendRowQ(q []int8, scale float32) {
	if m.qRows != nil {
		m.ShareRowQ(append([]int8(nil), q...), scale)
		return
	}
	m.Q = append(m.Q, q...)
	m.Scale = append(m.Scale, scale)
}

// AppendQuantized adds v (len Dims) as an int8 row scaled by its largest component.
func (m *Matrix) AppendQuantized(v []float32) {
	if m.qRows != nil {
		q := make([]int8, len(v))
		scale := quantizeRow(v, q)
		m.ShareRowQ(q, scale)
		return
	}
	// WHY encode directly into the matrix: sharing the cold/hot implementation
	// should not add a temporary allocation for contiguous fixture matrices.
	start := len(m.Q)
	m.Q = slices.Grow(m.Q, len(v))[:start+len(v)]
	scale := quantizeRow(v, m.Q[start:])
	m.Scale = append(m.Scale, scale)
}

// WHY a single row encoder: cold worker batches and hot matrix appends must
// use identical scale and rounding, independent of rebuild path or core count.
func quantizeRow(v []float32, q []int8) float32 {
	scale, inv := quantScale(v)
	for i, x := range v {
		q[i] = quantize(x, inv)
	}
	return scale
}

// quantScale is the scale of an int8 row (its largest component over 127) and the factor to apply to a
// component.
func quantScale(v []float32) (scale, inv float32) {
	var peak float32
	for _, x := range v {
		if x < 0 {
			x = -x
		}
		peak = max(peak, x)
	}
	scale = peak / 127
	if scale > 0 {
		inv = 1 / scale
	}
	return scale, inv
}

func quantize(x, inv float32) int8 {
	r := x * inv
	if r >= 0 {
		r += 0.5
	} else {
		r -= 0.5
	}
	return int8(max(-127, min(127, r)))
}

// EncodeQuantizedRow packs v the way AppendQuantized would store it: len(v) int8 bytes, then the float32
// scale (little endian) — len(v)+4 bytes, about a quarter of the same row as float32 and half of float16.
// A message vector kept in the database in this form loads into the index unchanged, so the stored row
// and the in-memory row are the same quantisation.
func EncodeQuantizedRow(v []float32) []byte {
	scale, inv := quantScale(v)
	out := make([]byte, len(v)+4)
	for i, x := range v {
		out[i] = byte(quantize(x, inv))
	}
	binary.LittleEndian.PutUint32(out[len(v):], math.Float32bits(scale))
	return out
}

// DecodeQuantizedRow unpacks an EncodeQuantizedRow row into dst (allocating when nil or too small) and
// returns the int8 components and the scale; ok is false for a row too short to hold a scale.
func DecodeQuantizedRow(raw []byte, dst []int8) (q []int8, scale float32, ok bool) {
	n := len(raw) - 4
	if n <= 0 {
		return nil, 0, false
	}
	if cap(dst) < n {
		dst = make([]int8, n)
	}
	q = dst[:n]
	for i := 0; i < n; i++ {
		q[i] = int8(raw[i])
	}
	return q, math.Float32frombits(binary.LittleEndian.Uint32(raw[n:])), true
}

// Truncate keeps the first dims components of every float32 row and re-normalizes (Matryoshka, Qwen3 MRL).
func (m Matrix) Truncate(dims int) Matrix {
	if dims <= 0 || dims >= m.Dims {
		return m
	}
	out := Matrix{Dims: dims, Data: make([]float32, m.Rows()*dims)}
	for i := 0; i < m.Rows(); i++ {
		row := out.Data[i*dims : (i+1)*dims]
		copy(row, m.Row(i)[:dims])
		Normalize(row)
	}
	return out
}

// Normalize scales v to unit length in place (sandbox: v /= norm + 1e-9).
func Normalize(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	n := math.Sqrt(s) + 1e-9
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
}

// FitQuery matches a query vector to the stored dimensionality.
func FitQuery(q []float32, dims int) []float32 {
	out := append([]float32(nil), q...)
	if dims > 0 && len(out) > dims {
		out = out[:dims]
	}
	Normalize(out)
	return out
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// dotInt8 is a·b for an int8 row; four float32 lanes keep it as fast as the float32 dot, and each
// lane sums at most a quarter of the terms, well inside float32's precision for unit vectors.
func dotInt8(a []int8, b []float32) float64 {
	b = b[:len(a)]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += float32(a[i]) * b[i]
		s1 += float32(a[i+1]) * b[i+1]
		s2 += float32(a[i+2]) * b[i+2]
		s3 += float32(a[i+3]) * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += float32(a[i]) * b[i]
	}
	return float64(s0) + float64(s1) + float64(s2) + float64(s3)
}

// scores computes q·row for the given rows (all rows when idx == nil), in parallel for large sets.
func (m Matrix) scores(q []float32, idx []int) []float64 {
	n := len(idx)
	if idx == nil {
		n = m.Rows()
	}
	out := make([]float64, n)
	row := func(k int) int {
		if idx == nil {
			return k
		}
		return idx[k]
	}
	workers := runtime.GOMAXPROCS(0)
	if n < 2048 || workers <= 1 {
		for k := 0; k < n; k++ {
			out[k] = m.Dot(row(k), q)
		}
		return out
	}
	var wg sync.WaitGroup
	chunk := (n + workers - 1) / workers
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min(n, (w+1)*chunk)
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for k := lo; k < hi; k++ {
				out[k] = m.Dot(row(k), q)
			}
		}(lo, hi)
	}
	wg.Wait()
	return out
}

// Scored is one dense or lexical hit with its raw score.
type Scored struct {
	ID    string
	Score float64
}

// denseTop returns the top-limit rows by cosine among allowed ids (nil = all), ties by row order.
func denseTop(m Matrix, ids []string, q []float32, allowed Set, limit int) []Scored {
	if m.Rows() == 0 || len(q) < m.Dims {
		// a query shorter than the stored vectors (another embedding space) cannot be scored; the
		// scoring goroutines would panic out of reach of any recover
		return nil
	}
	var idx []int
	if allowed != nil {
		idx = make([]int, 0, len(allowed))
		for i, id := range ids {
			if allowed.Has(id) {
				idx = append(idx, i)
			}
		}
		if len(idx) == 0 {
			return nil
		}
	}
	sims := m.scores(q, idx)
	order := make([]int, len(sims))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return sims[order[a]] > sims[order[b]] })
	if limit >= 0 && len(order) > limit {
		order = order[:limit]
	}
	out := make([]Scored, len(order))
	for k, o := range order {
		r := o
		if idx != nil {
			r = idx[o]
		}
		out[k] = Scored{ID: ids[r], Score: sims[o]}
	}
	return out
}

// ---------------------------------------------------------------------------
// float16 storage and .npy reading (sandbox artifacts are float16 .npy)
// ---------------------------------------------------------------------------

// Float16ToFloat32 converts an IEEE 754 half-precision value.
func Float16ToFloat32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := int32(h>>10) & 0x1f
	frac := uint32(h & 0x3ff)
	switch {
	case exp == 0:
		if frac == 0 {
			return math.Float32frombits(sign)
		}
		// subnormal: normalize
		e := int32(-14)
		for frac&0x400 == 0 {
			frac <<= 1
			e--
		}
		frac &= 0x3ff
		return math.Float32frombits(sign | uint32(e+127)<<23 | frac<<13)
	case exp == 0x1f:
		return math.Float32frombits(sign | 0xff<<23 | frac<<13)
	}
	return math.Float32frombits(sign | uint32(exp-15+127)<<23 | frac<<13)
}

// Float32ToFloat16 rounds to nearest-even half precision (numpy astype(float16)).
func Float32ToFloat16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int32(b>>23) & 0xff
	frac := b & 0x7fffff
	if exp == 0xff {
		if frac != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	}
	e := exp - 127 + 15
	if e >= 0x1f {
		return sign | 0x7c00
	}
	if e <= 0 {
		if e < -10 {
			return sign
		}
		m := frac | 0x800000
		shift := uint32(14 - e)
		half := uint32(1) << (shift - 1)
		rest := m & ((1 << shift) - 1)
		v := m >> shift
		if rest > half || (rest == half && v&1 == 1) {
			v++
		}
		return sign | uint16(v)
	}
	v := uint32(e)<<10 | frac>>13
	rest := frac & 0x1fff
	if rest > 0x1000 || (rest == 0x1000 && v&1 == 1) {
		v++
	}
	return sign | uint16(v)
}

// EncodeFloat16 packs a vector as little-endian float16 (storage format).
func EncodeFloat16(v []float32) []byte {
	out := make([]byte, 2*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint16(out[2*i:], Float32ToFloat16(x))
	}
	return out
}

// DecodeFloat16 unpacks a little-endian float16 vector into dst (allocating when nil).
func DecodeFloat16(raw []byte, dst []float32) []float32 {
	n := len(raw) / 2
	if dst == nil {
		dst = make([]float32, n)
	}
	for i := 0; i < n; i++ {
		dst[i] = Float16ToFloat32(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return dst
}

var npyShape = regexp.MustCompile(`'shape':\s*\((\d+),\s*(\d+)\)`)
var npyDescr = regexp.MustCompile(`'descr':\s*'([<>|]?[fi]\d)'`)

// LoadNPY reads a 2-D float16/float32 C-order .npy file (sandbox leaf_vecs.npy / summary_vecs.npy).
func LoadNPY(path string) (Matrix, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Matrix{}, err
	}
	if len(raw) < 10 || string(raw[1:6]) != "NUMPY" {
		return Matrix{}, errors.New("npy: bad magic")
	}
	var hlen, off int
	if raw[6] == 1 {
		hlen, off = int(binary.LittleEndian.Uint16(raw[8:10])), 10
	} else {
		hlen, off = int(binary.LittleEndian.Uint32(raw[8:12])), 12
	}
	header := string(raw[off : off+hlen])
	data := raw[off+hlen:]
	sm, dm := npyShape.FindStringSubmatch(header), npyDescr.FindStringSubmatch(header)
	if sm == nil || dm == nil {
		return Matrix{}, fmt.Errorf("npy: unsupported header %q", header)
	}
	rows, _ := strconv.Atoi(sm[1])
	dims, _ := strconv.Atoi(sm[2])
	out := Matrix{Dims: dims, Data: make([]float32, rows*dims)}
	switch dm[1] {
	case "<f2":
		DecodeFloat16(data[:2*rows*dims], out.Data)
	case "<f4":
		for i := range out.Data {
			out.Data[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
		}
	default:
		return Matrix{}, fmt.Errorf("npy: dtype %s", dm[1])
	}
	return out, nil
}

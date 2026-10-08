// Package vindex is an in-memory, int8-quantised, brute-force vector index
// for cosine similarity over L2-normalised embeddings. Brute force keeps
// results exact and the code simple; at 512 dims a scan runs at several
// million vectors per second per core.
package vindex

import (
	"container/heap"
	"encoding/binary"
	"math"
	"runtime"
	"sort"
	"sync"
)

// Hit is a search result.
type Hit struct {
	ID    int64
	Score float32 // cosine similarity
}

// Index holds quantised vectors.
type Index struct {
	mu     sync.RWMutex
	dim    int
	ids    []int64
	data   []int8
	scales []float32 // per-vector dequantisation factor
	pos    map[int64]int
}

func New(dim int) *Index { return &Index{dim: dim, pos: map[int64]int{}} }

func (ix *Index) Dim() int { return ix.dim }

func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.ids)
}

func quantize(v []float32, dst []int8) float32 {
	var m float32
	for _, x := range v {
		if x < 0 {
			x = -x
		}
		if x > m {
			m = x
		}
	}
	if m == 0 {
		return 0
	}
	s := 127 / m
	for i, x := range v {
		dst[i] = int8(math.Round(float64(x * s)))
	}
	return 1 / s
}

// Add inserts or replaces a vector.
func (ix *Index) Add(id int64, v []float32) {
	if len(v) != ix.dim {
		return
	}
	q := make([]int8, ix.dim)
	sc := quantize(v, q)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if p, ok := ix.pos[id]; ok {
		copy(ix.data[p*ix.dim:], q)
		ix.scales[p] = sc
		return
	}
	ix.pos[id] = len(ix.ids)
	ix.ids = append(ix.ids, id)
	ix.data = append(ix.data, q...)
	ix.scales = append(ix.scales, sc)
}

// Remove deletes a vector (swap-with-last).
func (ix *Index) Remove(id int64) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	p, ok := ix.pos[id]
	if !ok {
		return
	}
	last := len(ix.ids) - 1
	if p != last {
		ix.ids[p] = ix.ids[last]
		ix.scales[p] = ix.scales[last]
		copy(ix.data[p*ix.dim:(p+1)*ix.dim], ix.data[last*ix.dim:])
		ix.pos[ix.ids[p]] = p
	}
	ix.ids = ix.ids[:last]
	ix.scales = ix.scales[:last]
	ix.data = ix.data[:last*ix.dim]
	delete(ix.pos, id)
}

// Has reports whether id is indexed.
func (ix *Index) Has(id int64) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	_, ok := ix.pos[id]
	return ok
}

// Vector returns the (dequantised) stored vector for id.
func (ix *Index) Vector(id int64) ([]float32, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	p, ok := ix.pos[id]
	if !ok {
		return nil, false
	}
	out := make([]float32, ix.dim)
	for i, q := range ix.data[p*ix.dim : (p+1)*ix.dim] {
		out[i] = float32(q) * ix.scales[p]
	}
	return out, true
}

func dot8(a, b []int8) int32 {
	var s0, s1, s2, s3 int32
	n := len(a) &^ 3
	b = b[:len(a)]
	for i := 0; i < n; i += 4 {
		s0 += int32(a[i]) * int32(b[i])
		s1 += int32(a[i+1]) * int32(b[i+1])
		s2 += int32(a[i+2]) * int32(b[i+2])
		s3 += int32(a[i+3]) * int32(b[i+3])
	}
	for i := n; i < len(a); i++ {
		s0 += int32(a[i]) * int32(b[i])
	}
	return s0 + s1 + s2 + s3
}

type minHeap []Hit

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].Score < h[j].Score }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(Hit)) }
func (h *minHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// SearchOpts tune a query.
type SearchOpts struct {
	K        int                 // max results
	MinScore float32             // drop hits below this similarity
	Filter   func(id int64) bool // optional; return false to skip
}

// Search returns the top-K most similar vectors to q (cosine). With several
// queries, each vector's score is its best similarity to any query (useful
// for "find more of this person" from multiple exemplars).
func (ix *Index) Search(qs [][]float32, o SearchOpts) []Hit {
	if o.K <= 0 {
		o.K = 100
	}
	type qv struct {
		q  []int8
		sc float32
	}
	var qq []qv
	for _, q := range qs {
		if len(q) != ix.dim {
			continue
		}
		b := make([]int8, ix.dim)
		s := quantize(q, b)
		qq = append(qq, qv{b, s})
	}
	if len(qq) == 0 {
		return nil
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := len(ix.ids)
	// never size result heaps beyond the index: a huge K (e.g. from a deep
	// page number) must not allocate gigabytes
	o.K = min(o.K, max(n, 1))
	workers := runtime.GOMAXPROCS(0)
	if n < 20000 {
		workers = 1
	}
	chunk := (n + workers - 1) / workers
	results := make([]minHeap, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min(n, (w+1)*chunk)
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			h := make(minHeap, 0, o.K+1)
			for i := lo; i < hi; i++ {
				row := ix.data[i*ix.dim : (i+1)*ix.dim]
				best := float32(-2)
				for _, q := range qq {
					s := float32(dot8(q.q, row)) * q.sc * ix.scales[i]
					if s > best {
						best = s
					}
				}
				if best < o.MinScore {
					continue
				}
				if len(h) == o.K && best <= h[0].Score {
					continue
				}
				if o.Filter != nil && !o.Filter(ix.ids[i]) {
					continue
				}
				heap.Push(&h, Hit{ix.ids[i], best})
				if len(h) > o.K {
					heap.Pop(&h)
				}
			}
			results[w] = h
		}(w, lo, hi)
	}
	wg.Wait()
	var all []Hit
	for _, h := range results {
		all = append(all, h...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if len(all) > o.K {
		all = all[:o.K]
	}
	return all
}

// ---- float16 storage helpers -------------------------------------------

// EncodeF16 packs a float32 vector as little-endian IEEE half floats.
func EncodeF16(v []float32) []byte {
	b := make([]byte, len(v)*2)
	for i, x := range v {
		binary.LittleEndian.PutUint16(b[i*2:], f32to16(x))
	}
	return b
}

// DecodeF16 unpacks a half-float blob.
func DecodeF16(b []byte) []float32 {
	v := make([]float32, len(b)/2)
	for i := range v {
		v[i] = f16to32(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return v
}

func f32to16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int((b>>23)&0xff) - 127 + 15
	mant := b & 0x7fffff
	switch {
	case exp <= 0:
		if exp < -10 {
			return sign
		}
		mant |= 0x800000
		shift := uint(14 - exp)
		h := uint16(mant >> shift)
		if (mant>>(shift-1))&1 != 0 { // round
			h++
		}
		return sign | h
	case exp >= 31:
		return sign | 0x7c00
	}
	h := sign | uint16(exp)<<10 | uint16(mant>>13)
	if mant&0x1000 != 0 { // round half up
		h++
	}
	return h
}

func f16to32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// subnormal
		e := uint32(127 - 15 + 1)
		for mant&0x400 == 0 {
			mant <<= 1
			e--
		}
		mant &= 0x3ff
		return math.Float32frombits(sign | e<<23 | mant<<13)
	case 31:
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	}
	return math.Float32frombits(sign | (exp+127-15)<<23 | mant<<13)
}

// ForEach calls fn for every stored vector, in parallel across CPUs. fn must
// be safe for concurrent use. The quantised vector is only valid during fn.
func (ix *Index) ForEach(fn func(id int64, q []int8, scale float32)) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := len(ix.ids)
	workers := runtime.GOMAXPROCS(0)
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min(n, (w+1)*chunk)
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				fn(ix.ids[i], ix.data[i*ix.dim:(i+1)*ix.dim], ix.scales[i])
			}
		}(lo, hi)
	}
	wg.Wait()
}

// Quantized is a query vector prepared for repeated DotQ calls.
type Quantized struct {
	Q     []int8
	Scale float32
}

// Quantize prepares v for DotQ.
func Quantize(v []float32) Quantized {
	q := make([]int8, len(v))
	return Quantized{q, quantize(v, q)}
}

// DotQ is the approximate cosine similarity of two quantised vectors.
func DotQ(a Quantized, b []int8, bScale float32) float32 {
	return float32(dot8(a.Q, b)) * a.Scale * bScale
}

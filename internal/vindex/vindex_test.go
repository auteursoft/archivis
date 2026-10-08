package vindex

import (
	"math"
	"math/rand"
	"testing"
)

func randVec(r *rand.Rand, d int) []float32 {
	v := make([]float32, d)
	var s float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		s += float64(v[i] * v[i])
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(s))
	}
	return v
}

func TestSearchExactish(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	ix := New(512)
	vecs := map[int64][]float32{}
	for i := int64(0); i < 30000; i++ {
		v := randVec(r, 512)
		vecs[i] = v
		ix.Add(i, v)
	}
	q := vecs[123]
	// a noisy copy of 123 should be the top hit after 123 itself
	near := append([]float32(nil), q...)
	for i := range near {
		near[i] += float32(r.NormFloat64()) * 0.02
	}
	ix.Add(99999, near)
	hits := ix.Search([][]float32{q}, SearchOpts{K: 5})
	if hits[0].ID != 123 || hits[1].ID != 99999 {
		t.Fatalf("hits %+v", hits)
	}
	if math.Abs(float64(hits[0].Score)-1) > 0.01 {
		t.Fatalf("self score %v", hits[0].Score)
	}
	ix.Remove(123)
	if ix.Has(123) || ix.Len() != 30000 {
		t.Fatal("remove")
	}
	hits = ix.Search([][]float32{q}, SearchOpts{K: 1, Filter: func(id int64) bool { return id != 99999 }})
	if hits[0].ID == 99999 || hits[0].ID == 123 {
		t.Fatal("filter")
	}
	v, _ := ix.Vector(5)
	var d float64
	for i := range v {
		d += float64(v[i] * vecs[5][i])
	}
	if d < 0.999 {
		t.Fatalf("dequantised cos %v", d)
	}
}

func TestF16(t *testing.T) {
	for _, x := range []float32{0, 1, -1, 0.1, -0.0333, 1e-5, 65504, 3.14159} {
		y := DecodeF16(EncodeF16([]float32{x}))[0]
		if math.Abs(float64(x-y)) > math.Abs(float64(x))*1e-3+1e-7 {
			t.Errorf("%v -> %v", x, y)
		}
	}
}

func BenchmarkSearch1M(b *testing.B) {
	r := rand.New(rand.NewSource(1))
	ix := New(512)
	base := randVec(r, 512)
	for i := int64(0); i < 1000000; i++ {
		if i%1000 == 0 {
			base = randVec(r, 512)
		}
		ix.Add(i, base)
	}
	q := randVec(r, 512)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.Search([][]float32{q}, SearchOpts{K: 100})
	}
}

// A vector the filter rejects must never take a heap slot or set the cutoff.
func TestFilterBeforeCutoff(t *testing.T) {
	ix := New(4)
	ix.Add(1, []float32{1, 0, 0, 0})      // best, rejected
	ix.Add(2, []float32{0.9, 0.44, 0, 0}) // accepted
	ix.Add(3, []float32{0.8, 0.6, 0, 0})  // accepted
	ix.Add(4, []float32{0.7, 0.71, 0, 0}) // rejected
	ix.Add(5, []float32{0.6, 0.8, 0, 0})  // accepted
	hits := ix.Search([][]float32{{1, 0, 0, 0}}, SearchOpts{K: 2, Filter: func(id int64) bool { return id != 1 && id != 4 }})
	if len(hits) != 2 || hits[0].ID != 2 || hits[1].ID != 3 {
		t.Fatalf("got %+v, want ids 2 then 3", hits)
	}
}

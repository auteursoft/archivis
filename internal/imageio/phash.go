package imageio

import (
	"bytes"
	"image/jpeg"
	"math"
	"math/bits"
	"sort"
)

var dct32 = func() [8][32]float64 {
	var t [8][32]float64
	for u := 0; u < 8; u++ {
		for x := 0; x < 32; x++ {
			t[u][x] = math.Cos(math.Pi / 32 * (float64(x) + 0.5) * float64(u))
		}
	}
	return t
}()

// PHash computes a 64-bit DCT perceptual hash (same construction as the
// imagehash library's phash with hash_size=8).
func PHash(m *RGB) uint64 {
	small := Resize(m, 32, 32, Bilinear)
	var g [32][32]float64
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			p := small.Pix[(y*32+x)*3:]
			g[y][x] = 0.299*float64(p[0]) + 0.587*float64(p[1]) + 0.114*float64(p[2])
		}
	}
	// rows then columns, only the first 8 coefficients of each are needed
	var r [32][8]float64
	for y := 0; y < 32; y++ {
		for u := 0; u < 8; u++ {
			var s float64
			for x := 0; x < 32; x++ {
				s += g[y][x] * dct32[u][x]
			}
			r[y][u] = s
		}
	}
	var c [64]float64
	for v := 0; v < 8; v++ {
		for u := 0; u < 8; u++ {
			var s float64
			for y := 0; y < 32; y++ {
				s += r[y][u] * dct32[v][y]
			}
			c[v*8+u] = s
		}
	}
	sorted := c
	sort.Float64s(sorted[:])
	med := (sorted[31] + sorted[32]) / 2
	var h uint64
	for i, v := range c {
		if v > med {
			h |= 1 << uint(63-i)
		}
	}
	return h
}

// HammingDistance between two hashes.
func HammingDistance(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// EncodeJPEG encodes the image at the given quality.
func EncodeJPEG(m *RGB, quality int) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(m.W * m.H / 4)
	if err := jpeg.Encode(&buf, m.ToImage(), &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

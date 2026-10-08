package imageio

import "math"

// Filter is a resampling kernel.
type Filter struct {
	Support float64
	Kernel  func(x float64) float64
}

var (
	// Bilinear (triangle) filter, antialiased when downscaling.
	Bilinear = Filter{1, func(x float64) float64 {
		x = math.Abs(x)
		if x < 1 {
			return 1 - x
		}
		return 0
	}}
	// Bicubic with a = -0.5, matching PIL's BICUBIC (used by CLIP preprocessing).
	Bicubic = Filter{2, func(x float64) float64 {
		const a = -0.5
		x = math.Abs(x)
		if x < 1 {
			return ((a+2)*x-(a+3))*x*x + 1
		}
		if x < 2 {
			return (((x-5)*x+8)*x - 4) * a
		}
		return 0
	}}
)

type coeffs struct {
	start []int
	w     [][]float32
}

// precompute mirrors PIL's ImagingResample coefficient computation.
func precompute(inSize, outSize int, f Filter) coeffs {
	scale := float64(inSize) / float64(outSize)
	fs := math.Max(scale, 1)
	support := f.Support * fs
	c := coeffs{start: make([]int, outSize), w: make([][]float32, outSize)}
	for o := 0; o < outSize; o++ {
		center := (float64(o) + 0.5) * scale
		xmin := int(center - support + 0.5)
		if xmin < 0 {
			xmin = 0
		}
		xmax := int(center + support + 0.5)
		if xmax > inSize {
			xmax = inSize
		}
		ws := make([]float32, xmax-xmin)
		var tot float64
		tmp := make([]float64, xmax-xmin)
		for x := xmin; x < xmax; x++ {
			v := f.Kernel((float64(x) - center + 0.5) / fs)
			tmp[x-xmin] = v
			tot += v
		}
		for i := range tmp {
			if tot != 0 {
				ws[i] = float32(tmp[i] / tot)
			}
		}
		c.start[o], c.w[o] = xmin, ws
	}
	return c
}

// Resize resamples to exactly w x h with the given filter (antialiased).
func Resize(m *RGB, w, h int, f Filter) *RGB {
	if w == m.W && h == m.H {
		return &RGB{W: w, H: h, Pix: append([]uint8(nil), m.Pix...)}
	}
	// horizontal pass
	ch := precompute(m.W, w, f)
	tmp := NewRGB(w, m.H)
	for y := 0; y < m.H; y++ {
		src := m.Pix[y*m.W*3:]
		dst := tmp.Pix[y*w*3:]
		for x := 0; x < w; x++ {
			var r, g, b float32
			s := ch.start[x]
			for i, k := range ch.w[x] {
				p := src[(s+i)*3:]
				r += k * float32(p[0])
				g += k * float32(p[1])
				b += k * float32(p[2])
			}
			dst[x*3], dst[x*3+1], dst[x*3+2] = round8(r), round8(g), round8(b)
		}
	}
	// vertical pass
	cv := precompute(m.H, h, f)
	out := NewRGB(w, h)
	acc := make([]float32, w*3)
	for y := 0; y < h; y++ {
		for i := range acc {
			acc[i] = 0
		}
		s := cv.start[y]
		for i, k := range cv.w[y] {
			row := tmp.Pix[(s+i)*w*3 : (s+i+1)*w*3]
			for j, v := range row {
				acc[j] += k * float32(v)
			}
		}
		dst := out.Pix[y*w*3:]
		for j, v := range acc {
			dst[j] = round8(v)
		}
	}
	return out
}

func round8(v float32) uint8 {
	v += 0.5
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v)
}

// FitWithin returns dims scaled so the long edge is at most maxDim.
func FitWithin(w, h, maxDim int) (int, int) {
	if w <= maxDim && h <= maxDim {
		return w, h
	}
	if w >= h {
		return maxDim, max(1, int(math.Round(float64(h)*float64(maxDim)/float64(w))))
	}
	return max(1, int(math.Round(float64(w)*float64(maxDim)/float64(h)))), maxDim
}

// Thumbnail downsizes so the long edge is maxDim (never upscales).
func Thumbnail(m *RGB, maxDim int) *RGB {
	w, h := FitWithin(m.W, m.H, maxDim)
	if w == m.W && h == m.H {
		return m
	}
	return Resize(m, w, h, Bilinear)
}

package imageio

import (
	"image"
	"image/color"
	"math"
)

// RGB is a compact 8-bit interleaved RGB image.
type RGB struct {
	W, H int
	Pix  []uint8 // len = W*H*3
}

func NewRGB(w, h int) *RGB { return &RGB{W: w, H: h, Pix: make([]uint8, w*h*3)} }

// ToImage wraps the RGB buffer as an image.Image (for encoding).
func (m *RGB) ToImage() *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, m.W, m.H))
	for i, j := 0, 0; i < len(m.Pix); i, j = i+3, j+4 {
		out.Pix[j], out.Pix[j+1], out.Pix[j+2], out.Pix[j+3] = m.Pix[i], m.Pix[i+1], m.Pix[i+2], 255
	}
	return out
}

func clamp8(v int32) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// ToRGB converts any image to RGB. If maxDim > 0 and the image is larger, it is
// downscaled with an area filter while converting (row-streaming, so the full
// RGB copy of a huge image is never materialised).
func ToRGB(img image.Image, maxDim int) *RGB {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	tw, th := w, h
	if maxDim > 0 && (w > maxDim || h > maxDim) {
		if w >= h {
			tw, th = maxDim, int(math.Round(float64(h)*float64(maxDim)/float64(w)))
		} else {
			th, tw = maxDim, int(math.Round(float64(w)*float64(maxDim)/float64(h)))
		}
		if tw < 1 {
			tw = 1
		}
		if th < 1 {
			th = 1
		}
	}
	rowFn := rowConverter(img)
	if tw == w && th == h {
		out := NewRGB(w, h)
		for y := 0; y < h; y++ {
			rowFn(y, out.Pix[y*w*3:(y+1)*w*3])
		}
		return out
	}
	return areaDownscaleRows(rowFn, w, h, tw, th)
}

// rowConverter returns a function writing source row y as RGB triples into dst.
func rowConverter(img image.Image) func(y int, dst []uint8) {
	b := img.Bounds()
	switch m := img.(type) {
	case *image.YCbCr:
		return func(y int, dst []uint8) {
			yy := y + b.Min.Y
			yi := m.YOffset(b.Min.X, yy)
			for x := 0; x < b.Dx(); x++ {
				ci := m.COffset(b.Min.X+x, yy)
				Y := int32(m.Y[yi+x]) * 0x10101
				cb := int32(m.Cb[ci]) - 128
				cr := int32(m.Cr[ci]) - 128
				r := (Y + 91881*cr) >> 16
				g := (Y - 22554*cb - 46802*cr) >> 16
				bb := (Y + 116130*cb) >> 16
				dst[x*3], dst[x*3+1], dst[x*3+2] = clamp8(r), clamp8(g), clamp8(bb)
			}
		}
	case *image.Gray:
		return func(y int, dst []uint8) {
			row := m.Pix[(y)*m.Stride:]
			for x := 0; x < b.Dx(); x++ {
				v := row[x]
				dst[x*3], dst[x*3+1], dst[x*3+2] = v, v, v
			}
		}
	case *image.RGBA:
		return func(y int, dst []uint8) {
			row := m.Pix[y*m.Stride:]
			for x := 0; x < b.Dx(); x++ {
				dst[x*3], dst[x*3+1], dst[x*3+2] = row[x*4], row[x*4+1], row[x*4+2]
			}
		}
	case *image.NRGBA:
		return func(y int, dst []uint8) {
			row := m.Pix[y*m.Stride:]
			for x := 0; x < b.Dx(); x++ {
				a := uint32(row[x*4+3])
				// composite on white so transparent PNGs don't become black
				dst[x*3] = uint8((uint32(row[x*4])*a + 255*(255-a)) / 255)
				dst[x*3+1] = uint8((uint32(row[x*4+1])*a + 255*(255-a)) / 255)
				dst[x*3+2] = uint8((uint32(row[x*4+2])*a + 255*(255-a)) / 255)
			}
		}
	case *image.CMYK:
		return func(y int, dst []uint8) {
			row := m.Pix[y*m.Stride:]
			for x := 0; x < b.Dx(); x++ {
				r, g, bb := color.CMYKToRGB(row[x*4], row[x*4+1], row[x*4+2], row[x*4+3])
				dst[x*3], dst[x*3+1], dst[x*3+2] = r, g, bb
			}
		}
	default:
		return func(y int, dst []uint8) {
			for x := 0; x < b.Dx(); x++ {
				r, g, bb, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				// composite on white
				r += 0xffff - a
				g += 0xffff - a
				bb += 0xffff - a
				dst[x*3], dst[x*3+1], dst[x*3+2] = uint8(r>>8), uint8(g>>8), uint8(bb>>8)
			}
		}
	}
}

// areaDownscaleRows performs an exact box (area-average) downscale, streaming
// source rows so memory stays proportional to the output.
func areaDownscaleRows(rowFn func(int, []uint8), w, h, tw, th int) *RGB {
	out := NewRGB(tw, th)
	// Horizontal weights: for each output x, contributing src columns.
	type span struct {
		x0, x1 int
		w0, w1 float32 // weights of first and last column (partial coverage)
	}
	sx := float64(w) / float64(tw)
	spans := make([]span, tw)
	for i := 0; i < tw; i++ {
		a, bnd := float64(i)*sx, float64(i+1)*sx
		x0, x1 := int(a), int(math.Ceil(bnd))-1
		if x1 >= w {
			x1 = w - 1
		}
		spans[i] = span{x0, x1, float32(float64(x0+1) - a), float32(bnd - float64(x1))}
		if x0 == x1 {
			spans[i].w0 = float32(bnd - a)
		}
	}
	row := make([]uint8, w*3)
	hrow := make([]float32, tw*3) // horizontally reduced current src row
	acc := make([]float32, tw*3)  // vertical accumulator
	sy := float64(h) / float64(th)
	norm := float32(1 / (sx * sy))
	oy := 0
	yEnd := sy // end of current output row in src coords
	for y := 0; y < h && oy < th; y++ {
		rowFn(y, row)
		for i, s := range spans {
			var r, g, b float32
			for x := s.x0; x <= s.x1; x++ {
				wt := float32(1)
				if x == s.x0 {
					wt = s.w0
				} else if x == s.x1 {
					wt = s.w1
				}
				r += wt * float32(row[x*3])
				g += wt * float32(row[x*3+1])
				b += wt * float32(row[x*3+2])
			}
			hrow[i*3], hrow[i*3+1], hrow[i*3+2] = r, g, b
		}
		y0, y1 := float64(y), float64(y+1)
		for y1 > y0 && oy < th {
			cov := math.Min(y1, yEnd) - y0
			c := float32(cov)
			for i := range acc {
				acc[i] += c * hrow[i]
			}
			y0 += cov
			if y0 >= yEnd-1e-9 {
				o := out.Pix[oy*tw*3 : (oy+1)*tw*3]
				for i := range acc {
					v := acc[i]*norm + 0.5
					if v > 255 {
						v = 255
					}
					o[i] = uint8(v)
					acc[i] = 0
				}
				oy++
				yEnd = float64(oy+1) * sy
			}
		}
	}
	return out
}

// Orient applies an EXIF orientation (1..8) to the image.
func Orient(m *RGB, o int) *RGB {
	if o <= 1 || o > 8 {
		return m
	}
	w, h := m.W, m.H
	var out *RGB
	if o >= 5 {
		out = NewRGB(h, w)
	} else {
		out = NewRGB(w, h)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			si := (y*w + x) * 3
			di := (ny*out.W + nx) * 3
			out.Pix[di], out.Pix[di+1], out.Pix[di+2] = m.Pix[si], m.Pix[si+1], m.Pix[si+2]
		}
	}
	return out
}

// Crop returns a copy of the given rectangle (clamped to the image).
func (m *RGB) Crop(x0, y0, x1, y1 int) *RGB {
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > m.W {
		x1 = m.W
	}
	if y1 > m.H {
		y1 = m.H
	}
	if x1 <= x0 || y1 <= y0 {
		return NewRGB(1, 1)
	}
	out := NewRGB(x1-x0, y1-y0)
	for y := y0; y < y1; y++ {
		copy(out.Pix[(y-y0)*out.W*3:(y-y0+1)*out.W*3], m.Pix[(y*m.W+x0)*3:(y*m.W+x1)*3])
	}
	return out
}

// Gray returns Rec.601 luma as float32 in [0,255].
func (m *RGB) Gray() []float32 {
	g := make([]float32, m.W*m.H)
	for i := range g {
		p := m.Pix[i*3:]
		g[i] = 0.299*float32(p[0]) + 0.587*float32(p[1]) + 0.114*float32(p[2])
	}
	return g
}

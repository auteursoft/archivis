package imageio

import "image"

// Blockiness measures JPEG block artefacts: the mean absolute luminance step
// across 8-pixel block boundaries divided by the mean step between other
// neighbouring pixels. Uncompressed or high-quality images read ~1.0;
// heavily compressed ones read well above (1.3+). It must be measured at the
// native resolution, where the 8x8 coding grid is intact.
func Blockiness(y []uint8, w, h, stride int) float64 {
	if w < 32 || h < 32 {
		return 1
	}
	var bnd, in float64
	var nb, ni int
	for r := 0; r < h; r += 2 { // every other row is plenty
		row := y[r*stride : r*stride+w]
		for x := 1; x < w; x++ {
			d := float64(row[x]) - float64(row[x-1])
			if d < 0 {
				d = -d
			}
			if x%8 == 0 {
				bnd += d
				nb++
			} else {
				in += d
				ni++
			}
		}
	}
	for r := 1; r < h; r++ {
		if r%8 != 0 && r%8 != 4 { // boundary rows and a mid-block control
			continue
		}
		a, b := y[(r-1)*stride:], y[r*stride:]
		for x := 0; x < w; x += 2 {
			d := float64(b[x]) - float64(a[x])
			if d < 0 {
				d = -d
			}
			if r%8 == 0 {
				bnd += d
				nb++
			} else {
				in += d
				ni++
			}
		}
	}
	if nb == 0 || ni == 0 || in == 0 {
		return 1
	}
	return (bnd / float64(nb)) / (in / float64(ni))
}

// blockinessOf computes Blockiness for decoded JPEG images.
func blockinessOf(img image.Image) float64 {
	switch m := img.(type) {
	case *image.YCbCr:
		b := m.Bounds()
		return Blockiness(m.Y[m.YOffset(b.Min.X, b.Min.Y):], b.Dx(), b.Dy(), m.YStride)
	case *image.Gray:
		b := m.Bounds()
		return Blockiness(m.Pix, b.Dx(), b.Dy(), m.Stride)
	}
	return 1
}

// BlockinessRGB computes Blockiness on an RGB image whose 8x8 grid starts at
// the origin (e.g. a JPEG decoded without resampling).
func BlockinessRGB(m *RGB) float64 {
	y := make([]uint8, m.W*m.H)
	for i := range y {
		p := m.Pix[i*3:]
		y[i] = uint8((299*int(p[0]) + 587*int(p[1]) + 114*int(p[2])) / 1000)
	}
	return Blockiness(y, m.W, m.H, m.W)
}

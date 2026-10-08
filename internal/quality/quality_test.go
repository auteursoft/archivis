package quality

import (
	"math"
	"math/rand"
	"testing"

	"github.com/auteursoft/archivis/internal/imageio"
)

func load(t *testing.T) *imageio.RGB {
	d, err := imageio.Load("testdata/t1.jpg", 0)
	if err != nil {
		t.Fatal(err)
	}
	w, h := scaleTo(d.Img.W, d.Img.H, AnalysisSize)
	return imageio.Resize(d.Img, w, h, imageio.Bicubic)
}

// boxBlur approximates a gaussian with repeated box filters.
func blur(m *imageio.RGB, r int) *imageio.RGB {
	out := &imageio.RGB{W: m.W, H: m.H, Pix: append([]uint8(nil), m.Pix...)}
	tmp := make([]uint8, len(out.Pix))
	for pass := 0; pass < 3; pass++ {
		for y := 0; y < m.H; y++ {
			for x := 0; x < m.W; x++ {
				for c := 0; c < 3; c++ {
					s, n := 0, 0
					for k := -r; k <= r; k++ {
						xx := x + k
						if xx >= 0 && xx < m.W {
							s += int(out.Pix[(y*m.W+xx)*3+c])
							n++
						}
					}
					tmp[(y*m.W+x)*3+c] = uint8(s / n)
				}
			}
		}
		for y := 0; y < m.H; y++ {
			for x := 0; x < m.W; x++ {
				for c := 0; c < 3; c++ {
					s, n := 0, 0
					for k := -r; k <= r; k++ {
						yy := y + k
						if yy >= 0 && yy < m.H {
							s += int(tmp[(yy*m.W+x)*3+c])
							n++
						}
					}
					out.Pix[(y*m.W+x)*3+c] = uint8(s / n)
				}
			}
		}
	}
	return out
}

func mapPix(m *imageio.RGB, f func(c int, v float64) float64) *imageio.RGB {
	out := imageio.NewRGB(m.W, m.H)
	for i, v := range m.Pix {
		out.Pix[i] = uint8(math.Max(0, math.Min(255, math.Round(f(i%3, float64(v))))))
	}
	return out
}

func TestFocusOrdering(t *testing.T) {
	img := load(t)
	base := Analyze(img)
	b1 := Analyze(blur(img, 1))
	b3 := Analyze(blur(img, 3))
	t.Logf("sharpness base=%.1f/%.1f (score %.2f) blur1=%.1f (%.2f) blur3=%.1f (%.2f)",
		base.Sharpness, base.SharpnessGlobal, base.FocusScore, b1.Sharpness, b1.FocusScore, b3.Sharpness, b3.FocusScore)
	if !(base.Sharpness > b1.Sharpness && b1.Sharpness > b3.Sharpness) {
		t.Fatal("sharpness not monotonic in blur")
	}
	if b3.FocusScore > 0.2 {
		t.Errorf("heavily blurred image focus score %.2f", b3.FocusScore)
	}
}

func TestNoiseDoesNotFakeSharpness(t *testing.T) {
	img := blur(load(t), 3)
	rng := rand.New(rand.NewSource(1))
	noisy := mapPix(img, func(_ int, v float64) float64 { return v + rng.NormFloat64()*6 })
	a, b := Analyze(img), Analyze(noisy)
	t.Logf("noise clean=%.2f noisy=%.2f; sharpness clean=%.1f noisy=%.1f", a.Noise, b.Noise, a.Sharpness, b.Sharpness)
	if b.Noise < 3 || b.Noise > 9 {
		t.Errorf("noise estimate %.2f for sigma 6 (after luma averaging ~4-6)", b.Noise)
	}
	if b.FocusScore > a.FocusScore+0.15 {
		t.Errorf("noise inflated focus score %.2f -> %.2f", a.FocusScore, b.FocusScore)
	}
	if b.NoiseScore >= a.NoiseScore {
		t.Error("noise score should drop")
	}
}

func TestExposureAndCast(t *testing.T) {
	img := load(t)
	base := Analyze(img)
	over := Analyze(mapPix(img, func(_ int, v float64) float64 { return v*1.8 + 40 }))
	under := Analyze(mapPix(img, func(_ int, v float64) float64 { return v * 0.25 }))
	warm := Analyze(mapPix(img, func(c int, v float64) float64 { return v * []float64{1.15, 1.0, 0.75}[c] }))
	mono := Analyze(mapPix(img, func(c int, v float64) float64 { return v }))
	_ = mono
	t.Logf("exposure base=%.2f over=%.2f (hi %.3f) under=%.2f (bright %.2f)", base.ExposureScore, over.ExposureScore, over.Highlights, under.ExposureScore, under.Brightness)
	t.Logf("cast base=%.1f (%s) warm=%.1f (%s) a=%.1f b=%.1f", base.CastStrength, CastLabel(base.CastA, base.CastB, base.CastStrength), warm.CastStrength, CastLabel(warm.CastA, warm.CastB, warm.CastStrength), warm.CastA, warm.CastB)
	if over.ExposureScore >= base.ExposureScore || under.ExposureScore >= base.ExposureScore {
		t.Error("exposure score should drop for over/under exposure")
	}
	if over.ExposureScore > 0.4 || under.ExposureScore > 0.5 {
		t.Errorf("bad exposures score too high: over %.2f under %.2f", over.ExposureScore, under.ExposureScore)
	}
	if CastLabel(warm.CastA, warm.CastB, warm.CastStrength) != "warm/yellow" || warm.ColorScore >= base.ColorScore {
		t.Error("warm cast not detected")
	}
	g := imageio.NewRGB(img.W, img.H)
	for i := 0; i < len(g.Pix); i += 3 {
		v := uint8((int(img.Pix[i]) + int(img.Pix[i+1]) + int(img.Pix[i+2])) / 3)
		g.Pix[i], g.Pix[i+1], g.Pix[i+2] = v, v, v
	}
	gm := Analyze(g)
	if !gm.Monochrome || base.Monochrome {
		t.Errorf("monochrome detection: gray=%v color=%v", gm.Monochrome, base.Monochrome)
	}
	t.Logf("colorfulness base=%.1f gray=%.1f; rating base=%.0f blurred=%.0f", base.Colorfulness, gm.Colorfulness, Rate(img), Rate(blur(img, 3)))
	if Rate(blur(img, 3)) >= Rate(img) {
		t.Error("blur should lower the rating")
	}
}

func BenchmarkAnalyze(b *testing.B) {
	d, _ := imageio.Load("testdata/t1.jpg", 0)
	for i := 0; i < b.N; i++ {
		Analyze(d.Img)
	}
}

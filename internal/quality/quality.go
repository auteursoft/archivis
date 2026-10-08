// Package quality computes objective, technical photo-quality measurements:
// focus, exposure, white balance / colour cast, colourfulness, contrast and
// noise. All measurements are made on an "analysis image" resampled to a fixed
// long edge so that numbers are comparable across cameras and resolutions.
package quality

import (
	_ "embed"
	"encoding/json"
	"math"
	"sort"

	"github.com/auteursoft/archivis/internal/imageio"
)

// AnalysisSize is the long edge (px) larger images are resampled to before
// measuring. Sharpness numbers are only comparable at a fixed scale.
const AnalysisSize = 1600

// Metrics are raw measurements plus derived 0..1 scores.
type Metrics struct {
	// Focus
	Sharpness       float64 `json:"sharpness"`        // Laplacian variance of the sharpest regions (noise-corrected)
	FocusRatio      float64 `json:"focus_ratio"`      // contrast-normalised sharpness of the sharpest regions
	SharpnessGlobal float64 `json:"sharpness_global"` // Laplacian variance of the whole frame (noise-corrected)
	// Exposure
	Brightness   float64 `json:"brightness"`    // mean luma 0..1
	Highlights   float64 `json:"highlights"`    // fraction of clipped highlight pixels
	Shadows      float64 `json:"shadows"`       // fraction of crushed shadow pixels
	DynamicRange float64 `json:"dynamic_range"` // (p99.5 - p0.5) of luma, 0..1
	Contrast     float64 `json:"contrast"`      // RMS contrast of luma, 0..~0.5
	// Colour
	CastA        float64 `json:"cast_a"`        // illuminant colour as Lab a* (+magenta / -green)
	CastB        float64 `json:"cast_b"`        // illuminant colour as Lab b* (+yellow/warm / -blue/cool)
	CastStrength float64 `json:"cast_strength"` // grey-edge illuminant deviation from neutral, degrees
	// labCast is the neutral-pixel cast magnitude (Lab units). The rating
	// model takes it as its "lab_cast" feature; the reported cast is the
	// grey-edge CastStrength above.
	labCast      float64
	Colorfulness float64 `json:"colorfulness"` // Hasler & Süsstrunk
	Saturation   float64 `json:"saturation"`   // mean HSV saturation 0..1
	Monochrome   bool    `json:"monochrome"`
	// Noise
	Noise float64 `json:"noise"` // estimated Gaussian noise sigma (8-bit levels)

	// Derived 0..1 scores (1 = technically excellent)
	FocusScore    float64 `json:"focus_score"`
	ExposureScore float64 `json:"exposure_score"`
	ColorScore    float64 `json:"color_score"`
	NoiseScore    float64 `json:"noise_score"`
}

// Analyze measures an image. The input may be any size; it is resampled to
// AnalysisSize on the long edge first.
func Analyze(src *imageio.RGB) Metrics {
	img := src
	// Downscale large images to the analysis size. Smaller images are measured
	// at native size: upscaling would smear noise into fake "detail".
	if max(img.W, img.H) > AnalysisSize {
		w, h := scaleTo(img.W, img.H, AnalysisSize)
		img = imageio.Resize(src, w, h, imageio.Bilinear)
	}
	var m Metrics
	gray := img.Gray()
	m.Noise = noiseSigma(gray, img.W, img.H)
	m.SharpnessGlobal, m.Sharpness = sharpness(gray, img.W, img.H, m.Noise)
	m.FocusRatio = focusRatio(gray, img.W, img.H, m.Noise)
	exposure(gray, &m)
	color(img, &m)
	scores(&m)
	return m
}

func scaleTo(w, h, long int) (int, int) {
	if w >= h {
		return long, max(1, int(math.Round(float64(h)*float64(long)/float64(w))))
	}
	return max(1, int(math.Round(float64(w)*float64(long)/float64(h)))), long
}

// laplacian computes the 4-neighbour Laplacian response (interior only).
func laplacian(g []float32, w, h int) []float32 {
	out := make([]float32, w*h)
	for y := 1; y < h-1; y++ {
		r := y * w
		for x := 1; x < w-1; x++ {
			i := r + x
			out[i] = g[i-1] + g[i+1] + g[i-w] + g[i+w] - 4*g[i]
		}
	}
	return out
}

// sharpness returns the whole-frame Laplacian variance and the mean over the
// sharpest 10% of tiles (the in-focus subject, robust to shallow depth of
// field). Both are corrected for the contribution of sensor noise: the
// Laplacian of white noise with sigma s has variance 20*s^2.
func sharpness(g []float32, w, h int, noise float64) (global, peak float64) {
	lap := laplacian(g, w, h)
	noiseVar := 20 * noise * noise
	var s, s2 float64
	n := 0
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			v := float64(lap[y*w+x])
			s += v
			s2 += v * v
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	mean := s / float64(n)
	global = math.Max(0, s2/float64(n)-mean*mean-noiseVar)

	// tiles of ~1/16 of the long edge
	ts := max(16, max(w, h)/16)
	var tiles []float64
	for ty := 1; ty+ts <= h-1; ty += ts {
		for tx := 1; tx+ts <= w-1; tx += ts {
			var ts1, ts2 float64
			for y := ty; y < ty+ts; y++ {
				row := lap[y*w+tx : y*w+tx+ts]
				for _, v := range row {
					fv := float64(v)
					ts1 += fv
					ts2 += fv * fv
				}
			}
			cnt := float64(ts * ts)
			mu := ts1 / cnt
			tiles = append(tiles, math.Max(0, ts2/cnt-mu*mu-noiseVar))
		}
	}
	if len(tiles) == 0 {
		return global, global
	}
	sort.Float64s(tiles)
	k := max(1, len(tiles)/10)
	var top float64
	for _, v := range tiles[len(tiles)-k:] {
		top += v
	}
	return global, top / float64(k)
}

// focusRatio is a contrast-normalised focus measure: per tile, the
// Laplacian energy divided by the tile's intensity variance (both with the
// noise contribution removed). Lowering contrast (haze, underexposure)
// scales both equally and cancels out, whereas blur removes high
// frequencies and lowers the ratio. Reported for the sharpest 10% of
// textured tiles, like Sharpness.
func focusRatio(g []float32, w, h int, noise float64) float64 {
	// A light binomial pre-filter suppresses pixel noise (variance x0.14)
	// while keeping the edges that distinguish sharp from blurred; the
	// remaining noise contribution is subtracted analytically.
	f := binomial3(g, w, h)
	lap := laplacian(f, w, h)
	nvF := binomialNoiseGain * noise * noise
	nvL := lapBinomialNoiseGain * noise * noise
	ts := max(16, max(w, h)/16)
	var vals []float64
	for ty := 2; ty+ts <= h-2; ty += ts {
		for tx := 2; tx+ts <= w-2; tx += ts {
			var l1, l2, g1, g2 float64
			for y := ty; y < ty+ts; y++ {
				for x := tx; x < tx+ts; x++ {
					i := y*w + x
					l := float64(lap[i])
					v := float64(f[i])
					l1 += l
					l2 += l * l
					g1 += v
					g2 += v * v
				}
			}
			n := float64(ts * ts)
			lv := l2/n - (l1/n)*(l1/n) - nvL
			gv := g2/n - (g1/n)*(g1/n) - nvF
			if gv < 9 { // featureless tile (sky, wall): no focus information
				continue
			}
			vals = append(vals, math.Max(0, lv)/(gv+25))
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	k := max(1, len(vals)/10)
	var t float64
	for _, v := range vals[len(vals)-k:] {
		t += v
	}
	return t / float64(k)
}

// Noise variance gains of the binomial filter, and of the Laplacian applied
// after it (sum of squared kernel weights), for white noise.
var binomialNoiseGain, lapBinomialNoiseGain = func() (float64, float64) {
	b := [3]float64{1, 2, 1}
	var k [3][3]float64
	var gb float64
	for y := 0; y < 3; y++ {
		for x := 0; x < 3; x++ {
			k[y][x] = b[y] * b[x] / 16
			gb += k[y][x] * k[y][x]
		}
	}
	// 5x5 combined kernel: Laplacian (4-neighbour) convolved with binomial
	var c [5][5]float64
	lapK := [3][3]float64{{0, 1, 0}, {1, -4, 1}, {0, 1, 0}}
	for y := 0; y < 3; y++ {
		for x := 0; x < 3; x++ {
			for v := 0; v < 3; v++ {
				for u := 0; u < 3; u++ {
					c[y+v][x+u] += lapK[y][x] * k[v][u]
				}
			}
		}
	}
	var gl float64
	for y := range c {
		for x := range c[y] {
			gl += c[y][x] * c[y][x]
		}
	}
	return gb, gl
}()

func binomial3(g []float32, w, h int) []float32 {
	tmp := make([]float32, len(g))
	out := make([]float32, len(g))
	for y := 0; y < h; y++ {
		r := y * w
		for x := 1; x < w-1; x++ {
			tmp[r+x] = (g[r+x-1] + 2*g[r+x] + g[r+x+1]) / 4
		}
		tmp[r], tmp[r+w-1] = g[r], g[r+w-1]
	}
	copy(out, tmp)
	for y := 1; y < h-1; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			out[i] = (tmp[i-w] + 2*tmp[i] + tmp[i+w]) / 4
		}
	}
	return out
}

// noiseSigma estimates sensor noise (Gaussian sigma, 8-bit levels).
//
// Immerkær's operator (a Laplacian difference that cancels smooth image
// structure) is evaluated per pixel, averaged over 16x16 blocks, and the
// noise level is taken from the flattest blocks (10th percentile). Using
// only flat regions matters: fine texture (foliage, fabric, hair) also
// passes the operator, and averaging over the whole frame made detailed,
// sharp photos read as "noisy". Clipped blocks are skipped because
// saturation hides noise.
func noiseSigma(g []float32, w, h int) float64 {
	const bs = 16
	if w < 2*bs || h < 2*bs {
		return 0
	}
	var sigmas []float64
	for by := 1; by+bs < h-1; by += bs {
		for bx := 1; bx+bs < w-1; bx += bs {
			var sum, lum float64
			for y := by; y < by+bs; y++ {
				for x := bx; x < bx+bs; x++ {
					i := y*w + x
					r := g[i-w-1] - 2*g[i-w] + g[i-w+1] - 2*g[i-1] + 4*g[i] - 2*g[i+1] + g[i+w-1] - 2*g[i+w] + g[i+w+1]
					if r < 0 {
						r = -r
					}
					sum += float64(r)
					lum += float64(g[i])
				}
			}
			lum /= bs * bs
			if lum < 8 || lum > 247 {
				continue
			}
			sigmas = append(sigmas, math.Sqrt(math.Pi/2)*sum/(6*bs*bs))
		}
	}
	if len(sigmas) == 0 {
		return 0
	}
	sort.Float64s(sigmas)
	// The lowest decile of noisy per-block estimates is biased low; this
	// factor was calibrated on photos with known added noise (see
	// tools/validate, "ladder").
	return noiseBias * sigmas[len(sigmas)/10]
}

var noiseBias = 1.1

func exposure(g []float32, m *Metrics) {
	var hist [256]int
	var s, s2 float64
	for _, v := range g {
		iv := int(v + 0.5)
		if iv > 255 {
			iv = 255
		}
		hist[iv]++
		s += float64(v)
		s2 += float64(v) * float64(v)
	}
	n := float64(len(g))
	mean := s / n
	m.Brightness = mean / 255
	m.Contrast = math.Sqrt(math.Max(0, s2/n-mean*mean)) / 255
	clipHi, clipLo := 0, 0
	for i := 250; i < 256; i++ {
		clipHi += hist[i]
	}
	for i := 0; i <= 5; i++ {
		clipLo += hist[i]
	}
	m.Highlights = float64(clipHi) / n
	m.Shadows = float64(clipLo) / n
	lo, hi := histPercentile(&hist, len(g), 0.005), histPercentile(&hist, len(g), 0.995)
	m.DynamicRange = float64(hi-lo) / 255
}

func histPercentile(h *[256]int, n int, p float64) int {
	target := int(p * float64(n))
	c := 0
	for i, v := range h {
		c += v
		if c > target {
			return i
		}
	}
	return 255
}

var srgbToLinear = func() [256]float64 {
	var t [256]float64
	for i := range t {
		c := float64(i) / 255
		if c <= 0.04045 {
			t[i] = c / 12.92
		} else {
			t[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return t
}()

func labF(t float64) float64 {
	if t > 216.0/24389 {
		return math.Cbrt(t)
	}
	return (24389.0/27*t + 16) / 116
}

// Lab converts an sRGB pixel to CIELAB (D65).
func Lab(r, g, b uint8) (L, A, B float64) {
	rl, gl, bl := srgbToLinear[r], srgbToLinear[g], srgbToLinear[b]
	x := (0.4124564*rl + 0.3575761*gl + 0.1804375*bl) / 0.95047
	y := 0.2126729*rl + 0.7151522*gl + 0.0721750*bl
	z := (0.0193339*rl + 0.1191920*gl + 0.9503041*bl) / 1.08883
	fx, fy, fz := labF(x), labF(y), labF(z)
	return 116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)
}

func color(img *imageio.RGB, m *Metrics) {
	var sumRG, sumYB, sqRG, sqYB, sat float64
	var na, nb, nn, aa, ab float64
	var chromatic, total int
	for y := 0; y < img.H; y += 2 {
		for x := 0; x < img.W; x += 2 {
			p := img.Pix[(y*img.W+x)*3:]
			r, g, b := float64(p[0]), float64(p[1]), float64(p[2])
			rg := r - g
			yb := 0.5*(r+g) - b
			sumRG += rg
			sumYB += yb
			sqRG += rg * rg
			sqYB += yb * yb
			mx := math.Max(r, math.Max(g, b))
			mn := math.Min(r, math.Min(g, b))
			if mx > 0 {
				sat += (mx - mn) / mx
			}
			L, A, B := Lab(p[0], p[1], p[2])
			c := math.Hypot(A, B)
			total++
			aa += A
			ab += B
			if c > 8 && L > 5 {
				chromatic++
			}
			// candidate neutrals: mid-tones with low chroma
			if L > 20 && L < 95 && c < 18 {
				na += A
				nb += B
				nn++
			}
		}
	}
	t := float64(total)
	muRG, muYB := sumRG/t, sumYB/t
	sdRG := math.Sqrt(math.Max(0, sqRG/t-muRG*muRG))
	sdYB := math.Sqrt(math.Max(0, sqYB/t-muYB*muYB))
	m.Colorfulness = math.Hypot(sdRG, sdYB) + 0.3*math.Hypot(muRG, muYB)
	m.Saturation = sat / t
	m.Monochrome = float64(chromatic)/t < 0.005
	if nn/t > 0.02 {
		m.CastA, m.CastB = na/nn, nb/nn
	} else {
		// too few neutral pixels: fall back to a damped grey-world estimate
		m.CastA, m.CastB = 0.5*aa/t, 0.5*ab/t
	}
	m.labCast = math.Hypot(m.CastA, m.CastB)
	// Reported cast: grey-edge illuminant estimate, which on photos with a
	// known applied cast detects it far more reliably (tools/validate).
	m.CastStrength, m.CastA, m.CastB = grayEdge(img)
}

func ramp(v, lo, hi float64) float64 {
	if hi == lo {
		return 0
	}
	t := (v - lo) / (hi - lo)
	return math.Max(0, math.Min(1, t))
}

func scores(m *Metrics) {
	// Focus: log scale between "clearly soft" and "crisp" at AnalysisSize.
	// Contrast-normalised focus on a log scale. Calibrated on real photos
	// with known applied blur (tools/validate): Gaussian blur sigma=2 px
	// reads ~0.012 (score 0), sigma=1 ~0.05 (score ~0.4), and a typical
	// untouched photo ~0.19 (score ~0.9).
	m.FocusScore = ramp(math.Log(m.FocusRatio+1e-6), math.Log(0.015), math.Log(0.25))

	// Exposure: penalise clipping and extreme mean luminance, reward use of range.
	e := 1.0
	e -= 4 * math.Max(0, m.Highlights-0.01) // >1% blown highlights hurts quickly
	e -= 2 * math.Max(0, m.Shadows-0.04)
	if m.Brightness < 0.25 {
		e -= (0.25 - m.Brightness) * 4
	}
	if m.Brightness > 0.75 {
		e -= (m.Brightness - 0.75) * 4
	}
	if m.DynamicRange < 0.5 {
		e -= (0.5 - m.DynamicRange) * 0.8 // flat / hazy / underexposed
	}
	m.ExposureScore = math.Max(0, math.Min(1, e))

	// Colour: neutral greys should be neutral. Cast below ~3 is invisible.
	if m.Monochrome {
		m.ColorScore = 1
	} else {
		// calibrated on photos with known applied casts: <3° neutral
		// (half of untouched photos), a 10% channel shift reads ~4-5°,
		// 35% reads 11-14°
		m.ColorScore = 1 - ramp(m.CastStrength, 3, 12)
	}

	// Noise sigma at analysis scale: ~1 clean, 6+ very noisy.
	m.NoiseScore = 1 - ramp(m.Noise, 1.5, 7)
}

// Technical is the overall technical score (0..100): the human-calibrated
// rating (see Rate) with mild penalties for exposure errors, colour casts
// and heavy JPEG compression, which the rating's training data rarely
// contained. Penalty strengths were chosen on tuning data as the strongest
// that keep agreement with human ratings within 0.01 Spearman; stronger
// penalties make the score disagree with people (who often do not consider
// a warm or bright photo flawed), so those properties are reported as
// separate scores instead (tools/validate/tune_penalties.py).
func Technical(rating float64, m Metrics, blockiness float64) float64 {
	const exposurePenalty, colorPenalty, compressionPenalty = 0.1, 0.1, 0.2
	jp := math.Max(0, math.Min(1, (blockiness-1.15)/0.85))
	return rating * (1 - exposurePenalty*(1-m.ExposureScore)) * (1 - colorPenalty*(1-m.ColorScore)) * (1 - compressionPenalty*jp)
}

// RatingSize is the long edge at which the human-calibrated rating model
// measures images. The model was fit on 512 px images (BIQ2021), so photos
// are measured at the same scale for its coefficients to apply.
const RatingSize = 512

// The rating model predicts mean human opinion score (0..1) from
// measurements taken at RatingSize: a ridge regression on standardised
// features and their pairwise products (quadratic), which captures effects a
// linear mix cannot, such as exposure being bad at both extremes. It was fit
// on the 10,000-image BIQ2021 training split (30 raters per image); on the
// held-out 2,000-image test split it reaches Spearman 0.63 with human
// ratings, versus 0.43 for the hand-weighted formula it replaced.
// Regenerate with tools/validate/fit_quality.py --export.
//
//go:embed rating_model.json
var ratingModelJSON []byte

type ratingParams struct {
	Features  []string    `json:"features"`
	LogFeat   []string    `json:"log_features"`
	Mean1     []float64   `json:"mean1"`
	Scale1    []float64   `json:"scale1"`
	Powers    [][]float64 `json:"powers"`
	Mean2     []float64   `json:"mean2"`
	Scale2    []float64   `json:"scale2"`
	Coef      []float64   `json:"coef"`
	Intercept float64     `json:"intercept"`
}

var rating = func() ratingParams {
	var r ratingParams
	if err := json.Unmarshal(ratingModelJSON, &r); err != nil {
		panic("quality: bad rating_model.json: " + err.Error())
	}
	return r
}()

func ratingFeature(m Metrics, name string) float64 {
	switch name {
	case "sharpness":
		return m.Sharpness
	case "sharpness_global":
		return m.SharpnessGlobal
	case "brightness":
		return m.Brightness
	case "highlights":
		return m.Highlights
	case "shadows":
		return m.Shadows
	case "dynamic_range":
		return m.DynamicRange
	case "contrast":
		return m.Contrast
	case "lab_cast":
		return m.labCast
	case "colorfulness":
		return m.Colorfulness
	case "noise":
		return m.Noise
	}
	panic("quality: unknown rating feature " + name)
}

// LabCast is the rating model's colour-cast input (see labCast), exposed so
// tools/validate can export exactly the features the model sees.
func (m Metrics) LabCast() float64 { return m.labCast }

// Rate predicts human-perceived technical quality on a 0..100 scale. src
// may be any size; it is measured at RatingSize.
func Rate(src *imageio.RGB) float64 {
	img := src
	if max(img.W, img.H) > RatingSize {
		w, h := scaleTo(img.W, img.H, RatingSize)
		img = imageio.Resize(src, w, h, imageio.Bilinear)
	}
	return RateMetrics(Analyze(img))
}

// RateMetrics applies the rating model to metrics measured at RatingSize.
func RateMetrics(m Metrics) float64 {
	r := &rating
	x := make([]float64, len(r.Features))
	for i, f := range r.Features {
		v := ratingFeature(m, f)
		for _, l := range r.LogFeat {
			if l == f {
				v = math.Log1p(v)
			}
		}
		x[i] = (v - r.Mean1[i]) / r.Scale1[i]
	}
	v := r.Intercept
	for k, pw := range r.Powers {
		t := 1.0
		for i, p := range pw {
			for ; p > 0; p-- {
				t *= x[i]
			}
		}
		v += r.Coef[k] * (t - r.Mean2[k]) / r.Scale2[k]
	}
	return 100 * math.Max(0, math.Min(1, v))
}

// CastLabel describes the dominant colour cast in words.
func CastLabel(a, b, strength float64) string {
	if strength < 3 {
		return "neutral"
	}
	ang := math.Atan2(b, a) * 180 / math.Pi // a=+x (magenta), b=+y (yellow)
	switch {
	case ang >= 45 && ang < 135:
		return "warm/yellow"
	case ang >= -45 && ang < 45:
		return "magenta"
	case ang >= -135 && ang < -45:
		return "cool/blue"
	default:
		return "green"
	}
}

// RegionSharpness measures Laplacian variance of an image region that has
// already been normalised in size (e.g. an aligned 112x112 face crop). The
// outer border is ignored to avoid alignment padding.
func RegionSharpness(img *imageio.RGB) float64 {
	g := img.Gray()
	w, h := img.W, img.H
	lap := laplacian(g, w, h)
	bx, by := w/8, h/8
	var s, s2 float64
	n := 0
	for y := by; y < h-by; y++ {
		for x := bx; x < w-bx; x++ {
			v := float64(lap[y*w+x])
			s += v
			s2 += v * v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	mu := s / float64(n)
	return s2/float64(n) - mu*mu
}

// GrayEdgeCast returns the grey-edge cast angle in degrees (see grayEdge).
func GrayEdgeCast(src *imageio.RGB) float64 {
	a, _, _ := grayEdge(src)
	return a
}

// grayEdge estimates the colour of the illuminant from image edges
// (van de Weijer, Gevers & Gijsenij 2007, "grey-edge", Minkowski p=6) in
// linear RGB. It returns the angular deviation from neutral in degrees and
// the illuminant's colour direction as Lab a*, b* (for labelling the cast).
// Edges are less dominated by large coloured surfaces than raw pixels.
func grayEdge(src *imageio.RGB) (angle, a, b float64) {
	img := src
	if max(img.W, img.H) > 800 {
		w, h := scaleTo(img.W, img.H, 800)
		img = imageio.Resize(src, w, h, imageio.Bilinear)
	}
	var e [3]float64
	for y := 1; y < img.H-1; y++ {
		for x := 1; x < img.W-1; x++ {
			i := (y*img.W + x) * 3
			for c := 0; c < 3; c++ {
				p := img.Pix[i+c]
				if p >= 250 || p <= 3 {
					continue // clipped values carry no colour information
				}
				dx := srgbToLinear[img.Pix[i+3+c]] - srgbToLinear[img.Pix[i-3+c]]
				dy := srgbToLinear[img.Pix[i+img.W*3+c]] - srgbToLinear[img.Pix[i-img.W*3+c]]
				g := dx*dx + dy*dy
				e[c] += g * g * g // (|grad|^2)^3 = |grad|^6
			}
		}
	}
	for c := range e {
		e[c] = math.Pow(e[c], 1.0/6)
	}
	n := math.Sqrt(e[0]*e[0] + e[1]*e[1] + e[2]*e[2])
	if n == 0 {
		return 0, 0, 0
	}
	cos := (e[0] + e[1] + e[2]) / (n * math.Sqrt(3))
	angle = math.Acos(math.Min(1, cos)) * 180 / math.Pi
	// colour direction: the illuminant as a mid-grey surface would show it
	mx := math.Max(e[0], math.Max(e[1], e[2]))
	toByte := func(v float64) uint8 {
		lin := 0.2 * v / mx
		var c float64
		if lin <= 0.0031308 {
			c = 12.92 * lin
		} else {
			c = 1.055*math.Pow(lin, 1/2.4) - 0.055
		}
		return uint8(math.Round(math.Max(0, math.Min(1, c)) * 255))
	}
	_, a, b = Lab(toByte(e[0]), toByte(e[1]), toByte(e[2]))
	return angle, a, b
}

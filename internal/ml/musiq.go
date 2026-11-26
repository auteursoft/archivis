package ml

import (
	"fmt"
	"math"
	"path/filepath"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/auteursoft/archivis/internal/imageio"
)

// MUSIQ predicts how good people find a photo's technical quality, with
// Google Research's MUSIQ (Ke et al., ICCV 2021; github.com/google-research/
// google-research/tree/master/musiq, Apache-2.0), its checkpoint trained on
// KonIQ-10k, exported to ONNX by tools/musiq/export_onnx.py. It reads the
// image at three scales (224 px, 384 px and as given) as one sequence of
// 32-px patches. On BIQ2021 test photos it agrees with people at Spearman
// 0.755 on its own, against 0.632 for archivis's measurements (see
// VALIDATION.md); quality.RateWith blends the two.
type MUSIQ struct {
	s *session
}

// MUSIQSize is the longest side, in pixels, MUSIQ is given: the scale of the
// photos the blend in quality.RateWith was fit on, and of the exported
// model's fixed sequence.
const MUSIQSize = 512

const (
	musiqPatch  = 32
	musiqGrid   = 10
	musiqTokLen = musiqPatch*musiqPatch*3 + 3 // patch pixels, then position, scale, mask
	// musiqTokens is the exported model's sequence: 7x7 tokens at 224 px,
	// 12x12 at 384 px and up to 16x16 at MUSIQSize.
	musiqTokens = 49 + 144 + (MUSIQSize/musiqPatch)*(MUSIQSize/musiqPatch)
)

var musiqScales = [2]int{224, 384}

// NewMUSIQ loads musiq/koniq.onnx from the models directory.
func NewMUSIQ(modelsDir string, o Options) (*MUSIQ, error) {
	p := filepath.Join(modelsDir, "musiq", "koniq.onnx")
	s, err := openSession(p, o)
	if err != nil {
		return nil, err
	}
	if len(s.inputs) != 1 || len(s.inputs[0].Dimensions) != 3 || s.inputs[0].Dimensions[1] != musiqTokens || s.inputs[0].Dimensions[2] != musiqTokLen {
		s.close()
		return nil, fmt.Errorf("MUSIQ model %s: want one (1, %d, %d) input; run archivis setup again", p, musiqTokens, musiqTokLen)
	}
	return &MUSIQ{s: s}, nil
}

func (m *MUSIQ) Close() {
	if m != nil {
		m.s.close()
	}
}

// Score predicts the mean opinion score (0..100, KonIQ's scale) of an image
// whose longer side is at most MUSIQSize.
func (m *MUSIQ) Score(img *imageio.RGB) (float64, error) {
	seq, err := musiqSequence(img)
	if err != nil {
		return 0, err
	}
	t, err := ort.NewTensor(ort.NewShape(1, musiqTokens, musiqTokLen), seq)
	if err != nil {
		return 0, err
	}
	defer t.Destroy()
	outs, _, err := m.s.run(t)
	if err != nil {
		return 0, fmt.Errorf("musiq: %w", err)
	}
	if len(outs) == 0 || len(outs[0]) != 1 {
		return 0, fmt.Errorf("musiq: model returned %d outputs", len(outs))
	}
	return float64(outs[0][0]), nil
}

// musiqSequence builds the model's input for an image, as Google's
// preprocessing does (tools/musiq/preprocess.py, which matches it): pixels
// to [-1, 1]; the image resized, aspect ratio kept, to a 224 px and a 384 px
// longer side with TF's Gaussian resize, each cut into 32-px patches padded
// to a full square grid of tokens; then every patch of the image as given;
// the rest of the sequence is padding (mask 0).
func musiqSequence(img *imageio.RGB) ([]float32, error) {
	if img.W < 1 || img.H < 1 || max(img.W, img.H) > MUSIQSize {
		return nil, fmt.Errorf("musiq: image is %dx%d; its longer side must be 1-%d px", img.W, img.H, MUSIQSize)
	}
	x := make([]float32, len(img.Pix))
	for i, v := range img.Pix {
		x[i] = float32(float32(v)/255*2) - 1 // float32(): see musiqWeights
	}
	seq := make([]float32, musiqTokens*musiqTokLen)
	n := 0
	for id, l := range musiqScales {
		h, w := musiqResizedSize(img.H, img.W, l)
		r := musiqResize(x, img.H, img.W, h, w)
		grid := (l + musiqPatch - 1) / musiqPatch
		musiqPatches(seq[n*musiqTokLen:], r, h, w, id)
		n += grid * grid
	}
	musiqPatches(seq[n*musiqTokLen:], x, img.H, img.W, len(musiqScales))
	return seq, nil
}

// musiqResizedSize is the size with the longer side at l: each side times
// l/longer, in float32, rounded half to even (as tf.round).
func musiqResizedSize(h, w, l int) (int, int) {
	ratio := float32(l) / float32(max(h, w))
	return int(math.RoundToEven(float64(float32(h) * ratio))), int(math.RoundToEven(float64(float32(w) * ratio)))
}

// musiqResize is tf.image.resize(method=GAUSSIAN, antialias=False) of an
// h x w x 3 image to rh x rw: separable, rows then columns.
func musiqResize(x []float32, h, w, rh, rw int) []float32 {
	wh, ww := musiqWeights(h, rh), musiqWeights(w, rw)
	rows := make([]float32, rh*w*3) // vertical pass
	for oy, k := range wh {
		dst := rows[oy*w*3 : (oy+1)*w*3]
		for _, t := range k {
			src := x[t.i*w*3 : (t.i+1)*w*3]
			for j, v := range src {
				dst[j] += float32(t.w * v)
			}
		}
	}
	out := make([]float32, rh*rw*3)
	for y := 0; y < rh; y++ {
		row := rows[y*w*3 : (y+1)*w*3]
		for ox, k := range ww {
			var r, g, b float32
			for _, t := range k {
				p := row[t.i*3:]
				r += float32(t.w * p[0])
				g += float32(t.w * p[1])
				b += float32(t.w * p[2])
			}
			o := out[(y*rw+ox)*3:]
			o[0], o[1], o[2] = r, g, b
		}
	}
	return out
}

type musiqTap struct {
	i int
	w float32
}

// musiqWeights are TF's ScaleAndTranslate spans for a Gaussian kernel
// (radius 1.5, sigma 0.5), no antialiasing, no translation, normalised per
// output pixel.
//
// Every product is rounded to float32 by an explicit conversion: Go may
// otherwise fuse a multiply and an add into one instruction (it does on
// arm64), skipping that rounding, and a tap exactly 1.5 px away (as when
// 512 px become 224) then falls on the other side of the kernel's edge from
// TF and the PyTorch port: other pixels on Apple-silicon Macs than on PCs.
func musiqWeights(in, out int) [][]musiqTap {
	scale := float32(out) / float32(in)
	inv := 1 / scale
	const radius = float32(1.5)
	res := make([][]musiqTap, out)
	for x := 0; x < out; x++ {
		sample := float32((float32(x) + 0.5) * inv)
		if sample < 0 || sample > float32(in) {
			continue
		}
		start := int(math.Ceil(float64(sample - radius - 0.5)))
		end := int(math.Floor(float64(sample + radius - 0.5)))
		start = min(max(start, 0), in-1)
		end = min(max(end, 0), in-1) + 1
		var taps []musiqTap
		var sum float32
		for i := start; i < end; i++ {
			d := float32(math.Abs(float64(float32(i) + 0.5 - sample)))
			var wt float32
			if d < radius {
				wt = float32(math.Exp(float64(float32(-d*d) / (2 * 0.5 * 0.5))))
			}
			taps = append(taps, musiqTap{i, wt})
			sum += wt
		}
		if float64(sum) >= 1000*math.SmallestNonzeroFloat32 {
			for k := range taps {
				taps[k].w /= sum
			}
		}
		res[x] = taps
	}
	return res
}

// musiqPatches writes an h x w x 3 image's 32-px patches as tokens into dst,
// row by row: tf.image.extract_patches with SAME padding (zeros, the smaller
// half before the image), each followed by its hashed position, the scale id
// and mask 1. A side of n patches maps to grid cell floor(i*10/n).
func musiqPatches(dst, x []float32, h, w, scale int) {
	ch, cw := (h+musiqPatch-1)/musiqPatch, (w+musiqPatch-1)/musiqPatch
	top, left := (ch*musiqPatch-h)/2, (cw*musiqPatch-w)/2
	hash := func(i, n int) int {
		return min(int(math.Floor(float64(float32(i)*(float32(musiqGrid)/float32(n))))), musiqGrid-1)
	}
	for py := 0; py < ch; py++ {
		for px := 0; px < cw; px++ {
			tok := dst[(py*cw+px)*musiqTokLen:]
			for yy := 0; yy < musiqPatch; yy++ {
				sy := py*musiqPatch + yy - top
				if sy < 0 || sy >= h {
					continue
				}
				for xx := 0; xx < musiqPatch; xx++ {
					sx := px*musiqPatch + xx - left
					if sx < 0 || sx >= w {
						continue
					}
					copy(tok[(yy*musiqPatch+xx)*3:(yy*musiqPatch+xx)*3+3], x[(sy*w+sx)*3:(sy*w+sx)*3+3])
				}
			}
			tok[musiqTokLen-3] = float32(hash(py, ch)*musiqGrid + hash(px, cw))
			tok[musiqTokLen-2] = float32(scale)
			tok[musiqTokLen-1] = 1
		}
	}
}

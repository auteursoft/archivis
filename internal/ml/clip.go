package ml

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	ort "github.com/yalue/onnxruntime_go"

	"archivis/internal/imageio"
)

var (
	clipMean = [3]float32{0.48145466, 0.4578275, 0.40821073}
	clipStd  = [3]float32{0.26862954, 0.26130258, 0.27577711}
)

// CLIP computes image and text embeddings in a shared space. It accepts the
// clip-as-service exports (visual.onnx / textual.onnx) as well as Hugging Face
// style exports (vision_model.onnx / text_model.onnx).
type CLIP struct {
	vis, txt  *session
	tok       *Tokenizer
	visOut    int // index of the embedding output
	txtOut    int
	txtInt64  bool
	txtNeedsM bool // text model takes an attention_mask input
	Dim       int
}

// NewCLIP loads the CLIP encoders from dir. The text encoder is optional.
func NewCLIP(dir string, o Options) (*CLIP, error) {
	visPath := firstExisting(dir, "visual.onnx", "vision_model.onnx", "clip_visual.onnx")
	if visPath == "" {
		return nil, fmt.Errorf("no CLIP image model (visual.onnx) in %s", dir)
	}
	c := &CLIP{}
	var err error
	if c.vis, err = openSession(visPath, o); err != nil {
		return nil, err
	}
	if c.visOut, c.Dim, err = pickEmbeddingOutput(c.vis.outputs); err != nil {
		return nil, err
	}
	// The photo index, the aesthetic model and stored embeddings are all
	// 512-dimensional (ViT-B/32); any other export would be silently unusable.
	if c.Dim != EmbedDim {
		return nil, fmt.Errorf("CLIP image model %s outputs %d-dimensional embeddings; Archivis needs CLIP ViT-B/32 (%d)", visPath, c.Dim, EmbedDim)
	}
	if txtPath := firstExisting(dir, "textual.onnx", "text_model.onnx", "clip_textual.onnx"); txtPath != "" {
		if c.txt, err = openSession(txtPath, o); err != nil {
			return nil, err
		}
		var txtDim int
		if c.txtOut, txtDim, err = pickEmbeddingOutput(c.txt.outputs); err != nil {
			return nil, err
		}
		if txtDim != c.Dim {
			return nil, fmt.Errorf("CLIP text model %s outputs %d-dimensional embeddings but the image model %d: they must come from the same CLIP model", txtPath, txtDim, c.Dim)
		}
		for _, in := range c.txt.inputs {
			if strings.Contains(in.Name, "mask") {
				c.txtNeedsM = true
			}
			if strings.Contains(in.Name, "input_ids") && in.DataType == ort.TensorElementDataTypeInt64 {
				c.txtInt64 = true
			}
		}
		if c.tok, err = DefaultTokenizer(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func firstExisting(dir string, names ...string) string {
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// EmbedDim is the CLIP embedding size Archivis stores and searches.
const EmbedDim = 512

// pickEmbeddingOutput finds the rank-2 [batch, dim] output, preferring names
// like "image_embeds"/"text_embeds" when several exist.
func pickEmbeddingOutput(outs []ort.InputOutputInfo) (int, int, error) {
	best, dim := -1, 0
	for i, o := range outs {
		if len(o.Dimensions) != 2 {
			continue
		}
		if best < 0 || strings.Contains(o.Name, "embeds") {
			best, dim = i, int(o.Dimensions[1])
		}
	}
	if best < 0 {
		return 0, 0, errors.New("CLIP model has no [batch, dim] output")
	}
	if dim <= 0 {
		return 0, 0, fmt.Errorf("CLIP model output %q has no fixed embedding size", outs[best].Name)
	}
	return best, dim, nil
}

func (c *CLIP) Close() {
	c.vis.close()
	c.txt.close()
}

// HasText reports whether text search is available.
func (c *CLIP) HasText() bool { return c.txt != nil }

// clipCropOffset is where a centre crop of size s starts in n pixels:
// torchvision's CenterCrop computes int(round((n - s) / 2)), and Python's
// round goes to even on halves (an excess of 1 starts at 0, not 1).
func clipCropOffset(n, s int) int { return int(math.RoundToEven(float64(n-s) / 2)) }

// Preprocess applies CLIP's resize (short side 224, bicubic), center crop and
// normalisation.
func clipPreprocess(img *imageio.RGB) []float32 {
	const S = 224
	var w, h int
	if img.W <= img.H {
		w, h = S, int(float64(S)*float64(img.H)/float64(img.W))
	} else {
		h, w = S, int(float64(S)*float64(img.W)/float64(img.H))
	}
	r := imageio.Resize(img, max(w, S), max(h, S), imageio.Bicubic)
	left, top := clipCropOffset(r.W, S), clipCropOffset(r.H, S)
	out := make([]float32, 3*S*S)
	for y := 0; y < S; y++ {
		for x := 0; x < S; x++ {
			p := r.Pix[((y+top)*r.W+x+left)*3:]
			for ch := 0; ch < 3; ch++ {
				out[ch*S*S+y*S+x] = (float32(p[ch])/255 - clipMean[ch]) / clipStd[ch]
			}
		}
	}
	return out
}

// EmbedImage returns the L2-normalised CLIP embedding of an image.
func (c *CLIP) EmbedImage(img *imageio.RGB) ([]float32, error) {
	t, err := ort.NewTensor(ort.NewShape(1, 3, 224, 224), clipPreprocess(img))
	if err != nil {
		return nil, err
	}
	defer t.Destroy()
	outs, _, err := c.vis.run(t)
	if err != nil {
		return nil, fmt.Errorf("clip image: %w", err)
	}
	if len(outs[c.visOut]) < c.Dim {
		return nil, fmt.Errorf("clip image: model returned %d values, want %d", len(outs[c.visOut]), c.Dim)
	}
	return Normalize(outs[c.visOut][:c.Dim]), nil
}

// EmbedText returns the L2-normalised CLIP embedding of a text query.
func (c *CLIP) EmbedText(text string) ([]float32, error) {
	if c.txt == nil {
		return nil, errors.New("CLIP text model not installed")
	}
	ids, mask := c.tok.Tokenize(text)
	shape := ort.NewShape(1, contextWidth)
	var inputs []ort.Value
	if c.txtInt64 {
		ids64, mask64 := make([]int64, len(ids)), make([]int64, len(mask))
		for i := range ids {
			ids64[i], mask64[i] = int64(ids[i]), int64(mask[i])
		}
		t, err := ort.NewTensor(shape, ids64)
		if err != nil {
			return nil, err
		}
		defer t.Destroy()
		inputs = append(inputs, t)
		if c.txtNeedsM {
			m, err := ort.NewTensor(shape, mask64)
			if err != nil {
				return nil, err
			}
			defer m.Destroy()
			inputs = append(inputs, m)
		}
	} else {
		t, err := ort.NewTensor(shape, ids)
		if err != nil {
			return nil, err
		}
		defer t.Destroy()
		inputs = append(inputs, t)
		if c.txtNeedsM {
			m, err := ort.NewTensor(shape, mask)
			if err != nil {
				return nil, err
			}
			defer m.Destroy()
			inputs = append(inputs, m)
		}
	}
	// inputs must follow the model's declared order
	if len(inputs) == 2 && strings.Contains(c.txt.inputs[0].Name, "mask") {
		inputs[0], inputs[1] = inputs[1], inputs[0]
	}
	outs, _, err := c.txt.run(inputs...)
	if err != nil {
		return nil, fmt.Errorf("clip text: %w", err)
	}
	if len(outs[c.txtOut]) < c.Dim {
		return nil, fmt.Errorf("clip text: model returned %d values, want %d", len(outs[c.txtOut]), c.Dim)
	}
	return Normalize(outs[c.txtOut][:c.Dim]), nil
}

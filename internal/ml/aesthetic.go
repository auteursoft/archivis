package ml

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
)

// Aesthetic predicts how appealing people find a photo from its normalised
// CLIP ViT-B/32 image embedding with a linear head. The built-in model was
// fit to the mean ratings of ~30 people per photo on the EVA dataset
// (tools/validate/fit_aesthetic.py); on held-out photos it agrees with human
// aesthetic ratings at Spearman 0.78 (the LAION predictor it replaced
// managed 0.42); see VALIDATION.md. Models trained from a catalogue's own
// ratings (archivis aesthetic train) have the same form.
type Aesthetic struct {
	// ID is Name plus a hash of the weights, so two models with different
	// weights never share an ID, whatever they are called.
	ID   string
	Name string
	W    []float32
	B    float32
}

//go:embed aesthetic_model.json
var aestheticJSON []byte

var aestheticName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,39}$`)

// NewAesthetic builds a model and derives its ID.
func NewAesthetic(name string, w []float32, b float32) (*Aesthetic, error) {
	if !aestheticName.MatchString(name) {
		return nil, fmt.Errorf("aesthetic model name %q: use 1-40 lower-case letters, digits, '.' or '-'", name)
	}
	if len(w) != EmbedDim {
		return nil, fmt.Errorf("aesthetic model %s has %d weights, want %d", name, len(w), EmbedDim)
	}
	for _, v := range append(w[:len(w):len(w)], b) {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("aesthetic model %s has non-finite weights", name)
		}
	}
	return &Aesthetic{ID: name + "-" + aestheticHash(w, b), Name: name, W: w, B: b}, nil
}

// aestheticHash is the first 12 hex digits of the SHA-256 of the weights and
// bias as little-endian float32s: the exact numbers the model computes with.
func aestheticHash(w []float32, b float32) string {
	h := sha256.New()
	buf := make([]byte, 4)
	for _, v := range append(w[:len(w):len(w)], b) {
		binary.LittleEndian.PutUint32(buf, math.Float32bits(v))
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

type aestheticFile struct {
	Name string    `json:"name"`
	W    []float32 `json:"w"`
	B    float32   `json:"b"`
}

// ParseAesthetic reads a model from JSON ({"name", "w", "b"}; other fields
// are ignored).
func ParseAesthetic(data []byte) (*Aesthetic, error) {
	var m aestheticFile
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("aesthetic model: %w", err)
	}
	return NewAesthetic(m.Name, m.W, m.B)
}

// JSON encodes the model for ParseAesthetic.
func (a *Aesthetic) JSON() []byte {
	b, _ := json.Marshal(aestheticFile{a.Name, a.W, a.B})
	return b
}

// DefaultAesthetic returns the built-in aesthetic model.
func DefaultAesthetic() *Aesthetic {
	a, err := ParseAesthetic(aestheticJSON)
	if err != nil {
		panic("ml: bad embedded aesthetic_model.json: " + err.Error())
	}
	return a
}

// Score returns the predicted mean rating on EVA's 0-10 scale (most
// photographs land between 4 and 7.5).
func (a *Aesthetic) Score(emb []float32) float32 {
	if a == nil || len(emb) != len(a.W) {
		return 0
	}
	return Dot(a.W, emb) + a.B
}

// AestheticPercent maps a 0-10 rating to 0..100: 3 -> 0, 8 -> 100. That
// range covers ~99% of both contest photos (EVA) and everyday ones (BIQ2021).
func AestheticPercent(rating float64) float64 {
	return math.Max(0, math.Min(1, (rating-3)/5)) * 100
}

// BuiltinAestheticID is the ID of the embedded model (checked by a test).
const BuiltinAestheticID = "eva-ridge-828e586c6724"

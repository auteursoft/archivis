package ml

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/auteursoft/archivis/internal/imageio"
)

type musiqCase struct {
	W, H    int
	Tokens  int
	Sum     float64
	AbsSum  float64 `json:"abs_sum"`
	Samples [][2]float64
	Score   float64
}

func musiqCases(t *testing.T) []musiqCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "musiq_ref.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Cases []musiqCase }
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Cases
}

// musiqImage is the fixture's synthetic image (tools/musiq/make_fixture.py).
func musiqImage(w, h int) *imageio.RGB {
	img := imageio.NewRGB(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < 3; c++ {
				img.Pix[(y*w+x)*3+c] = uint8((7*x + 13*y + 50*c + (x*y)%31) % 256)
			}
		}
	}
	return img
}

// The Go input sequence matches the PyTorch port's (which matches Google's
// preprocessing): token for token, scale by scale, padding included.
func TestMUSIQSequenceMatchesReference(t *testing.T) {
	for _, c := range musiqCases(t) {
		seq, err := musiqSequence(musiqImage(c.W, c.H))
		if err != nil {
			t.Fatal(err)
		}
		real := 0
		var sum, abs float64
		for i := 0; i < musiqTokens; i++ {
			if seq[i*musiqTokLen+musiqTokLen-1] == 1 {
				real++
			}
		}
		for _, v := range seq {
			sum += float64(v)
			abs += math.Abs(float64(v))
		}
		// the sequence's real tokens: the padded 224 and 384 px grids, then
		// every native patch
		if want := c.Tokens - (49 + 144) + musiqRealGrid(c.H, c.W, 224) + musiqRealGrid(c.H, c.W, 384); real != want {
			t.Errorf("%dx%d: %d real tokens, want %d", c.W, c.H, real, want)
		}
		if math.Abs(sum-c.Sum) > 1e-3*math.Max(1, math.Abs(c.Sum)) || math.Abs(abs-c.AbsSum) > 1e-4*c.AbsSum {
			t.Errorf("%dx%d: sum %.4f abs %.4f, want %.4f, %.4f", c.W, c.H, sum, abs, c.Sum, c.AbsSum)
		}
		bad := 0
		for _, s := range c.Samples {
			i := int(s[0])
			if d := math.Abs(float64(seq[i]) - s[1]); d > 1e-4 && bad < 5 {
				t.Errorf("%dx%d: value %d (token %d, item %d) = %.6f, want %.6f", c.W, c.H, i, i/musiqTokLen, i%musiqTokLen, seq[i], s[1])
				bad++
			}
		}
	}
}

// musiqRealGrid counts the real (mask 1) tokens of a resized scale.
func musiqRealGrid(h, w, l int) int {
	rh, rw := musiqResizedSize(h, w, l)
	return ((rh + musiqPatch - 1) / musiqPatch) * ((rw + musiqPatch - 1) / musiqPatch)
}

// An image larger than the model's sequence holds is refused, not cut.
func TestMUSIQSequenceRefusesLargeImages(t *testing.T) {
	if _, err := musiqSequence(imageio.NewRGB(MUSIQSize+1, 10)); err == nil {
		t.Fatal("a 513 px image was accepted")
	}
}

// With the model, the score matches the PyTorch port's.
func TestMUSIQScoreMatchesReference(t *testing.T) {
	dir := modelsDir(t)
	m, err := NewMUSIQ(dir, Options{})
	if err != nil {
		if os.Getenv("ARCHIVIS_REQUIRE_MODELS") == "" {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	defer m.Close()
	for _, c := range musiqCases(t) {
		got, err := m.Score(musiqImage(c.W, c.H))
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(got-c.Score) > 0.01 {
			t.Errorf("%dx%d: score %.4f, want %.4f", c.W, c.H, got, c.Score)
		}
	}
}

package ml

import (
	"encoding/json"
	ort "github.com/yalue/onnxruntime_go"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
)

// Model-dependent tests run when ARCHIVIS_TEST_MODELS points at a directory
// containing buffalo_l/ and clip/ (and ONNXRUNTIME_LIB is set if needed).
// Otherwise they are skipped, or with ARCHIVIS_REQUIRE_MODELS=1 (as in CI)
// they fail, so a missing model or library cannot pass unnoticed.
func modelsDir(t *testing.T) string {
	skip := t.Skip
	if os.Getenv("ARCHIVIS_REQUIRE_MODELS") != "" {
		skip = t.Fatal
	}
	d := os.Getenv("ARCHIVIS_TEST_MODELS")
	if d == "" {
		skip("ARCHIVIS_TEST_MODELS not set")
	}
	if err := Init(Options{}); err != nil {
		skip(err)
	}
	return d
}

func TestTokenizerMatchesReference(t *testing.T) {
	var cases []struct {
		Text string
		IDs  []int32
	}
	b, _ := os.ReadFile("testdata/tok_ref.json")
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	tok, err := DefaultTokenizer()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got := tok.Encode(c.Text)
		if len(got) != len(c.IDs) {
			t.Errorf("%q: got %v want %v", c.Text, got, c.IDs)
			continue
		}
		for i := range got {
			if got[i] != c.IDs[i] {
				t.Errorf("%q: got %v want %v", c.Text, got, c.IDs)
				break
			}
		}
	}
}

func cos(a, b []float32) float64 { return float64(Dot(a, b)) }

func TestFacesMatchInsightFace(t *testing.T) {
	dir := modelsDir(t)
	eng, err := NewFaceEngine(filepath.Join(dir, "buffalo_l"), Options{IntraOpThreads: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	var ref map[string][]struct {
		BBox  []float64
		Kps   [][]float64
		Score float64
		Emb   []float32
	}
	b, _ := os.ReadFile("testdata/faces_ref.json")
	json.Unmarshal(b, &ref)

	for _, name := range []string{"t1.jpg", "astronaut.jpg", "cat.jpg"} {
		d, err := imageio.Load("testdata/"+name, 0)
		if err != nil {
			t.Fatal(err)
		}
		faces, err := eng.Detect(d.Img)
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.Embed(d.Img, faces); err != nil {
			t.Fatal(err)
		}
		want := ref[name]
		if len(faces) != len(want) {
			t.Fatalf("%s: %d faces, want %d", name, len(faces), len(want))
		}
		for _, w := range want {
			// match by box center
			best, bd := -1, math.MaxFloat64
			for i, f := range faces {
				dx := float64(f.Box[0]+f.Box[2])/2 - (w.BBox[0]+w.BBox[2])/2
				dy := float64(f.Box[1]+f.Box[3])/2 - (w.BBox[1]+w.BBox[3])/2
				if dd := dx*dx + dy*dy; dd < bd {
					best, bd = i, dd
				}
			}
			f := faces[best]
			c := cos(f.Embedding, w.Emb)
			t.Logf("%s face: box dist %.2fpx score %.3f/%.3f emb cos %.4f yaw %.2f", name, math.Sqrt(bd), f.Score, w.Score, c, f.Yaw())
			if math.Sqrt(bd) > 3 || math.Abs(float64(f.Score)-w.Score) > 0.03 || c < 0.98 {
				t.Errorf("%s: mismatch vs insightface", name)
			}
		}
	}
	// different people in the group photo must be dissimilar to each other
	d, _ := imageio.Load("testdata/t1.jpg", 0)
	faces, _ := eng.Detect(d.Img)
	eng.Embed(d.Img, faces)
	for i := range faces {
		for j := i + 1; j < len(faces); j++ {
			if s := cos(faces[i].Embedding, faces[j].Embedding); s > 0.35 {
				t.Errorf("faces %d,%d too similar: %.3f", i, j, s)
			}
		}
	}
	// the same face at a different scale must match strongly
	half := imageio.Resize(d.Img, d.Img.W/2, d.Img.H/2, imageio.Bilinear)
	hf, _ := eng.Detect(half)
	eng.Embed(half, hf)
	if len(hf) == 0 {
		t.Fatal("no faces at half scale")
	}
	bestSim := float32(0)
	for _, f := range faces {
		bestSim = max(bestSim, Dot(f.Embedding, hf[0].Embedding))
	}
	t.Logf("same face at half resolution: cos %.3f", bestSim)
	if bestSim < 0.6 {
		t.Error("same face at half scale not matched")
	}
}

func TestCLIPMatchesReference(t *testing.T) {
	dir := modelsDir(t)
	c, err := NewCLIP(filepath.Join(dir, "clip"), Options{IntraOpThreads: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var ref struct {
		Texts   []string
		TextEmb [][]float32 `json:"text_emb"`
		Img     map[string][]float32
	}
	b, _ := os.ReadFile("testdata/clip_ref.json")
	json.Unmarshal(b, &ref)
	var texts [][]float32
	for i, s := range ref.Texts {
		e, err := c.EmbedText(s)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, e)
		if cs := cos(e, ref.TextEmb[i]); cs < 0.999 {
			t.Errorf("text %q cos %.5f", s, cs)
		}
	}
	expect := map[string]int{"cat.jpg": 0, "astronaut.jpg": 1, "coffee.jpg": 2, "t1.jpg": 3}
	for name, idx := range expect {
		d, _ := imageio.Load("testdata/"+name, 0)
		e, err := c.EmbedImage(d.Img)
		if err != nil {
			t.Fatal(err)
		}
		cs := cos(e, ref.Img[name])
		best := 0
		for i := range texts {
			if Dot(e, texts[i]) > Dot(e, texts[best]) {
				best = i
			}
		}
		t.Logf("%s: cos vs python %.4f, zero-shot -> %q", name, cs, ref.Texts[best])
		if cs < 0.975 || best != idx { // Go's JPEG decoder upsamples chroma differently from libjpeg
			t.Errorf("%s mismatch", name)
		}
	}
}

func TestDefaultAesthetic(t *testing.T) {
	a := DefaultAesthetic()
	if len(a.W) != 512 || a.ID != BuiltinAestheticID {
		t.Fatalf("model: %d weights, ID %q (the built-in model changed: update BuiltinAestheticID, and keep\n"+
			"eva-ridge-1 catalogues mapped to the old ID in indexer.legacyAesthetic)", len(a.W), a.ID)
	}
	// An embedding pointing along the weights must score above one pointing
	// against them, and both must stay on the 0-10 scale's neighbourhood.
	up, down := make([]float32, 512), make([]float32, 512)
	copy(up, a.W)
	for i := range down {
		down[i] = -up[i]
	}
	up, down = Normalize(up), Normalize(down)
	if hi, lo := a.Score(up), a.Score(down); !(hi > lo) || hi > 30 || lo < -20 {
		t.Fatalf("scores %v %v", hi, lo)
	}
	for r, want := range map[float64]float64{2: 0, 3: 0, 5.5: 50, 8: 100, 9: 100} {
		if got := AestheticPercent(r); math.Abs(got-want) > 1e-9 {
			t.Errorf("AestheticPercent(%v) = %v, want %v", r, got, want)
		}
	}
}

func TestUnrotateFace(t *testing.T) {
	img := imageio.NewRGB(40, 30)
	for _, cw := range []bool{true, false} {
		rot := rotate90(img, cw)
		// a point marked in the rotated image must map back to the same pixel
		px, py := 7, 11 // original coordinates
		var u, v int
		if cw {
			u, v = img.H-1-py, px
		} else {
			u, v = py, img.W-1-px
		}
		if u >= rot.W || v >= rot.H {
			t.Fatalf("bad test geometry")
		}
		f := unrotateFace(Face{Box: [4]float32{float32(u), float32(v), float32(u), float32(v)}}, img.W, img.H, cw)
		if int(f.Box[0]) != px || int(f.Box[1]) != py {
			t.Errorf("cw=%v: got (%v,%v) want (%d,%d)", cw, f.Box[0], f.Box[1], px, py)
		}
	}
}

// The versioned-name fallback must pick the real library, not the macOS
// debug bundle (a directory) or a provider plug-in that sorts nearby.
func TestLibraryInSkipsDebugBundleAndProviders(t *testing.T) {
	dir := t.TempDir()
	name := LibraryName()
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	// names that sort before the real library
	os.MkdirAll(filepath.Join(dir, stem+".1.29.0"+ext+".dSYM", "Contents"), 0o755)
	os.Mkdir(filepath.Join(dir, stem+".0"+ext+".dSYM"), 0o755)
	os.WriteFile(filepath.Join(dir, stem+"_providers_shared"+ext), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, stem+".pc"), []byte("x"), 0o644)
	if got := LibraryIn(dir); got != "" {
		t.Fatalf("picked %s from a directory with no runtime library", got)
	}
	real := filepath.Join(dir, stem+".1.29.0"+ext)
	if ext == ".so" {
		real = filepath.Join(dir, name+".1.29.0")
	}
	os.WriteFile(real, []byte("lib"), 0o644)
	if got := LibraryIn(dir); got != real {
		t.Fatalf("got %q, want %q", got, real)
	}
	// a symlink at the canonical name is followed and preferred
	os.Symlink(filepath.Base(real), filepath.Join(dir, name))
	if got := LibraryIn(dir); got != filepath.Join(dir, name) {
		t.Fatalf("got %q, want the canonical name", got)
	}
}

func TestPickEmbeddingOutputNeedsFixedSize(t *testing.T) {
	outs := []ort.InputOutputInfo{{Name: "image_embeds", Dimensions: ort.NewShape(-1, -1)}}
	if _, _, err := pickEmbeddingOutput(outs); err == nil {
		t.Fatal("dynamic embedding size accepted")
	}
	outs = []ort.InputOutputInfo{{Name: "last_hidden_state", Dimensions: ort.NewShape(-1, 50, 768)}, {Name: "image_embeds", Dimensions: ort.NewShape(-1, 512)}}
	if i, d, err := pickEmbeddingOutput(outs); err != nil || i != 1 || d != 512 {
		t.Fatalf("got %d %d %v", i, d, err)
	}
}

// A huge query must be cheap (only what fits in CLIP's context is
// tokenized) and give the same tokens as the reference's truncation.
func TestLongQueryIsBounded(t *testing.T) {
	tok, _ := DefaultTokenizer()
	long := strings.Repeat("protest at night in the rain ", 20000) + strings.Repeat("x", 400000)
	start := time.Now()
	ids, _ := tok.Tokenize(long)
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("tokenizing a %d KB query took %v", len(long)>>10, el)
	}
	full := tok.Encode(strings.Repeat("protest at night in the rain ", 40))
	for i := 0; i < contextWidth-2; i++ {
		if ids[i+1] != full[i] {
			t.Fatalf("token %d differs from the untruncated encoding", i)
		}
	}
	start = time.Now()
	tok.Tokenize(strings.Repeat("y", 400000)) // one enormous word
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("one 400 KB word took %v", el)
	}
}

func TestAestheticIDFollowsWeights(t *testing.T) {
	a := DefaultAesthetic()
	b, err := ParseAesthetic(a.JSON())
	if err != nil || b.ID != a.ID {
		t.Fatalf("round trip: %v, %q vs %q", err, b.ID, a.ID)
	}
	w := append([]float32(nil), a.W...)
	w[100] = math.Nextafter32(w[100], 1)
	c, err := NewAesthetic(a.Name, w, a.B)
	if err != nil || c.ID == a.ID || c.Name != a.Name {
		t.Fatalf("one weight changed by one ulp kept ID %q (%v)", c.ID, err)
	}
	if d, _ := NewAesthetic(a.Name, a.W, a.B+1); d.ID == a.ID {
		t.Fatal("bias change kept the ID")
	}
	for _, bad := range []string{"", "Bad Name", "-x", "a/b"} {
		if _, err := NewAesthetic(bad, a.W, a.B); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	nan := append([]float32(nil), a.W...)
	nan[0] = float32(math.NaN())
	if _, err := NewAesthetic("x", nan, 0); err == nil {
		t.Error("NaN weight accepted")
	}
	if _, err := NewAesthetic("x", a.W[:10], 0); err == nil {
		t.Error("short model accepted")
	}
}

// Centre-crop offsets match torchvision (Python rounds halves to even).
func TestClipCropOffsetMatchesTorchvision(t *testing.T) {
	// int(round((n - 224) / 2)) in Python
	for n, want := range map[int]int{224: 0, 225: 0, 226: 1, 227: 2, 229: 2, 231: 4, 298: 37} {
		if got := clipCropOffset(n, 224); got != want {
			t.Errorf("n=%d: offset %d, want %d", n, got, want)
		}
	}
}

// Only versioned library names are accepted as fallbacks, not backups.
func TestLibraryInRejectsBackups(t *testing.T) {
	dir := t.TempDir()
	name := LibraryName()
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for _, b := range []string{name + ".backup", name + ".old", stem + ".1.29.0" + ext + ".bak", name + ".1.29.0.orig", stem + "-copy" + ext} {
		os.WriteFile(filepath.Join(dir, b), []byte("x"), 0o644)
	}
	if got := LibraryIn(dir); got != "" {
		t.Fatalf("picked %s", got)
	}
}

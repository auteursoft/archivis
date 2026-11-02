package ml

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
)

// "auto" is CoreML on macOS and the CPU elsewhere; the CoreML providers
// differ only in the units they may use: every one (the Neural Engine
// too), or the CPU and GPU, in both of ORT's calls.
func TestProviders(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "coreml", "linux": "cpu", "windows": "cpu"} {
		if got := resolveProvider("auto", goos); got != want {
			t.Errorf("auto on %s: %q, want %q", goos, got, want)
		}
	}
	if got := resolveProvider("CoreML-GPU", "darwin"); got != "coreml-gpu" {
		t.Errorf("an explicit provider: %q", got)
	}
	if u := coreMLUnits["coreml"]; u.units != "ALL" || u.flags != 0 {
		t.Errorf("coreml: %+v, want every unit", u)
	}
	const cpuAndGPU, aneOnly = 0x020, 0x004 // COREML_FLAG_USE_CPU_AND_GPU, COREML_FLAG_ONLY_ENABLE_DEVICE_WITH_ANE
	if u := coreMLUnits["coreml-gpu"]; u.units != "CPUAndGPU" || u.flags&cpuAndGPU == 0 || u.flags&aneOnly != 0 {
		t.Errorf("coreml-gpu: %+v, want the CPU and GPU only", u)
	}
}

// With ARCHIVIS_TEST_PROVIDER set (and not cpu), its outputs are compared
// with the CPU's on the same photos and texts, and both are timed: what a
// provider changes, if anything, for search and for faces.
func TestProviderMatchesCPU(t *testing.T) {
	prov := os.Getenv("ARCHIVIS_TEST_PROVIDER")
	if prov == "" || prov == "cpu" {
		t.Skip("ARCHIVIS_TEST_PROVIDER not set")
	}
	dir := modelsDir(t)
	cpu, other := Options{IntraOpThreads: 2}, testOptions()
	photos := []string{"t1.jpg", "astronaut.jpg", "cat.jpg", "coffee.jpg"}

	clips := map[string]*CLIP{}
	for name, o := range map[string]Options{"cpu": cpu, prov: other} {
		c, err := NewCLIP(filepath.Join(dir, "clip"), o)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		clips[name] = c
	}
	for _, name := range photos {
		d, err := imageio.Load("testdata/"+name, 0)
		if err != nil {
			t.Fatal(err)
		}
		a, err := clips["cpu"].EmbedImage(d.Img)
		if err != nil {
			t.Fatal(err)
		}
		b, err := clips[prov].EmbedImage(d.Img)
		if err != nil {
			t.Fatal(err)
		}
		c := cos(a, b)
		t.Logf("CLIP image %s: %s vs cpu cos %.5f", name, prov, c)
		if c < 0.99 {
			t.Errorf("CLIP image %s: %s differs from the CPU (cos %.5f)", name, prov, c)
		}
	}
	for _, s := range []string{"a photo of a cat", "protest at night in the rain"} {
		a, err := clips["cpu"].EmbedText(s)
		if err != nil {
			t.Fatal(err)
		}
		b, err := clips[prov].EmbedText(s)
		if err != nil {
			t.Fatal(err)
		}
		c := cos(a, b)
		t.Logf("CLIP text %q: %s vs cpu cos %.5f", s, prov, c)
		if c < 0.99 {
			t.Errorf("CLIP text %q: %s differs from the CPU (cos %.5f)", s, prov, c)
		}
	}

	engines := map[string]*FaceEngine{}
	for name, o := range map[string]Options{"cpu": cpu, prov: other} {
		e, err := NewFaceEngine(filepath.Join(dir, "buffalo_l"), o)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		engines[name] = e
	}
	d, err := imageio.Load("testdata/t1.jpg", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]Face{}
	for name, e := range engines {
		faces, err := e.Detect(d.Img)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Embed(d.Img, faces); err != nil {
			t.Fatal(err)
		}
		found[name] = faces
	}
	if len(found["cpu"]) != len(found[prov]) {
		t.Fatalf("faces in t1.jpg: cpu %d, %s %d", len(found["cpu"]), prov, len(found[prov]))
	}
	for i, f := range found["cpu"] {
		best, bc := -1, -2.0 // the same person: the closest embedding
		for j, g := range found[prov] {
			if c := cos(f.Embedding, g.Embedding); c > bc {
				best, bc = j, c
			}
		}
		g := found[prov][best]
		t.Logf("face %d: score cpu %.3f %s %.3f, embedding cos %.4f", i, f.Score, prov, g.Score, bc)
		if bc < 0.98 {
			t.Errorf("face %d: %s embedding differs from the CPU (cos %.4f)", i, prov, bc)
		}
	}

	// speed: the average of a few runs, after one to warm up (CoreML
	// compiles the model on first use)
	timeIt := func(f func() error) time.Duration {
		if err := f(); err != nil {
			t.Fatal(err)
		}
		const n = 5
		start := time.Now()
		for range n {
			if err := f(); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start) / n
	}
	for _, name := range []string{"cpu", prov} {
		c, e := clips[name], engines[name]
		img := timeIt(func() error { _, err := c.EmbedImage(d.Img); return err })
		txt := timeIt(func() error { _, err := c.EmbedText("protest at night in the rain"); return err })
		faces := timeIt(func() error {
			fs, err := e.Detect(d.Img)
			if err == nil {
				err = e.Embed(d.Img, fs)
			}
			return err
		})
		t.Logf("%-10s CLIP image %v, CLIP text %v, faces (detect + embed, %d faces) %v", name, img, txt, len(found[name]), faces)
	}
}

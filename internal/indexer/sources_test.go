package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

func axis(i int) []float32 {
	v := make([]float32, 512)
	v[i] = 1
	return v
}

// testSources: meme prompts along axis 0, photo prompts along axis 1.
func testSources() *Sources {
	s := &Sources{}
	for i := range sourcePrompts {
		if i < memePrompts {
			s.prompts = append(s.prompts, axis(0))
		} else {
			s.prompts = append(s.prompts, axis(1))
		}
	}
	return s
}

// A named camera makes a camera photo, whatever it shows; otherwise the
// embedding decides between meme and saved image; with no embedding or no
// classifier the source stays to be worked out.
func TestSourceOf(t *testing.T) {
	s := testSources()
	mixed := make([]float32, 512)
	mixed[0], mixed[1] = 0.7071, 0.7071 // as like one as the other: below the threshold
	for _, c := range []struct {
		s      *Sources
		camera bool
		emb    []float32
		want   int
	}{
		{s, true, axis(0), store.SourceCamera},
		{s, false, axis(0), store.SourceMeme},
		{s, false, axis(1), store.SourceSaved},
		{s, false, mixed, store.SourceSaved},
		{s, false, nil, store.SourceUnknown},
		{nil, false, axis(0), store.SourceUnknown},
		{nil, true, nil, store.SourceCamera},
	} {
		if got := c.s.Of(c.camera, c.emb); got != c.want {
			t.Errorf("sources %v, camera %v: %d, want %d", c.s != nil, c.camera, got, c.want)
		}
	}
	if p := s.MemeProb(mixed); p < 0.3 || p > 0.6 {
		t.Errorf("an image as like a meme as a photo: %.2f", p)
	}
}

// Catalogued photos are worked out from their stored embeddings; one with
// none stays to be worked out; with all, everything is worked out again.
func TestClassifySources(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*store.Result
	add := func(cam string, emb []float32) {
		p := fmt.Sprintf("/p/%d.jpg", len(rs))
		ph := store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: cam, Version: store.SchemaVersion}
		if emb != nil {
			ph.CLIP = vindex.EncodeF16(emb)
		}
		rs = append(rs, &store.Result{Photo: ph})
	}
	add("Z 8", axis(0)) // a camera's photo of a screen: a camera photo
	add("", axis(0))    // a meme
	add("", axis(1))    // a saved photo
	add("", nil)        // no embedding yet
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	s := testSources()
	n, err := ClassifySources(context.Background(), st, s, false)
	if err != nil || n != 3 {
		t.Fatalf("worked out %d (%v), want 3", n, err)
	}
	want := []int{store.SourceCamera, store.SourceMeme, store.SourceSaved, store.SourceUnknown}
	ps, err := st.Photos([]int64{rs[0].Photo.ID, rs[1].Photo.ID, rs[2].Photo.ID, rs[3].Photo.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if i := p.ID - rs[0].Photo.ID; p.Source != want[i] {
			t.Errorf("%s: source %d, want %d", p.Path, p.Source, want[i])
		}
	}
	if n, _ := ClassifySources(context.Background(), st, s, false); n != 0 {
		t.Errorf("a second run worked out %d again", n)
	}
	// all: worked out again (as after retag), by the rules as they are now
	for i := range s.prompts {
		s.prompts[i] = axis(1 - min(1, i/memePrompts)) // meme and photo prompts swapped
	}
	if n, _ := ClassifySources(context.Background(), st, s, true); n != 3 {
		t.Errorf("all: worked out %d, want 3", n)
	}
	if n, _ := st.MemeCount(nil); n != 1 {
		t.Errorf("after swapping the prompts: %d memes, want 1 (the former saved photo)", n)
	}
}

// Analysing a photo reads whether a camera is named from the decoded
// metadata itself: the photo's own fields are filled in after analysis.
func TestAnalyzeSourceFromMetadata(t *testing.T) {
	eng := &Engines{Sources: testSources()} // no CLIP: only the camera can decide
	for _, c := range []struct {
		make, model string
		want        int
	}{{"NIKON CORPORATION", "NIKON Z 8", store.SourceCamera}, {"", "iPhone 15 Pro", store.SourceCamera}, {"", "", store.SourceUnknown}} {
		d := &imageio.Decoded{Img: imageio.NewRGB(64, 64)}
		d.Meta.Make, d.Meta.Model = c.make, c.model
		a, err := AnalyzeImage(d, eng)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Result.Photo.Source; got != c.want {
			t.Errorf("camera %q %q: source %d, want %d", c.make, c.model, got, c.want)
		}
	}
}

// Without a classifier (no CLIP text model), camera photos are still worked
// out; the rest wait for one.
func TestClassifySourcesWithoutClassifier(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*store.Result
	for i, cam := range []string{"Z 8", ""} {
		p := fmt.Sprintf("/p/%d.jpg", i)
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: cam,
			CLIP: vindex.EncodeF16(axis(0)), Version: store.SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if n, err := ClassifySources(context.Background(), st, nil, false); err != nil || n != 1 {
		t.Fatalf("worked out %d (%v), want the camera photo", n, err)
	}
	if left, _ := st.SourceInputs(0, false, 10); len(left) != 1 || left[0].ID != rs[1].Photo.ID {
		t.Fatalf("left to work out: %+v", left)
	}
}

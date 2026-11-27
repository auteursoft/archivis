package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/quality"
	"github.com/auteursoft/archivis/internal/store"
)

// stubMUSIQ scores every image the same, recording the sizes it was given.
type stubMUSIQ struct {
	score   float64
	calls   atomic.Int64
	maxSide atomic.Int64
	before  func() // run before scoring (to change the photo meanwhile)
}

func (s *stubMUSIQ) Score(img *imageio.RGB) (float64, error) {
	s.calls.Add(1)
	if m := int64(max(img.W, img.H)); m > s.maxSide.Load() {
		s.maxSide.Store(m)
	}
	if s.before != nil {
		s.before()
	}
	return s.score, nil
}

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), uint8((x + y) % 256), 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
}

// backfillFixture catalogues n photos as indexing without MUSIQ did (no
// score), each with a real file but the last, whose file is gone.
func backfillFixture(t *testing.T, n int) (*store.Store, []store.Photo) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var rs []*store.Result
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("p%d.jpg", i))
		size, mtime := int64(1), int64(1)
		if i < n-1 {
			writeJPEG(t, p, 900, 600)
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			size, mtime = fi.Size(), fi.ModTime().Unix() // as indexing records them
		}
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: size, MTime: mtime, Fingerprint: fmt.Sprintf("f%d", i),
			Technical: 40, ExposureScore: 0.8, ColorScore: 0.9, Blockiness: 1, Overall: 40,
			Aesthetic: sql.NullFloat64{Float64: 5, Valid: i%2 == 0}, Version: store.SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	var ps []store.Photo
	for _, r := range rs {
		ps = append(ps, r.Photo)
	}
	return st, ps
}

// The backfill scores every photo it can read, at the rating's size, and
// works out its technical and overall scores as indexing would; a missing
// file is counted and left for next time; groups are marked stale.
func TestBackfillMUSIQ(t *testing.T) {
	old := backfillBatch
	backfillBatch = 2 // several batches
	defer func() { backfillBatch = old }()
	st, ps := backfillFixture(t, 5)
	o := st.GroupOptions()
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	m := &stubMUSIQ{score: 77}
	var log strings.Builder
	b, err := BackfillMUSIQ(context.Background(), st, m, time.Time{}, 3, &log)
	if err != nil {
		t.Fatal(err)
	}
	if b.Scored != 4 || b.Missing != 1 || b.Failed != 0 || b.Left != 1 || b.Stopped {
		t.Fatalf("stats %+v", b)
	}
	if m.maxSide.Load() > quality.RatingSize {
		t.Fatalf("MUSIQ was given a %d px image", m.maxSide.Load())
	}
	for _, p := range ps[:4] {
		got, err := st.Photos([]int64{p.ID})
		if err != nil {
			t.Fatal(err)
		}
		g := got[0]
		d, err := imageio.Load(p.Path, WorkSize)
		if err != nil {
			t.Fatal(err)
		}
		rating, s, err := Rating(d.Img, m)
		if err != nil {
			t.Fatal(err)
		}
		wantTech := round1(quality.TechnicalWithMUSIQ(rating, quality.Metrics{ExposureScore: 0.8, ColorScore: 0.9}, 1))
		if !g.MUSIQ.Valid || g.MUSIQ.Float64 != 77 || s.Float64 != 77 || g.Technical != wantTech || g.Overall != Overall(wantTech, p.Aesthetic) {
			t.Errorf("photo %d: musiq %v technical %v overall %v, want 77, %v, %v", p.ID, g.MUSIQ, g.Technical, g.Overall, wantTech, Overall(wantTech, p.Aesthetic))
		}
	}
	if !st.GroupsStale(o) {
		t.Error("groups not marked stale after the scores changed")
	}
	// next run: only the missing file is tried again, and it is still missing
	calls := m.calls.Load()
	b, err = BackfillMUSIQ(context.Background(), st, m, time.Time{}, 1, nil)
	if err != nil || b.Scored != 0 || b.Missing != 1 || m.calls.Load() != calls {
		t.Fatalf("second run: %+v %v (%d new calls)", b, err, m.calls.Load()-calls)
	}
	if scored, left, _ := st.MUSIQCounts(); scored != 4 || left != 1 {
		t.Fatalf("counts %d scored, %d left", scored, left)
	}
}

// With its time up, the backfill stops and saves what it did; the next run
// picks up the rest.
func TestBackfillMUSIQStopsAndResumes(t *testing.T) {
	st, _ := backfillFixture(t, 4)
	m := &stubMUSIQ{score: 60}
	b, err := BackfillMUSIQ(context.Background(), st, m, time.Now().Add(-time.Second), 1, nil)
	if err != nil || b.Scored != 0 || !b.Stopped || b.Left != 4 {
		t.Fatalf("out of time at once: %+v %v", b, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.before = func() {
		if m.calls.Load() == 2 {
			cancel() // stop after this photo
		}
	}
	b, err = BackfillMUSIQ(ctx, st, m, time.Time{}, 1, nil)
	if err != nil || b.Scored != 2 || !b.Stopped || b.Left != 2 {
		t.Fatalf("cancelled: %+v %v", b, err)
	}
	m.before = nil
	b, err = BackfillMUSIQ(context.Background(), st, m, time.Time{}, 1, nil)
	if err != nil || b.Scored != 1 || b.Missing != 1 || b.Stopped {
		t.Fatalf("resumed: %+v %v", b, err)
	}
}

// A photo re-indexed while it was being scored keeps the indexer's scores.
func TestBackfillMUSIQSkipsChangedPhotos(t *testing.T) {
	st, ps := backfillFixture(t, 2)
	m := &stubMUSIQ{score: 50}
	m.before = func() { // re-index the photo meanwhile
		p := ps[0]
		p.Technical, p.MUSIQ = 12.3, sql.NullFloat64{Float64: 1, Valid: true}
		if err := st.Save([]*store.Result{{Photo: p}}); err != nil {
			t.Error(err)
		}
	}
	b, err := BackfillMUSIQ(context.Background(), st, m, time.Time{}, 1, nil)
	if err != nil || b.Changed != 1 || b.Scored != 0 {
		t.Fatalf("%+v %v", b, err)
	}
	got, _ := st.Photos([]int64{ps[0].ID})
	if got[0].Technical != 12.3 || got[0].MUSIQ.Float64 != 1 {
		t.Fatalf("the re-indexed scores were overwritten: %v %v", got[0].Technical, got[0].MUSIQ)
	}
}

// Without MUSIQ there is nothing to backfill with.
func TestBackfillMUSIQNeedsTheModel(t *testing.T) {
	st, _ := backfillFixture(t, 1)
	if _, err := BackfillMUSIQ(context.Background(), st, nil, time.Time{}, 1, nil); err == nil {
		t.Fatal("ran without a model")
	}
}

// Indexing with MUSIQ scores the photo at the rating's size, stores the
// score, and rates with it; without MUSIQ, the measurements alone.
func TestRatingWithAndWithoutMUSIQ(t *testing.T) {
	img := imageio.NewRGB(1600, 1000)
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 7 % 251)
	}
	r0, s0, err := Rating(img, nil)
	if err != nil || s0.Valid || r0 != quality.Rate(img) {
		t.Fatalf("without MUSIQ: %v %v %v (want %v)", r0, s0, err, quality.Rate(img))
	}
	m := &stubMUSIQ{score: 66}
	r1, s1, err := Rating(img, m)
	small := quality.RatingImage(img)
	if err != nil || !s1.Valid || s1.Float64 != 66 || r1 != quality.RateWith(quality.Analyze(small), 66) || m.maxSide.Load() != quality.RatingSize {
		t.Fatalf("with MUSIQ: %v %v %v (scored %d px)", r1, s1, err, m.maxSide.Load())
	}
}

// Overall is worked out from the aesthetic score as it is when the MUSIQ
// score is saved: a blend refreshed meanwhile (which leaves seq alone) is
// not undone.
func TestBackfillMUSIQUsesCurrentAesthetic(t *testing.T) {
	st, ps := backfillFixture(t, 2)
	newAes := sql.NullFloat64{Float64: 9, Valid: true}
	m := &stubMUSIQ{score: 70}
	m.before = func() { // the web interface refreshes the blend meanwhile
		if err := st.SetBlendedScores([]store.BlendUpdate{{ID: ps[0].ID, Seq: seqOf(t, st, ps[0].ID), Aesthetic: newAes}}, Overall); err != nil {
			t.Error(err)
		}
	}
	if _, err := BackfillMUSIQ(context.Background(), st, m, time.Time{}, 1, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Photos([]int64{ps[0].ID})
	if g := got[0]; !g.MUSIQ.Valid || g.Aesthetic != newAes || g.Overall != Overall(g.Technical, newAes) {
		t.Fatalf("overall %v from technical %v and aesthetic %v; want %v (the refreshed aesthetic)", g.Overall, g.Technical, g.Aesthetic, Overall(g.Technical, newAes))
	}
}

func seqOf(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	var seq int64
	if err := st.DB.QueryRow(`SELECT seq FROM photos WHERE id = ?`, id).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

// And the other way round: a blend refreshed after MUSIQ changed the
// technical score keeps the new technical score in overall.
func TestBlendRefreshUsesCurrentTechnical(t *testing.T) {
	st, ps := backfillFixture(t, 2)
	seq := seqOf(t, st, ps[0].ID)
	aes := sql.NullFloat64{Float64: 6, Valid: true}
	// MUSIQ rescored it after the blend update was worked out
	if _, err := st.SetMUSIQ([]store.MUSIQUpdate{{ID: ps[0].ID, Seq: seq, MUSIQ: 80, Technical: 77}}, Overall); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBlendedScores([]store.BlendUpdate{{ID: ps[0].ID, Seq: seq, Aesthetic: aes, Overall: Overall(40, aes)}}, Overall); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Photos([]int64{ps[0].ID})
	if g := got[0]; g.Overall != Overall(77, aes) {
		t.Fatalf("overall %v, want %v (from technical 77, not the 40 the update was worked out from)", g.Overall, Overall(77, aes))
	}
}

// A file edited since it was indexed is not scored with the old file's
// stored scores: it is left for the next index, and counted.
func TestBackfillMUSIQSkipsChangedFiles(t *testing.T) {
	st, ps := backfillFixture(t, 3)
	writeJPEG(t, ps[0].Path, 400, 300) // edited: new size
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(ps[1].Path, later, later); err != nil { // touched: new time
		t.Fatal(err)
	}
	m := &stubMUSIQ{score: 70}
	b, err := BackfillMUSIQ(context.Background(), st, m, time.Time{}, 1, nil)
	if err != nil || b.Stale != 2 || b.Scored != 0 || b.Missing != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	if !strings.Contains(b.String(), "2 files changed since they were indexed") {
		t.Fatalf("report: %s", b)
	}
}

// A run stopped by its time limit is continued by the next, so a photo that
// fails slowly cannot use up every run's time; once a pass has reached the
// end, the next run starts again from the beginning.
func TestBackfillMUSIQContinuesWhereItStopped(t *testing.T) {
	st, ps := backfillFixture(t, 4)    // the last file is missing
	writeJPEG(t, ps[0].Path, 700, 500) // photo 0: one MUSIQ fails on (its 512x366 rating image)
	fi, err := os.Stat(ps[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE photos SET size = ?, mtime = ? WHERE id = ?`, fi.Size(), fi.ModTime().Unix(), ps[0].ID); err != nil {
		t.Fatal(err) // as re-indexed
	}
	// each run has time for one photo: the stub stops the run on every call
	m := &cancellingMUSIQ{failW: 512, failH: 366}
	run := func() BackfillStats {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m.cancel = cancel
		b, err := BackfillMUSIQ(ctx, st, m, time.Time{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	scored := func(id int64) bool {
		got, _ := st.Photos([]int64{id})
		return got[0].MUSIQ.Valid
	}
	if b := run(); b.Failed != 1 || b.Scored != 0 { // photo 0 fails, and the time is up
		t.Fatalf("first run: %+v", b)
	}
	if b := run(); b.Scored != 1 || !scored(ps[1].ID) { // not photo 0 again: photo 1
		t.Fatalf("second run: %+v; it started over instead of carrying on", b)
	}
	if b := run(); b.Scored != 1 || !scored(ps[2].ID) {
		t.Fatalf("third run: %+v", b)
	}
	m.cancel = nil
	b, err := BackfillMUSIQ(context.Background(), st, m, time.Time{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	// the rest of the pass (the missing file), then round to photo 0 again
	if b.Missing != 1 || b.Failed != 1 || b.Stopped {
		t.Fatalf("last run: %+v", b)
	}
	if st.GetMeta(backfillCursor) != "0" {
		t.Fatalf("after a whole pass the cursor is %q, want 0", st.GetMeta(backfillCursor))
	}
}

// cancellingMUSIQ fails on images of one size, and stops the run (cancel)
// on every call.
type cancellingMUSIQ struct {
	failW, failH int
	cancel       func()
}

func (c *cancellingMUSIQ) Score(img *imageio.RGB) (float64, error) {
	if c.cancel != nil {
		c.cancel()
	}
	if img.W == c.failW && img.H == c.failH {
		return 0, fmt.Errorf("model failed")
	}
	return 70, nil
}

type failingMUSIQ struct{}

func (failingMUSIQ) Score(*imageio.RGB) (float64, error) { return 0, fmt.Errorf("model crashed") }

// A MUSIQ failure does not lose the photo: it is analysed and rated by its
// measurements, its MUSIQ score left for the backfill, and the error kept.
func TestAnalyzeKeepsPhotoWhenMUSIQFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jpg")
	writeJPEG(t, p, 900, 600)
	d, err := imageio.Load(p, WorkSize)
	if err != nil {
		t.Fatal(err)
	}
	a, err := AnalyzeImage(d, &Engines{MUSIQ: failingMUSIQ{}})
	if err != nil {
		t.Fatalf("the photo was dropped: %v", err)
	}
	ph := a.Result.Photo
	want := round1(quality.Technical(quality.Rate(d.Img), a.Metrics, d.Blockiness))
	if a.MUSIQErr == nil || ph.MUSIQ.Valid || ph.Technical != want {
		t.Fatalf("MUSIQErr %v, musiq %v, technical %v (want %v from the measurements)", a.MUSIQErr, ph.MUSIQ, ph.Technical, want)
	}
}

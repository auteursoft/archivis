package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"archivis/internal/imageio"
	"archivis/internal/store"
)

// Byte-identical files share a fingerprint, so concurrent workers can write
// the same thumbnail path at once. Every write must succeed and leave a
// complete file, with no temporary files behind.
func TestWriteAtomicConcurrentSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thumb.jpg")
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(i)}, 64<<10)
			if err := WriteAtomic(path, body); err != nil {
				errs <- fmt.Errorf("writer %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) != 64<<10 || !bytes.Equal(b, bytes.Repeat(b[:1], len(b))) {
		t.Fatalf("final file is not one writer's complete output (%d bytes, %v)", len(b), err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("leftover files: %v", names)
	}
}

// The CLIP text model is optional; without it there is no Tagger, and
// tagging must quietly produce no tags rather than panic.
func TestTagsWithoutTextModel(t *testing.T) {
	var tg *Tagger
	if tags := tg.Tags(make([]float32, 512)); tags != nil {
		t.Fatalf("got %v", tags)
	}
}

// photodex.db (the catalogue's name before Archivis) is renamed with its
// WAL files so no labels are lost.
func TestLayoutRenamesOldDatabase(t *testing.T) {
	dir := t.TempDir()
	for _, sfx := range []string{"", "-wal", "-shm"} {
		os.WriteFile(filepath.Join(dir, "photodex.db"+sfx), []byte("x"+sfx), 0o644)
	}
	p := Layout{Dir: dir}.DB()
	if filepath.Base(p) != "archivis.db" {
		t.Fatalf("DB() = %s", p)
	}
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if b, err := os.ReadFile(p + sfx); err != nil || string(b) != "x"+sfx {
			t.Fatalf("archivis.db%s: %q %v", sfx, b, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "photodex.db"+sfx)); err == nil {
			t.Fatalf("photodex.db%s still there", sfx)
		}
	}
}

// Two files of the same size whose first and last 64 KiB match but whose
// middles differ (e.g. uncompressed scans with identical margins) are
// different photos: the second must be analysed, not reuse the first's
// analysis, faces and thumbnail.
func TestSameEdgesDifferentMiddleIsNotACopy(t *testing.T) {
	d, err := imageio.Load("../imageio/testdata/astronaut.jpg", 0)
	if err != nil {
		t.Fatal(err)
	}
	src, err := imageio.EncodeJPEG(imageio.Resize(d.Img, 1400, 1400, imageio.Bicubic), 97)
	if err != nil || len(src) < 3*64<<10 {
		t.Fatalf("test JPEG: %d bytes, %v", len(src), err)
	}
	root := t.TempDir()
	a := filepath.Join(root, "a.jpg")
	os.WriteFile(a, src, 0o644)
	b := bytes.Clone(src)
	mid := len(b) / 2
	for i := mid; i < mid+256; i++ {
		b[i] ^= 0x5a
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	layout := Layout{Dir: t.TempDir()}
	run := func() *Indexer {
		ix := New(Config{Roots: []string{root}, Workers: 1}, st, &Engines{}, layout)
		if err := ix.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		return ix
	}
	run()
	os.WriteFile(filepath.Join(root, "b.jpg"), b, 0o644)
	ix := run()
	if n := ix.Stats.Copied.Load(); n != 0 {
		t.Fatalf("b.jpg treated as a byte-identical copy of a.jpg (%d copies)", n)
	}
	fp := func(p string) string {
		var f string
		if err := st.DB.QueryRow(`SELECT fingerprint FROM photos WHERE path = ?`, p).Scan(&f); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return f
	}
	if fp(a) == fp(filepath.Join(root, "b.jpg")) {
		t.Fatal("different files share a fingerprint (and so a thumbnail)")
	}
	// a true copy is still recognised, and keeps every aesthetic score
	var aid int64
	st.DB.QueryRow(`SELECT id FROM photos WHERE path = ?`, a).Scan(&aid)
	st.AddAestheticScores("m-1", map[int64]float64{aid: 6.5})
	cp := filepath.Join(root, "a-copy.jpg")
	os.WriteFile(cp, src, 0o644)
	if ix := run(); ix.Stats.Copied.Load() != 1 {
		t.Fatalf("identical copy not recognised (%d copies)", ix.Stats.Copied.Load())
	}
	var cid int64
	st.DB.QueryRow(`SELECT id FROM photos WHERE path = ?`, cp).Scan(&cid)
	if sc, _ := st.AestheticScores(cid); sc["m-1"] != 6.5 {
		t.Fatalf("copy's aesthetic scores %v", sc)
	}
}

// A catalogue made before full-content fingerprints still recognises moves
// (keeping labels) and true copies, and does not mistake a same-edges file
// for a copy.
func TestLegacyFingerprintCatalogue(t *testing.T) {
	d, _ := imageio.Load("../imageio/testdata/coffee.jpg", 0)
	src, _ := imageio.EncodeJPEG(imageio.Resize(d.Img, 1400, 1400, imageio.Bicubic), 97)
	lookalike := bytes.Clone(src)
	for i := len(src) / 2; i < len(src)/2+256; i++ {
		lookalike[i] ^= 0x5a
	}
	root := t.TempDir()
	a := filepath.Join(root, "a.jpg")
	os.WriteFile(a, src, 0o644)
	st, _ := store.Open(filepath.Join(t.TempDir(), "c.db"))
	defer st.Close()
	layout := Layout{Dir: t.TempDir()}
	run := func() *Indexer {
		ix := New(Config{Roots: []string{root}, Workers: 1}, st, &Engines{}, layout)
		if err := ix.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		return ix
	}
	run()
	// make it look like an old catalogue
	lfp, _ := LegacyFingerprint(a, int64(len(src)))
	var id int64
	st.DB.QueryRow(`SELECT id FROM photos WHERE path = ?`, a).Scan(&id)
	st.DB.Exec(`UPDATE photos SET fingerprint = ? WHERE id = ?`, lfp, id)

	os.WriteFile(filepath.Join(root, "lookalike.jpg"), lookalike, 0o644)
	if ix := run(); ix.Stats.Copied.Load() != 0 || ix.Stats.Moved.Load() != 0 {
		t.Fatalf("same-edges file reused an old record (copied %d, moved %d)", ix.Stats.Copied.Load(), ix.Stats.Moved.Load())
	}
	os.WriteFile(filepath.Join(root, "copy.jpg"), src, 0o644)
	if ix := run(); ix.Stats.Copied.Load() != 1 {
		t.Fatalf("identical copy of an old record not recognised (%d)", ix.Stats.Copied.Load())
	}
	os.Rename(a, filepath.Join(root, "moved.jpg"))
	ix := run()
	var moved int64
	st.DB.QueryRow(`SELECT id FROM photos WHERE path = ?`, filepath.Join(root, "moved.jpg")).Scan(&moved)
	if ix.Stats.Moved.Load() != 1 || moved != id {
		t.Fatalf("move of an old record not recognised (moved %d, id %d -> %d)", ix.Stats.Moved.Load(), id, moved)
	}
}

func TestSameContent(t *testing.T) {
	dir := t.TempDir()
	w := func(n string, b []byte) string { p := filepath.Join(dir, n); os.WriteFile(p, b, 0o644); return p }
	big := bytes.Repeat([]byte("abcdefgh"), 100000)
	other := bytes.Clone(big)
	other[len(other)/2] ^= 1
	for _, tc := range []struct {
		a, b []byte
		want bool
	}{{big, big, true}, {big, other, false}, {nil, nil, true}, {big, big[:len(big)-1], false}} {
		if got, err := sameContent(w("x", tc.a), w("y", tc.b)); err != nil || got != tc.want {
			t.Errorf("len %d vs %d: got %v %v", len(tc.a), len(tc.b), got, err)
		}
	}
}

func TestLockIndexExcludesSecondRun(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockIndex(dir); !errors.Is(err, ErrIndexRunning) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	u2, err := LockIndex(dir)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	u2()
}

// A batch that fails to save (here: one row rejected) must still save the
// other photos, count the failure, and not mark the file as bad forever.
func TestFailedBatchSavesTheRest(t *testing.T) {
	root := t.TempDir()
	jpg, _ := os.ReadFile("../imageio/testdata/coffee.jpg")
	for _, n := range []string{"a.jpg", "bad.jpg", "c.jpg"} {
		b := bytes.Clone(jpg)
		b = append(b, n...) // distinct content
		os.WriteFile(filepath.Join(root, n), b, 0o644)
	}
	st, _ := store.Open(filepath.Join(t.TempDir(), "c.db"))
	defer st.Close()
	if _, err := st.DB.Exec(`CREATE TRIGGER reject BEFORE INSERT ON photos WHEN NEW.path LIKE '%bad.jpg'
		BEGIN SELECT RAISE(ABORT, 'disk full (simulated)'); END`); err != nil {
		t.Fatal(err)
	}
	ix := New(Config{Roots: []string{root}, Workers: 1}, st, &Engines{}, Layout{Dir: t.TempDir()})
	if err := ix.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := st.Count(false); n != 2 {
		t.Fatalf("%d photos saved, want the 2 good ones", n)
	}
	if ix.Stats.Errors.Load() != 1 {
		t.Fatalf("errors %d, want 1", ix.Stats.Errors.Load())
	}
	bad := filepath.Join(root, "bad.jpg")
	info, _ := os.Stat(bad)
	if marked, _ := st.ErrorFor(bad, info.Size(), info.ModTime().Unix()); marked {
		t.Fatal("a save failure marked the file as bad, so it would never be retried")
	}
	st.DB.Exec(`DROP TRIGGER reject`)
	ix = New(Config{Roots: []string{root}, Workers: 1}, st, &Engines{}, Layout{Dir: t.TempDir()})
	ix.Run(context.Background())
	if st.Count(false) != 3 {
		t.Fatal("the next run did not retry the failed photo")
	}
}

// Only a missing categories file means "use the defaults"; an unreadable one
// must stop retagging rather than silently replace custom tags.
func TestLoadCategories(t *testing.T) {
	dir := t.TempDir()
	if c, err := LoadCategories(filepath.Join(dir, "none.txt")); err != nil || c != DefaultCategories {
		t.Fatalf("missing file: %v", err)
	}
	p := filepath.Join(dir, "categories.txt")
	os.WriteFile(p, []byte("darkroom: a darkroom"), 0o644)
	if c, err := LoadCategories(p); err != nil || c != "darkroom: a darkroom" {
		t.Fatalf("custom file: %q %v", c, err)
	}
	unreadable := filepath.Join(dir, "dir.txt") // reading a directory fails, even as root
	os.Mkdir(unreadable, 0o755)
	if _, err := LoadCategories(unreadable); err == nil {
		t.Fatal("read error silently replaced by the defaults")
	}
}

// An empty or malformed categories file must be rejected, not turned into
// "no categories" (which would let retag erase every photo's tags).
func TestParseCategories(t *testing.T) {
	if cats, err := ParseCategories(DefaultCategories); err != nil || len(cats) == 0 {
		t.Fatalf("built-in categories: %d, %v", len(cats), err)
	}
	cats, err := ParseCategories("# my categories\n\nprotest: a protest march | a demonstration\nfire: a fire\n")
	if err != nil || len(cats) != 3 {
		t.Fatalf("valid file: %v %v", cats, err)
	}
	for name, text := range map[string]string{
		"empty":         "",
		"comments only": "# nothing here\n\n",
		"missing colon": "protest: a protest march\nfire a fire\n",
		"no prompts":    "protest: a protest\nfire: | \n",
		"no name":       ": a fire\n",
	} {
		if _, err := ParseCategories(text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseCategories("ok: fine\nbroken line\n"); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name the bad line: %v", err)
	}
}

func TestParseCategoriesRejectsTruncation(t *testing.T) {
	long := "fire: a fire\nprotest: " + strings.Repeat("x", 2<<20) + "\nsport: a match\n"
	if _, err := ParseCategories(long); err == nil {
		t.Fatal("an over-long line truncated the file silently")
	}
	ok := "fire: a fire\nprotest: " + strings.Repeat("a long description ", 5000) + "\n"
	if cats, err := ParseCategories(ok); err != nil || len(cats) != 2 {
		t.Fatalf("a long but reasonable line: %d %v", len(cats), err)
	}
}

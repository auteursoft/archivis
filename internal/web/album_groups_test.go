package web

import (
	"context"
	"fmt"
	"html"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

func unit(v []float32) []float32 {
	var n float32
	for _, x := range v {
		n += x * x
	}
	for i := range v {
		v[i] /= float32Sqrt(n)
	}
	return v
}

func float32Sqrt(x float32) float32 {
	z := x
	for i := 0; i < 30; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// clipNear is an embedding near base (a shot of the same subject).
func clipNear(rng *rand.Rand, base []float32) []byte {
	v := make([]float32, len(base))
	for i := range base {
		v[i] = base[i] + float32(rng.NormFloat64()*0.01)
	}
	return vindex.EncodeF16(unit(v))
}

func clipRandom(rng *rand.Rand) []float32 {
	v := make([]float32, 512)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	return unit(v)
}

// seedOutAround catalogues an album like the one in the report, three
// shots of a painting and three of a crane (each reframed, so their hashes
// differ) and a burst, under /pics/2026/out; and, beside it, a burst and
// two identical copies.
func seedOutAround(t *testing.T, s *Server) map[string]int64 {
	t.Helper()
	cat := s.cat
	rng := rand.New(rand.NewSource(3))
	painting, crane := clipRandom(rng), clipRandom(rng)
	var rs []*store.Result
	add := func(path string, at int64, hash uint64, overall float64, clip []byte) {
		rs = append(rs, &store.Result{Photo: store.Photo{Path: path, Size: 1, MTime: 1, Fingerprint: fmt.Sprintf("%032x", len(rs)+1),
			CameraModel: "Z 8", TakenAt: at, PHash: hash, Overall: overall, CLIP: clip, Version: store.SchemaVersion}})
	}
	t0 := int64(1_763_000_000)
	for i := 0; i < 3; i++ {
		add(fmt.Sprintf("/pics/2026/out/painting%d.nef", i), t0+int64(i)*20, rng.Uint64(), float64(50+i), clipNear(rng, painting))
		add(fmt.Sprintf("/pics/2026/out/crane%d.nef", i), t0+200+int64(i)*25, rng.Uint64(), float64(60+i), clipNear(rng, crane))
		add(fmt.Sprintf("/pics/2026/out/burst%d.nef", i), t0+400+int64(i), rng.Uint64(), float64(30+i), nil)
		add(fmt.Sprintf("/pics/other/burst%d.nef", i), t0+90000+int64(i), rng.Uint64(), float64(40+i), nil)
	}
	add("/pics/other/copy-a.jpg", 0, 0xC0FFEE, 20, nil)
	add("/pics/other/copy-b.jpg", 0, 0xC0FFEE, 21, nil)
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Store.DB.Exec(`INSERT INTO roots (path) VALUES ('/pics')`); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Store.RebuildFolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	groupNow(t, cat)
	ids := map[string]int64{}
	for _, r := range rs {
		ids[r.Photo.Path] = r.Photo.ID
	}
	return ids
}

// An album has Photos, Bursts and Duplicates tabs; its Bursts and
// Duplicates pages list only the groups with photos in it, and highly
// similar shots of one scene are duplicates.
func TestAlbumBurstsAndDuplicates(t *testing.T) {
	s, _, _ := newTestServer(t)
	ids := seedOutAround(t, s)
	h := s.Handler()
	dir := url.QueryEscape("/pics/2026/out")

	// the album's tabs carry it and its filters
	body := html.UnescapeString(get(t, h, "/albums?dir="+dir+"&min_overall=1&lf=sort%3Dnewest"))
	mustContain(t, "album tabs", body,
		`<nav class="albumtabs"`,
		`href="/albums?dir=`+dir+`&lf=sort%3Dnewest&min_overall=1" class="on" aria-current="page">Photos</a>`,
		`href="/bursts?dir=`+dir+`&lf=sort%3Dnewest&min_overall=1">Bursts</a>`,
		`href="/duplicates?dir=`+dir+`&lf=sort%3Dnewest&min_overall=1">Duplicates</a>`)
	if body := get(t, h, "/albums"); strings.Contains(body, `class="albumtabs"`) {
		t.Error("the album list has an album's tabs")
	}

	// everywhere: the painting, the crane and the copies
	body = get(t, h, "/duplicates")
	if c := strings.Count(body, `class="card stack"`); c != 3 {
		t.Fatalf("duplicates: %d sets, want 3", c)
	}
	if strings.Contains(body, `class="albumtabs"`) {
		t.Error("the catalogue's duplicates have an album's tabs")
	}
	// in the album: the painting and the crane, three shots each
	body = html.UnescapeString(get(t, h, "/duplicates?dir="+dir))
	if c := strings.Count(body, `class="card stack"`); c != 2 || strings.Count(body, `>3</span>`) != 2 {
		t.Fatalf("album duplicates: %d sets, want 2 of 3: %.600s", c, body)
	}
	for _, want := range []string{"/pics/2026/out/painting2.nef", "/pics/2026/out/crane2.nef"} { // each set's best
		mustContain(t, "album duplicates", body, fmt.Sprintf(`href="/photo/%d"`, ids[want]))
	}
	if strings.Contains(body, "copy-a.jpg") || strings.Contains(body, "copy-b.jpg") {
		t.Error("album duplicates list copies from beside it")
	}
	mustContain(t, "album duplicates", body,
		`<a href="/albums">Albums</a> › <a href="/albums?dir=%2Fpics" title="/pics">pics</a> › <a href="/albums?dir=%2Fpics%2F2026" title="/pics/2026">2026</a> › <b>out</b>`,
		`href="/duplicates?dir=`+dir+`" class="on" aria-current="page">Duplicates</a>`,
		`href="/albums?dir=`+dir+`">Photos</a>`,
		`<input type="hidden" name="dir" value="/pics/2026/out">`, // filtering stays in the album
		`href="/duplicates?dir=`+dir+`">Reset</a>`,
		"Duplicates in out", "highly similar shots")

	// bursts: the album's own
	body = html.UnescapeString(get(t, h, "/bursts?dir="+dir))
	mustContain(t, "album bursts", body, "/pics/2026/out/burst0.nef", "Bursts & sequences in out",
		`href="/bursts?dir=`+dir+`" class="on" aria-current="page">Bursts</a>`)
	if strings.Contains(body, "/pics/other/") {
		t.Error("album bursts list bursts from beside it")
	}
	if c := strings.Count(body, `<section class="burst">`); c != 1 {
		t.Errorf("album bursts: %d, want 1", c)
	}
	body = get(t, h, "/bursts")
	if c := strings.Count(body, `<section class="burst">`); c != 2 {
		t.Errorf("bursts: %d, want 2", c)
	}
	// filters apply inside the album too
	body = get(t, h, "/duplicates?dir="+dir+"&min_overall=61")
	if c := strings.Count(body, `class="card stack"`); c != 1 || !strings.Contains(body, `>2/3</span>`) {
		t.Fatalf("filtered album duplicates: %d sets; the crane shows 2 of 3", c)
	}
	// an album with none
	body = get(t, h, "/duplicates?dir="+url.QueryEscape("/pics/other/none"))
	mustContain(t, "empty album", body, "No duplicates found in none")
}

// The visual similarity is a setting.
func TestSimilaritySetting(t *testing.T) {
	s, cat, _ := newTestServer(t)
	b := &browser{t: t, h: s.Handler()}
	body := b.do("GET", "/admin/settings", nil).Body.String()
	mustContain(t, "settings", body, `name="similar_clip"`, `value="93"`)
	if rec := b.do("POST", "/admin/settings", url.Values{"similar_clip": {"90"}}); rec.Code != http.StatusSeeOther || cat.Store.GetSettings().SimilarCLIP != 90 {
		t.Fatalf("save: %d %+v", rec.Code, cat.Store.GetSettings())
	}
	if rec := b.do("POST", "/admin/settings", url.Values{"similar_clip": {"50"}}); rec.Code != http.StatusBadRequest || cat.Store.GetSettings().SimilarCLIP != 90 {
		t.Fatalf("out of range: %d %+v", rec.Code, cat.Store.GetSettings())
	}
}

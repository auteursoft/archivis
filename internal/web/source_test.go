package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

// The Photos page and Albums leave memes and screenshots out unless asked;
// the Source filter picks camera photos, saved images or memes; counts and
// the heading say so.
func TestSourceFilterPages(t *testing.T) {
	s, cat, _ := newTestServer(t)
	h := s.Handler()
	var rs []*store.Result
	for i, src := range []int{store.SourceCamera, store.SourceCamera, store.SourceSaved, store.SourceMeme, store.SourceMeme, store.SourceMeme, store.SourceUnknown} {
		p := fmt.Sprintf("/pics/src%d-%d.jpg", src, i)
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: float64(50 + i), Source: src, Version: store.SchemaVersion}})
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	if err := cat.Store.RecordRoots([]string{"/pics"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Store.RebuildFolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	shown := func(body string) (out []int) {
		for _, r := range rs {
			if strings.Contains(body, `title="`+r.Photo.Path) {
				out = append(out, r.Photo.Source)
			}
		}
		return out
	}
	for _, c := range []struct {
		query string
		want  string // sources shown, in id order
		total string
	}{
		{"", "[1 1 2 0]", "4 photos"},
		{"source=all", "[1 1 2 3 3 3 0]", "7 photos"},
		{"source=camera", "[1 1]", "2 photos"},
		{"source=saved", "[2]", "1 photos"},
		{"source=memes", "[3 3 3]", "3 photos"},
		{"source=bogus", "[1 1 2 0]", "4 photos"},
	} {
		for _, page := range []string{"/?", "/albums?dir=%2Fpics&"} {
			body := html.UnescapeString(get(t, h, page+c.query))
			got := shown(body)
			// the cards come best first; compare as sets of sources
			if fmt.Sprint(sortedInts(got)) != fmt.Sprint(sortedInts(parseInts(c.want))) {
				t.Errorf("%s%s: shows sources %v, want %s", page, c.query, got, c.want)
			}
			mustContain(t, page+c.query, body, c.total)
			hidden := strings.Contains(body, "memes & screenshots hidden")
			if wantHidden := c.query == "" || c.query == "source=bogus"; hidden != wantHidden {
				t.Errorf("%s%s: 'hidden' note shown %v", page, c.query, hidden)
			}
		}
	}
	// searches leave memes out too, and say so (no text model here: the
	// heading shows all the same)
	for _, page := range []string{"/?q=dock", "/albums?dir=%2Fpics&q=dock"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, page, nil))
		mustContain(t, page, html.UnescapeString(rec.Body.String()), "Best matches for", "memes & screenshots hidden")
	}
	// so do Bursts and Duplicates
	for _, page := range []string{"/bursts", "/duplicates"} {
		mustContain(t, page, html.UnescapeString(get(t, h, page)), "memes & screenshots hidden")
		if strings.Contains(html.UnescapeString(get(t, h, page+"?source=all")), "memes & screenshots hidden") {
			t.Errorf("%s?source=all: says memes are hidden", page)
		}
	}
	body := get(t, h, "/?source=memes")
	mustContain(t, "source select", body, `<option value="memes" selected>`)
	if strings.Contains(get(t, h, "/"), `" selected>memes`) {
		t.Error("memes chosen by default")
	}
}

func parseInts(s string) []int {
	var out []int
	for _, f := range strings.Fields(strings.Trim(s, "[]")) {
		var n int
		fmt.Sscan(f, &n)
		out = append(out, n)
	}
	return out
}

func sortedInts(xs []int) []int {
	out := slices.Clone(xs)
	slices.Sort(out)
	return out
}

// Album cards count what the album's page shows by default: memes left out.
func TestAlbumCardCountsLeaveMemesOut(t *testing.T) {
	s, cat, _ := newTestServer(t)
	var rs []*store.Result
	for i, src := range []int{store.SourceCamera, store.SourceSaved, store.SourceMeme, store.SourceMeme} {
		p := fmt.Sprintf("/pics/%d.jpg", i)
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: 50, Source: src, Version: store.SchemaVersion}})
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	// as recorded (RecordRoots would add a drive on Windows)
	if _, err := cat.Store.DB.Exec(`INSERT INTO roots (path) VALUES (?)`, "/pics"); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Store.RebuildFolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := get(t, s.Handler(), "/albums")
	mustContain(t, "albums", body, `<span class="count">2</span>`)
	if strings.Contains(body, `<span class="count">4</span>`) {
		t.Error("the album card counts the memes its page hides")
	}
}

// Similar photos leave memes out, as the pages do, unless the photo itself
// is one: a meme's page may list alike memes.
func TestSimilarPhotosLeaveMemesOut(t *testing.T) {
	s, cat, _ := newTestServer(t)
	vec := func(a, b float32) []byte {
		v := make([]float32, 512)
		v[0], v[1] = a, b
		return vindex.EncodeF16(v)
	}
	var rs []*store.Result
	for _, x := range []struct {
		name string
		src  int
		clip []byte
	}{
		{"photo", store.SourceCamera, vec(1, 0)},
		{"meme-near", store.SourceMeme, vec(0.99, 0.14)},    // the closest to photo
		{"other-photo", store.SourceCamera, vec(0.9, 0.44)}, // further
		{"meme-twin", store.SourceMeme, vec(0.98, 0.2)},
	} {
		p := "/s/" + x.name + ".jpg"
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Source: x.src, CLIP: x.clip, Version: store.SchemaVersion}})
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	if err := cat.Refresh(); err != nil {
		t.Fatal(err)
	}
	similar := func(id int64) string {
		body := get(t, s.Handler(), fmt.Sprintf("/photo/%d", id))
		i := strings.Index(body, "<h2>Similar photos</h2>")
		if i < 0 {
			t.Fatalf("photo %d: no similar photos", id)
		}
		return body[i:]
	}
	got := similar(rs[0].Photo.ID)
	if !strings.Contains(got, "/s/other-photo.jpg") || strings.Contains(got, "/s/meme-near.jpg") || strings.Contains(got, "/s/meme-twin.jpg") {
		t.Fatal("a photo's similar photos: want the other photo, not the memes")
	}
	if got := similar(rs[1].Photo.ID); !strings.Contains(got, "/s/meme-twin.jpg") {
		t.Fatal("a meme's similar photos should include the alike meme")
	}
}

// Search by photo leaves memes out of its face matches too, as of its
// similar scenes, and says so.
func TestQueryFaceMatchesLeaveMemesOut(t *testing.T) {
	s, cat, _ := newTestServer(t)
	face := make([]float32, 512)
	face[3] = 1
	var rs []*store.Result
	for _, x := range []struct {
		name string
		src  int
	}{{"camera", store.SourceCamera}, {"meme", store.SourceMeme}} {
		p := "/q/" + x.name + ".jpg"
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Source: x.src, FaceCount: 1, Version: store.SchemaVersion},
			Faces: []store.Face{{X1: .1, Y1: .1, X2: .3, Y2: .4, Score: .9, Embedding: vindex.EncodeF16(face)}}})
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	if err := cat.Refresh(); err != nil {
		t.Fatal(err)
	}
	s.cacheQuery("tok", &catalog.QueryResult{Analysis: &indexer.Analysis{Faces: []ml.Face{{Embedding: face}}, Result: &store.Result{}}})
	body := get(t, s.Handler(), "/query/tok")
	mustContain(t, "query", body, "/q/camera.jpg", "memes &amp; screenshots hidden")
	if strings.Contains(body, "/q/meme.jpg") {
		t.Fatal("a face in a meme matched on Search by photo")
	}
}

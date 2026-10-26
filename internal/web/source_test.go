package web

import (
	"context"
	"fmt"
	"html"
	"slices"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/store"
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

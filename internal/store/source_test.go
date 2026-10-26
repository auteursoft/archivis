package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// sourceFixture: 40 photos under /a and /b, their sources cycling through
// not worked out, camera, saved image, meme; every third has a camera named.
func sourceFixture(t *testing.T) (*Store, []Photo) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var rs []*Result
	for i := 0; i < 40; i++ {
		p := fmt.Sprintf("/%c/%02d.jpg", "ab"[i%2], i)
		ph := Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: float64(i), Source: i % 4, Version: SchemaVersion}
		if i%3 == 0 {
			ph.CameraModel = "Z 8"
		}
		rs = append(rs, &Result{Photo: ph})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	var ps []Photo
	for _, r := range rs {
		ps = append(ps, r.Photo)
	}
	return st, ps
}

// Each source filter keeps what it says, counted alike the quick way
// (leaving memes out: all less the memes) and the plain way.
func TestSourceFilter(t *testing.T) {
	st, ps := sourceFixture(t)
	keep := map[string]func(p Photo) bool{
		"":       func(Photo) bool { return true },
		"photos": func(p Photo) bool { return p.Source != SourceMeme },
		"camera": func(p Photo) bool { return p.Source == SourceCamera },
		"saved":  func(p Photo) bool { return p.Source == SourceSaved },
		"memes":  func(p Photo) bool { return p.Source == SourceMeme },
	}
	for src, ok := range keep {
		for _, f := range []Filter{{Source: src}, {Source: src, Dirs: []string{"/a"}}, {Source: src, MinOverall: 10}} {
			want := 0
			for _, p := range ps {
				if ok(p) && (f.Dirs == nil || strings.HasPrefix(p.Path, "/a/")) && p.Overall >= f.MinOverall {
					want++
				}
			}
			if n, err := st.FilterCount(f); err != nil || n != want {
				t.Errorf("%+v: counted %d (%v), want %d", f, n, err, want)
			}
			f.Limit = 100
			got, total, _, err := st.BrowsePage(f)
			if err != nil || total != want || len(got) != want {
				t.Errorf("%+v: page of %d, total %d (%v), want %d", f, len(got), total, err, want)
			}
			for _, p := range got {
				if !ok(p) {
					t.Errorf("%+v: %s (source %d) shown", f, p.Path, p.Source)
				}
			}
		}
	}
	for _, c := range []struct {
		f       Filter
		narrows bool
	}{{Filter{}, false}, {Filter{Source: "photos"}, false}, {Filter{Source: "memes"}, true}, {Filter{Source: "photos", MinOverall: 1}, true}} {
		if c.f.Narrows() != c.narrows {
			t.Errorf("%+v: Narrows = %v", c.f, !c.narrows)
		}
	}
	if n, _ := st.MemeCount(nil); n != 10 {
		t.Errorf("memes: %d, want 10", n)
	}
	if n, _ := st.MemeCount([]string{"/a"}); n != 0 { // memes are i%4 == 3: odd i, all under /b
		t.Errorf("memes under /a: %d, want 0", n)
	}
	if n, _ := st.MemeCount([]string{"/b", "/a"}); n != 10 {
		t.Errorf("memes under /a and /b: %d, want 10", n)
	}
	// the memes are counted from their own index, not by reading every photo
	rows, err := st.DB.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM photos INDEXED BY photos_memes WHERE source = 3 AND (path >= ? AND path < ?)`, "/b/", "/b0")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		plan = append(plan, d)
	}
	rows.Close()
	if p := strings.Join(plan, "; "); !strings.Contains(p, "photos_memes") {
		t.Errorf("meme count plan: %s", p)
	}
}

// Sources are worked out from what is stored and written without touching
// seq; SourceInputs pages through what is not worked out yet, or all.
func TestSetSources(t *testing.T) {
	st, ps := sourceFixture(t)
	in, err := st.SourceInputs(0, false, 100)
	if err != nil || len(in) != 10 {
		t.Fatalf("not worked out: %d (%v), want 10", len(in), err)
	}
	for _, x := range in {
		p := ps[x.ID-ps[0].ID]
		if p.Source != SourceUnknown || x.Camera != (p.CameraModel != "") {
			t.Fatalf("input %+v for %+v", x, p)
		}
	}
	if page, _ := st.SourceInputs(in[2].ID, true, 3); len(page) != 3 || page[0].ID != in[2].ID+1 {
		t.Fatalf("a page of all after %d: %+v", in[2].ID, page)
	}
	seq := st.MaxSeq()
	set := map[int64]int{}
	for _, x := range in {
		set[x.ID] = SourceMeme
	}
	if err := st.SetSources(set); err != nil {
		t.Fatal(err)
	}
	if st.MaxSeq() != seq {
		t.Fatal("setting sources changed seq")
	}
	if n, _ := st.MemeCount(nil); n != 20 {
		t.Fatalf("memes after setting: %d, want 20", n)
	}
	if left, _ := st.SourceInputs(0, false, 100); len(left) != 0 {
		t.Fatalf("%d still not worked out", len(left))
	}
}

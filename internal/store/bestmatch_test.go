package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// BestMatches gives each group its best matching photo in the filter's
// sort, nil for a group with none; a group nested in another gets its own
// best, and a group of several folders the best of them all.
func TestBestMatches(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for i, x := range []struct {
		path    string
		overall float64
		mono    bool
	}{
		{"/a/1.jpg", 10, true}, {"/a/2.jpg", 80, false}, {"/a/sub/3.jpg", 60, true},
		{"/b/4.jpg", 90, false}, {"/c/5.jpg", 70, true}, {"/c2/6.jpg", 95, true},
	} {
		rs = append(rs, &Result{Photo: Photo{Path: x.path, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Overall: x.overall, Monochrome: x.mono, Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	groups := [][]string{{"/a"}, {"/a/sub"}, {"/b"}, {"/c", "/c2"}}
	less := map[string]func(a, b Photo) bool{
		"":      func(a, b Photo) bool { return a.Overall > b.Overall },
		"worst": func(a, b Photo) bool { return a.Overall < b.Overall },
	}
	// filtered (a scan) and unfiltered (a walk of the overall index) alike
	for _, c := range []struct {
		f    Filter
		want []string
	}{
		{Filter{Mono: "only"}, []string{"/a/sub/3.jpg", "/a/sub/3.jpg", "", "/c2/6.jpg"}},
		{Filter{Mono: "only", Sort: "worst"}, []string{"/a/1.jpg", "/a/sub/3.jpg", "", "/c/5.jpg"}},
		{Filter{}, []string{"/a/2.jpg", "/a/sub/3.jpg", "/b/4.jpg", "/c2/6.jpg"}},
		{Filter{Sort: "worst"}, []string{"/a/1.jpg", "/a/sub/3.jpg", "/b/4.jpg", "/c/5.jpg"}},
	} {
		f, want := c.f, c.want
		got, err := st.BestMatches(f, groups, []int{3, 1, 1, 2}, less[f.Sort])
		if err != nil {
			t.Fatal(err)
		}

		for i, p := range got {
			name := ""
			if p != nil {
				name = p.Path
			}
			if name != want[i] {
				t.Errorf("%+v, group %v: %q, want %q", f, groups[i], name, want[i])
			}
		}
	}
}

// Groups that do not nest are worked out in one SQL pass: the same answers.
func TestBestMatchesByGroup(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for i, x := range []struct {
		path    string
		overall float64
		at      int64
		mono    bool
	}{
		{"/a/1.jpg", 10, 0, true}, {"/a/2.jpg", 80, 500, false}, {"/a/sub/3.jpg", 60, 300, true},
		{"/b/4.jpg", 90, 100, false}, {"/c/5.jpg", 70, 0, true}, {"/c2/6.jpg", 95, 200, true},
	} {
		rs = append(rs, &Result{Photo: Photo{Path: x.path, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Overall: x.overall, TakenAt: x.at, Monochrome: x.mono, Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	groups := [][]string{{"/a"}, {"/b"}, {"/c", "/c2"}}
	for _, c := range []struct {
		f    Filter
		want []string
	}{
		{Filter{Mono: "only"}, []string{"/a/sub/3.jpg", "", "/c2/6.jpg"}},
		{Filter{Mono: "only", Sort: "worst"}, []string{"/a/1.jpg", "", "/c/5.jpg"}},
		{Filter{Sort: "oldest"}, []string{"/a/sub/3.jpg", "/b/4.jpg", "/c2/6.jpg"}}, // undated last
		{Filter{Sort: "newest", MinOverall: 50}, []string{"/a/2.jpg", "/b/4.jpg", "/c2/6.jpg"}},
	} {
		got, err := st.BestMatches(c.f, groups, []int{3, 1, 2}, nil) // no comparator: never needed here
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range got {
			name := ""
			if p != nil {
				name = p.Path
			}
			if name != c.want[i] {
				t.Errorf("%+v, group %v: %q, want %q", c.f, groups[i], name, c.want[i])
			}
		}
	}
}

// An album's subfolders are worked out by the folder each photo's path
// continues into, names with non-ASCII letters too; photos right in the
// album belong to none of them.
func TestBestMatchesBySubfolder(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for i, x := range []struct {
		path    string
		overall float64
	}{
		{"/al/top.jpg", 99}, {"/al/Ålesund/1.jpg", 40}, {"/al/Ålesund/deep/2.jpg", 70}, {"/al/b/3.jpg", 50}, {"/al/b2/4.jpg", 90}, {"/other/b/5.jpg", 95},
	} {
		rs = append(rs, &Result{Photo: Photo{Path: x.path, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Overall: x.overall, Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	groups := [][]string{{"/al/Ålesund"}, {"/al/b"}, {"/al/c"}, {"/al/b2"}}
	if p, ok := siblings(groups); !ok || p != "/al" {
		t.Fatalf("siblings: %q %v", p, ok)
	}
	got, err := st.BestMatches(Filter{Sort: "worst"}, groups, []int{2, 1, 0, 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/al/Ålesund/1.jpg", "/al/b/3.jpg", "", "/al/b2/4.jpg"}
	for i, p := range got {
		name := ""
		if p != nil {
			name = p.Path
		}
		if name != want[i] {
			t.Errorf("group %v: %q, want %q", groups[i], name, want[i])
		}
	}
}

// Unfiltered, a big album is found by walking the sort's index and a small
// one through the path index: both right.
func TestBestMatchesBigAndSmall(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	add := func(p string, overall float64) {
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: overall, Version: SchemaVersion}})
	}
	for i := 0; i < 300; i++ {
		add(fmt.Sprintf("/big/%d.jpg", i), float64(i)/10)
	}
	add("/small/a.jpg", 5)
	add("/small/b.jpg", 7)
	for i := 0; i < 1000; i++ {
		add(fmt.Sprintf("/elsewhere/%d.jpg", i), 99)
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	groups := [][]string{{"/big"}, {"/small"}}
	for _, c := range []struct {
		sort string
		want []string
	}{{"", []string{"/big/299.jpg", "/small/b.jpg"}}, {"worst", []string{"/big/0.jpg", "/small/a.jpg"}}} {
		got, err := st.BestMatches(Filter{Sort: c.sort}, groups, []int{300, 2}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range got {
			if p == nil || p.Path != c.want[i] {
				t.Errorf("sort %q, %v: %v, want %s", c.sort, groups[i], p, c.want[i])
			}
		}
	}
}

// Sorts with a second key (Sharpest: focus, then sharpness; Most faces:
// faces, then overall) pick by both, and Most aesthetic counts an unscored
// photo (as the Photos page does, below any score): for groups and for an
// album's subfolders alike.
func TestBestMatchesSecondKeyAndUnscored(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for i, x := range []struct {
		path         string
		overall      float64
		focus, sharp float64
		faces        int
		aes          sql.NullFloat64
	}{
		// the worse of each tie first: a single-key pick takes the first of equals
		{"/a/1.jpg", 10, 0.9, 0.1, 2, sql.NullFloat64{}}, {"/a/2.jpg", 90, 0.9, 0.8, 2, sql.NullFloat64{}},
		{"/b/3.jpg", 50, 0.2, 0.2, 0, sql.NullFloat64{}},
		{"/s/x/4.jpg", 20, 0.5, 0.1, 1, sql.NullFloat64{}}, {"/s/x/5.jpg", 80, 0.5, 0.9, 1, sql.NullFloat64{}},
		{"/s/y/6.jpg", 30, 0.1, 0.1, 0, sql.NullFloat64{}},
	} {
		rs = append(rs, &Result{Photo: Photo{Path: x.path, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Overall: x.overall,
			FocusScore: x.focus, Sharpness: x.sharp, FaceCount: x.faces, Aesthetic: x.aes, Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	less := map[string]func(a, b Photo) bool{
		"sharpness": func(a, b Photo) bool {
			if a.FocusScore != b.FocusScore {
				return a.FocusScore > b.FocusScore
			}
			return a.Sharpness > b.Sharpness
		},
		"faces": func(a, b Photo) bool {
			if a.FaceCount != b.FaceCount {
				return a.FaceCount > b.FaceCount
			}
			return a.Overall > b.Overall
		},
		"aesthetic": func(a, b Photo) bool { return false }, // all unscored: equal
	}
	for _, c := range []struct {
		groups [][]string
		sort   string
		want   []string
	}{
		{[][]string{{"/a"}, {"/b", "/c"}}, "sharpness", []string{"/a/2.jpg", "/b/3.jpg"}},
		{[][]string{{"/a"}, {"/b", "/c"}}, "faces", []string{"/a/2.jpg", "/b/3.jpg"}},
		{[][]string{{"/a"}, {"/b", "/c"}}, "aesthetic", []string{"/a/1.jpg", "/b/3.jpg"}},
		{[][]string{{"/s/x"}, {"/s/y"}}, "sharpness", []string{"/s/x/5.jpg", "/s/y/6.jpg"}},
		{[][]string{{"/s/x"}, {"/s/y"}}, "faces", []string{"/s/x/5.jpg", "/s/y/6.jpg"}},
		{[][]string{{"/s/x"}, {"/s/y"}}, "aesthetic", []string{"/s/x/4.jpg", "/s/y/6.jpg"}},
	} {
		got, err := st.BestMatches(Filter{Sort: c.sort}, c.groups, []int{2, 1}, less[c.sort])
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range got {
			name := ""
			if p != nil {
				name = p.Path
			}
			if name != c.want[i] {
				t.Errorf("sort %s, group %v: %q, want %q", c.sort, c.groups[i], name, c.want[i])
			}
		}
	}
}

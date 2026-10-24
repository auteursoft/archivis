package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// stackFixture: a burst of four near-identical frames (one stack, top
// scoring 90), two dissimilar frames in the same burst, and singles.
func stackFixture(t *testing.T) (*Store, []*Result) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var rs []*Result
	add := func(at int64, h uint64, overall float64, cam string) {
		p := fmt.Sprintf("/s/%d.jpg", len(rs))
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: cam,
			TakenAt: at, PHash: h, Overall: overall, Version: SchemaVersion}})
	}
	for i, o := range []float64{70, 90, 60, 80} { // the stack: top is rs[1]
		add(1_700_000_000+int64(i), 0xF0F0F0F0F0F0F0F0^uint64(i), o, "CAM")
	}
	add(1_700_000_004, 0x0F0F0F0F0F0F0F0F, 50, "CAM") // same burst, different picture
	for i := 0; i < 5; i++ {
		add(1_600_000_000+int64(i)*1000, uint64(1000+i), float64(10*i), "CAM") // singles
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RebuildGroups(context.Background(), GroupOptions{Gap: 3, Hamming: 6}); err != nil {
		t.Fatal(err)
	}
	return st, rs
}

func ids(ps []Photo) []int64 {
	var out []int64
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// Stacked browsing shows a stack as its top alone; a filter that leaves the
// top out shows the matching frames instead; the total counts every photo;
// paging knows whether more follow.
func TestBrowseStacked(t *testing.T) {
	st, rs := stackFixture(t)
	id := func(i int) int64 { return rs[i].Photo.ID }

	ps, total, more, err := st.BrowsePage(Filter{Stacked: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if total != len(rs) || more || len(ps) != len(rs)-3 {
		t.Fatalf("stacked: %d shown of %d, more %v", len(ps), total, more)
	}
	for _, p := range ps {
		if p.ID == id(0) || p.ID == id(2) || p.ID == id(3) {
			t.Fatalf("hidden frame %d shown", p.ID)
		}
		if p.ID == id(1) && (p.StackSize != 4 || p.StackTop != p.ID) {
			t.Fatalf("top: stack %d of %d", p.StackSize, p.StackTop)
		}
	}
	// without stacking, everything
	if ps, _, _, _ := st.BrowsePage(Filter{Limit: 100}); len(ps) != len(rs) {
		t.Fatalf("unstacked: %d", len(ps))
	}
	// a filter the top fails but frames pass (a tag on three frames): they show
	for _, i := range []int{0, 2, 3} {
		if _, err := st.DB.Exec(`INSERT INTO tags (photo_id, tag, score) VALUES (?, 'x', 1)`, id(i)); err != nil {
			t.Fatal(err)
		}
	}
	ps, _, _, _ = st.BrowsePage(Filter{Stacked: true, Tag: "x", Limit: 100})
	got := map[int64]bool{}
	for _, p := range ps {
		got[p.ID] = true
	}
	if !got[id(0)] || !got[id(2)] || !got[id(3)] || got[id(1)] {
		t.Fatalf("top filtered out: %v", ids(ps))
	}
	// a filter the top passes: frames stay hidden behind it
	ps, _, _, _ = st.BrowsePage(Filter{Stacked: true, MinOverall: 55, Limit: 100})
	if g := ids(ps); !reflect.DeepEqual(g, []int64{id(1)}) { // rs[4] scores 50
		t.Fatalf("top kept: %v", g)
	}
	// paging: exact pages, more until the last
	var all []int64
	for off := 0; ; off += 2 {
		ps, _, more, err := st.BrowsePage(Filter{Stacked: true, Limit: 2, Offset: off})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, ids(ps)...)
		if !more {
			break
		}
	}
	if len(all) != len(rs)-3 {
		t.Fatalf("paged %d, want %d", len(all), len(rs)-3)
	}
}

// Unfiltered stacked browsing still walks the overall index: no sort of
// the whole catalogue.
func TestBrowseStackedUsesIndex(t *testing.T) {
	st, _ := stackFixture(t)
	f := Filter{Stacked: true}
	rows, err := st.DB.Query(`EXPLAIN QUERY PLAN SELECT id FROM photos WHERE stacked = 0` + f.order() + ` LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		plan = append(plan, d)
	}
	if p := strings.Join(plan, "; "); strings.Contains(p, "TEMP B-TREE") || !strings.Contains(p, "photos_overall") {
		t.Fatalf("plan: %s", p)
	}
}

// StackMembers lists a stack's frames best first; not a stack top: none.
func TestStackMembers(t *testing.T) {
	st, rs := stackFixture(t)
	ms, err := st.StackMembers(rs[1].Photo.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{rs[1].Photo.ID, rs[3].Photo.ID, rs[0].Photo.ID, rs[2].Photo.ID}
	if !reflect.DeepEqual(ids(ms), want) {
		t.Fatalf("members %v, want %v", ids(ms), want)
	}
	if ms, _ := st.StackMembers(rs[4].Photo.ID); ms != nil {
		t.Fatalf("not a top: %v", ids(ms))
	}
}

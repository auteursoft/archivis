package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// Each account's stars are its own; starring twice or unstarring an
// unstarred photo changes nothing; a deleted photo leaves every list; the
// Starred filter lists and counts one account's stars.
func TestStars(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for i := 0; i < 4; i++ {
		p := fmt.Sprintf("/s/%d.jpg", i)
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: float64(i), Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	id := func(i int) int64 { return rs[i].Photo.ID }
	const ann, bob = 7, 8
	for _, x := range []struct {
		user  int64
		photo int
	}{{ann, 0}, {ann, 1}, {ann, 1}, {bob, 2}, {0, 3}} {
		if err := st.SetStar(x.user, id(x.photo), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetStar(ann, 999999, true); !errors.Is(err, ErrNoPhoto) {
		t.Fatalf("starring a missing photo: %v", err)
	}
	if err := st.SetStar(bob, id(0), false); err != nil { // never starred
		t.Fatal(err)
	}
	count := func(user int64) int {
		n, err := st.StarCount(user)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(ann) != 2 || count(bob) != 1 || count(0) != 1 {
		t.Fatalf("counts: ann %d, bob %d, shared %d", count(ann), count(bob), count(0))
	}
	got, err := st.Starred(ann, []int64{id(0), id(1), id(2), id(3)})
	if err != nil || len(got) != 2 || !got[id(0)] || !got[id(1)] {
		t.Fatalf("ann's stars: %v %v", got, err)
	}
	f := Filter{Starred: true, StarredBy: ann, Limit: 10}
	if n, _ := st.FilterCount(f); n != 2 {
		t.Fatalf("ann's starred count: %d", n)
	}
	if ps, _, _, _ := st.BrowsePage(Filter{Starred: true, StarredBy: bob, Limit: 10}); len(ps) != 1 || ps[0].ID != id(2) {
		t.Fatalf("bob's starred page: %+v", ps)
	}
	if !f.Narrows() {
		t.Fatal("the starred filter should narrow")
	}
	// unstar, and a deleted photo leaves the lists
	if err := st.SetStar(ann, id(0), false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`DELETE FROM photos WHERE id = ?`, id(1)); err != nil {
		t.Fatal(err)
	}
	if count(ann) != 0 {
		t.Fatalf("ann still has %d stars", count(ann))
	}
}

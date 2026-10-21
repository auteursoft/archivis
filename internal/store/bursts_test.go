package store

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
)

// Paging through bursts must give exactly the bursts a full grouping gives,
// newest first, each once and complete, even with several cameras shooting
// at once (runs interleave, and some are still open when a page fills).
func TestBurstsPageExactly(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rng := rand.New(rand.NewSource(1))
	type shot struct {
		id  int64
		cam string
		at  int64
	}
	var rs []*Result
	cams := []string{"NIKON D810", "ILCE-7RM4", "ILCE-1M2"}
	at := int64(1_700_000_000)
	for i := 0; i < 1500; i++ {
		// mostly frames a second or two apart, sometimes a long pause
		if rng.Intn(6) == 0 {
			at -= int64(5 + rng.Intn(120))
		} else {
			at -= int64(rng.Intn(3))
		}
		cam := cams[rng.Intn(len(cams))]
		p := fmt.Sprintf("/p/%d", i)
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: cam,
			TakenAt: at, Overall: float64(rng.Intn(100)), FocusScore: rng.Float64(), Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	var shots []shot
	for _, r := range rs {
		shots = append(shots, shot{r.Photo.ID, r.Photo.CameraModel, r.Photo.TakenAt})
	}

	// the reference: group everything, then sort
	reference := func(gap int64, minSize int, keep func(int64) bool) [][]int64 {
		s := append([]shot(nil), shots...)
		sort.Slice(s, func(i, j int) bool {
			if s[i].at != s[j].at {
				return s[i].at > s[j].at
			}
			return s[i].id > s[j].id
		})
		type run struct {
			ids         []int64
			start, last int64
		}
		open := map[string]*run{}
		var all []*run
		for _, x := range s {
			if !keep(x.id) {
				continue
			}
			r := open[x.cam]
			if r != nil && r.last-x.at > gap {
				all = append(all, r)
				r = nil
			}
			if r == nil {
				r = &run{start: x.at}
				open[x.cam] = r
			}
			r.ids = append(r.ids, x.id)
			r.last = x.at
		}
		for _, r := range open {
			all = append(all, r)
		}
		var out [][]int64
		sort.Slice(all, func(i, j int) bool {
			if all[i].start != all[j].start {
				return all[i].start > all[j].start
			}
			return all[i].ids[0] > all[j].ids[0]
		})
		for _, r := range all {
			if len(r.ids) >= minSize {
				ids := append([]int64(nil), r.ids...)
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				out = append(out, ids)
			}
		}
		return out
	}
	focus := map[int64]float64{}
	for _, r := range rs {
		focus[r.Photo.ID] = r.Photo.FocusScore
	}

	for _, tc := range []struct {
		gap     int64
		minSize int
		page    int
		filter  Filter
		keep    func(int64) bool
	}{
		{3, 3, 7, Filter{}, func(int64) bool { return true }},
		{1, 3, 5, Filter{}, func(int64) bool { return true }},
		{10, 2, 4, Filter{}, func(int64) bool { return true }},
		{3, 3, 6, Filter{MinFocus: 0.4}, func(id int64) bool { return focus[id] >= 0.4 }},
	} {
		want := reference(tc.gap, tc.minSize, tc.keep)
		if len(want) < 3*tc.page {
			t.Fatalf("gap %d: only %d bursts, test data too thin", tc.gap, len(want))
		}
		var got [][]int64
		for off := 0; ; off += tc.page {
			f := tc.filter
			f.Offset, f.Limit = off, tc.page
			bs, more, err := st.Bursts(f, tc.gap, tc.minSize)
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range bs {
				var ids []int64
				for _, p := range b.Photos {
					ids = append(ids, p.ID)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				got = append(got, ids)
			}
			if more != (off+tc.page < len(want)) {
				t.Fatalf("gap %d offset %d: more=%v with %d bursts in all", tc.gap, off, more, len(want))
			}
			if !more {
				break
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("gap %d minSize %d: paged bursts differ from the full grouping (%d vs %d bursts)", tc.gap, tc.minSize, len(got), len(want))
		}
	}
}

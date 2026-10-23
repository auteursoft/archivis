package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// groupFixture is a catalogue of shots from several cameras, in bursts of
// near-identical frames, with exact copies, undated and camera-less photos
// and many photos stamped with one second (a camera's default clock).
func groupFixture(t *testing.T) (*Store, []Photo) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rng := rand.New(rand.NewSource(2))
	cams := []string{"NIKON D810", "ILCE-7RM4", "ILCE-1M2", ""}
	var rs []*Result
	add := func(cam string, at int64, hash uint64) {
		p := fmt.Sprintf("/p/%d", len(rs))
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: cam,
			TakenAt: at, PHash: hash, Overall: float64(rng.Intn(20)), Technical: rng.Float64(), Aesthetic: sql.NullFloat64{Float64: rng.Float64(), Valid: true},
			FocusScore: rng.Float64(), FaceCount: rng.Intn(4), Version: SchemaVersion}})
	}
	at := int64(1_600_000_000)
	scene := rng.Uint64()
	for i := 0; i < 1200; i++ {
		if rng.Intn(6) == 0 {
			at += int64(4 + rng.Intn(100))
		} else {
			at += int64(rng.Intn(3))
		}
		if rng.Intn(4) == 0 {
			scene = rng.Uint64() // a new subject
		}
		h := scene
		for k := rng.Intn(10); k > 0; k-- {
			h ^= 1 << rng.Intn(64)
		}
		switch rng.Intn(20) {
		case 0:
			h = 0 // no hash
		case 1:
			add("", 0, h) // undated
			continue
		}
		add(cams[rng.Intn(len(cams))], at, h)
	}
	for i := 0; i < 60; i++ {
		add("CAM", 946684800, rng.Uint64()) // all at 2000-01-01 00:00:00
	}
	for i := 0; i < 40; i++ { // exact copies
		src := rs[rng.Intn(len(rs))].Photo
		add(src.CameraModel, src.TakenAt+int64(rng.Intn(2)*1000), src.PHash)
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

type groupState struct {
	photos map[int64][5]int64 // burst_id, stack_top, stack_size, stacked, dup_id
	groups map[[2]int64][]int64
}

// referenceGroups groups the photos the simple way: all in memory.
func referenceGroups(ps []Photo, gap int64, hamming int) groupState {
	out := groupState{photos: map[int64][5]int64{}, groups: map[[2]int64][]int64{}}
	for _, p := range ps {
		out.photos[p.ID] = [5]int64{}
	}
	s := append([]Photo(nil), ps...)
	sort.Slice(s, func(i, j int) bool {
		return s[i].TakenAt < s[j].TakenAt || (s[i].TakenAt == s[j].TakenAt && s[i].ID < s[j].ID)
	})
	open := map[string][]Photo{}
	var runs [][]Photo
	for _, p := range s {
		if p.TakenAt == 0 {
			continue
		}
		cam := p.CameraMake + "\x00" + p.CameraModel
		if r := open[cam]; len(r) > 0 && p.TakenAt-r[len(r)-1].TakenAt > gap {
			runs = append(runs, r)
			open[cam] = nil
		}
		open[cam] = append(open[cam], p)
	}
	for _, r := range open {
		runs = append(runs, r)
	}
	for _, r := range runs {
		if len(r) < 2 {
			continue
		}
		var gs []*gphoto
		var ids []int64
		for _, p := range r {
			gs = append(gs, &gphoto{id: p.ID, phash: p.PHash, overall: p.Overall})
			ids = append(ids, p.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		out.groups[[2]int64{GroupBurst, ids[0]}] = ids
		stacks := stackFrames(gs, hamming)
		for _, id := range ids {
			w := stacks[id]
			out.photos[id] = [5]int64{ids[0], w[0], w[1], w[2], 0}
		}
	}
	byHash := map[uint64][]int64{}
	for _, p := range ps {
		if p.PHash != 0 {
			byHash[p.PHash] = append(byHash[p.PHash], p.ID)
		}
	}
	for _, ids := range byHash {
		if len(ids) < 2 {
			continue
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		out.groups[[2]int64{GroupDuplicate, ids[0]}] = ids
		for _, id := range ids {
			x := out.photos[id]
			x[4] = ids[0]
			out.photos[id] = x
		}
	}
	return out
}

func storedGroups(t *testing.T, st *Store) groupState {
	t.Helper()
	out := groupState{photos: map[int64][5]int64{}, groups: map[[2]int64][]int64{}}
	rows, err := st.DB.Query(`SELECT id, burst_id, stack_top, stack_size, stacked, dup_id FROM photos`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var x [5]int64
		if err := rows.Scan(&id, &x[0], &x[1], &x[2], &x[3], &x[4]); err != nil {
			t.Fatal(err)
		}
		out.photos[id] = x
	}
	rows.Close()
	rows, err = st.DB.Query(`SELECT kind, id, size, members FROM photo_groups`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id int64
		var size int
		var m string
		if err := rows.Scan(&kind, &id, &size, &m); err != nil {
			t.Fatal(err)
		}
		var ids []int64
		json.Unmarshal([]byte(m), &ids)
		if len(ids) != size {
			t.Fatalf("group %d/%d: size %d, %d members", kind, id, size, len(ids))
		}
		out.groups[[2]int64{kind, id}] = ids
	}
	return out
}

func compareGroups(t *testing.T, got, want groupState) {
	t.Helper()
	for id, w := range want.photos {
		if g := got.photos[id]; g != w {
			t.Fatalf("photo %d: got %v, want %v", id, g, w)
		}
	}
	if !reflect.DeepEqual(got.groups, want.groups) {
		t.Fatalf("groups differ: got %d, want %d", len(got.groups), len(want.groups))
	}
}

// The rebuild must give exactly what grouping everything in memory gives,
// whatever the page size, write nothing when nothing changed, and leave
// photos' seq alone; changing the rules regroups.
func TestRebuildGroups(t *testing.T) {
	st, ps := groupFixture(t)
	defer func(n int) { scanPage = n }(scanPage)
	scanPage = 37 // runs and same-second photos span pages
	seq := st.MaxSeq()
	ctx := context.Background()
	opt := GroupOptions{Gap: 3, Hamming: 6}
	if !st.GroupsStale(opt) {
		t.Fatal("new catalogue's groups are not stale")
	}
	stats, err := st.RebuildGroups(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Bursts == 0 || stats.Stacks == 0 || stats.Duplicates == 0 {
		t.Fatalf("fixture too plain: %s", stats)
	}
	compareGroups(t, storedGroups(t, st), referenceGroups(ps, 3, 6))
	if st.MaxSeq() != seq {
		t.Fatal("rebuild changed seq")
	}
	if st.GroupsStale(opt) {
		t.Fatal("stale right after a rebuild")
	}
	again, err := st.RebuildGroups(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	if again.PhotoWrites != 0 || again.GroupWrites != 0 {
		t.Fatalf("second rebuild wrote: %s", again)
	}

	for _, o := range []GroupOptions{{Gap: 1, Hamming: 2}, {Gap: 20, Hamming: 10}, {Gap: 3, Hamming: 6}} {
		if !st.GroupsStale(o) {
			t.Fatalf("%+v: not stale", o)
		}
		if _, err := st.RebuildGroups(ctx, o); err != nil {
			t.Fatal(err)
		}
		compareGroups(t, storedGroups(t, st), referenceGroups(ps, o.Gap, o.Hamming))
	}

	// a new photo makes the groups stale
	if err := st.Save([]*Result{{Photo: Photo{Path: "/new", Size: 1, MTime: 1, Fingerprint: "/new", Version: SchemaVersion}}}); err != nil {
		t.Fatal(err)
	}
	if !st.GroupsStale(opt) {
		t.Fatal("not stale after a new photo")
	}
}

// Deleting a stack's top shows its other frames at once and marks the
// groups stale, every time.
func TestDeleteStackTop(t *testing.T) {
	st, _ := groupFixture(t)
	opt := GroupOptions{Gap: 3, Hamming: 6}
	for round := 0; round < 2; round++ {
		if _, err := st.RebuildGroups(context.Background(), opt); err != nil {
			t.Fatal(err)
		}
		var top int64
		if err := st.DB.QueryRow(`SELECT stack_top FROM photos WHERE stacked = 1 LIMIT 1`).Scan(&top); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`DELETE FROM photos WHERE id = ?`, top); err != nil {
			t.Fatal(err)
		}
		var n int
		st.DB.QueryRow(`SELECT COUNT(*) FROM photos WHERE stack_top = ?`, top).Scan(&n)
		if n != 0 {
			t.Fatalf("round %d: %d frames still stacked under deleted %d", round, n, top)
		}
		if !st.GroupsStale(opt) {
			t.Fatalf("round %d: not stale after a delete", round)
		}
	}
}

// Stacks: the top is the best frame, frames join only within the threshold
// of the top (no chaining along a pan), ties go to the lower id, and frames
// without a hash stay alone.
func TestStackFrames(t *testing.T) {
	f := func(id int64, h uint64, overall float64) *gphoto { return &gphoto{id: id, phash: h, overall: overall} }
	got := stackFrames([]*gphoto{
		f(1, 1<<40|0b0000, 5),
		f(2, 1<<40|0b0001, 9), // top: best
		f(3, 1<<40|0b0011, 7), // 1 bit from 2
		f(4, 1<<40|0b0111, 6), // 2 bits from 2: not with t=1, though 1 bit from 3
		f(5, 0, 9),            // no hash
		f(6, 0b1111<<8, 4),
		f(7, 0b1111<<8, 4), // ties 6: 6 is top
	}, 1)
	want := map[int64][3]int64{
		1: {2, 3, 1}, 2: {2, 3, 0}, 3: {2, 3, 1},
		6: {6, 2, 0}, 7: {6, 2, 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Paging through groups gives each matching group once, in sort order,
// with only its matching members, best first: unfiltered, with a filter
// checked group by group, and with one found from the photos.
func TestGroupPage(t *testing.T) {
	st, ps := groupFixture(t)
	if _, err := st.RebuildGroups(context.Background(), GroupOptions{Gap: 3, Hamming: 6}); err != nil {
		t.Fatal(err)
	}
	byID := map[int64]Photo{}
	for _, p := range ps {
		byID[p.ID] = p
	}
	mid := ps[len(ps)/2].TakenAt
	filters := []struct {
		name string
		f    Filter
		keep func(Photo) bool
	}{
		{"none", Filter{}, func(Photo) bool { return true }},
		{"overall", Filter{MinOverall: 12}, func(p Photo) bool { return p.Overall >= 12 }},
		{"date", Filter{From: mid}, func(p Photo) bool { return p.TakenAt >= mid }},
	}
	sorts := []string{"", "worst", "newest", "oldest", "technical", "aesthetic", "sharpness", "colorful", "faces"}
	for _, kind := range []int{GroupBurst, GroupDuplicate} {
		for _, minSize := range []int{2, 3} {
			for _, fl := range filters {
				for _, so := range sorts {
					name := fmt.Sprintf("kind%d/min%d/%s/%s", kind, minSize, fl.name, so)
					// reference: every group in sort order, kept if a member matches
					all, err := st.groupIDs(`SELECT id FROM photo_groups WHERE kind = ? AND size >= ? ORDER BY `+
						groupSort(so)+`, id DESC`, kind, minSize)
					if err != nil {
						t.Fatal(err)
					}
					var want []int64
					wantMembers := map[int64][]int64{}
					for _, g := range all {
						var m string
						st.DB.QueryRow(`SELECT members FROM photo_groups WHERE kind = ? AND id = ?`, kind, g).Scan(&m)
						var ids []int64
						json.Unmarshal([]byte(m), &ids)
						var keep []int64
						for _, id := range ids {
							if fl.keep(byID[id]) {
								keep = append(keep, id)
							}
						}
						if len(keep) > 0 {
							sort.Slice(keep, func(i, j int) bool {
								a, b := byID[keep[i]], byID[keep[j]]
								return a.Overall > b.Overall || (a.Overall == b.Overall && a.ID < b.ID)
							})
							want = append(want, g)
							wantMembers[g] = keep
						}
					}
					var got []int64
					f := fl.f
					f.Sort, f.Limit = so, 7
					for page := 0; ; page++ {
						f.Offset = page * f.Limit
						gs, more, err := st.GroupPage(kind, f, minSize)
						if err != nil {
							t.Fatalf("%s: %v", name, err)
						}
						for _, g := range gs {
							got = append(got, g.ID)
							var ids []int64
							for _, m := range g.Members {
								ids = append(ids, m.ID)
							}
							if !reflect.DeepEqual(ids, wantMembers[g.ID]) {
								t.Fatalf("%s: group %d members %v, want %v", name, g.ID, ids, wantMembers[g.ID])
							}
						}
						if !more.More {
							break
						}
					}
					if len(want) == 0 && kind == GroupBurst && fl.name == "none" {
						t.Fatalf("%s: fixture has no groups", name)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: got %d groups %v, want %d %v", name, len(got), head(got), len(want), head(want))
					}
				}
			}
		}
	}
}

func head(ids []int64) []int64 { return ids[:min(len(ids), 10)] }

// A page whose filter is checked group by group says when it stopped at the
// limit without finding a full page, rather than looking complete.
func TestGroupPagePartial(t *testing.T) {
	st, _ := groupFixture(t)
	if _, err := st.RebuildGroups(context.Background(), GroupOptions{Gap: 3, Hamming: 6}); err != nil {
		t.Fatal(err)
	}
	defer func(n int) { maxGroupWalk = n }(maxGroupWalk)
	maxGroupWalk = 5
	_, info, err := st.GroupPage(GroupBurst, Filter{Limit: 10, MinOverall: 1000}, 2) // matches nothing
	if err != nil || !info.Partial || info.More {
		t.Fatalf("stopped at the limit: %+v %v", info, err)
	}
	maxGroupWalk = 1 << 30
	if _, info, _ = st.GroupPage(GroupBurst, Filter{Limit: 10, MinOverall: 1000}, 2); info.Partial {
		t.Fatal("partial though every group was checked")
	}
}

// Rewriting scores in place (a new aesthetic blend) leaves seq alone, but
// the groups rank by those scores, so they must count as stale.
func TestScoreChangesMakeGroupsStale(t *testing.T) {
	st, ps := groupFixture(t)
	o := GroupOptions{Gap: 3, Hamming: 6}
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := st.SetScores(map[int64][2]float64{ps[0].ID: {5, 99}}); err != nil {
		t.Fatal(err)
	}
	if !st.GroupsStale(o) {
		t.Fatal("SetScores left the groups looking current")
	}
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var seq int64
	st.DB.QueryRow(`SELECT seq FROM photos WHERE id = ?`, ps[1].ID).Scan(&seq)
	if err := st.SetBlendedScores([]BlendUpdate{{ID: ps[1].ID, Seq: seq, Aesthetic: sql.NullFloat64{Float64: 5, Valid: true}, Overall: 98}}); err != nil {
		t.Fatal(err)
	}
	if !st.GroupsStale(o) {
		t.Fatal("SetBlendedScores left the groups looking current")
	}
}

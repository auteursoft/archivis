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
	"strings"
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
	return referenceScenes(ps, GroupOptions{Gap: gap, Hamming: hamming})
}

// referenceScenes groups the photos the simple way, with scenes: each
// camera's dated shots in time order are cut into scenes at gaps over the
// scene gap, scenes into bursts at gaps over the burst gap, and a scene's
// bursts packed in order into chunks of at most maxStackRun shots, each
// stacked on its own (a burst over maxStackRun alone, unstacked, ending the
// chunk before it).
func referenceScenes(ps []Photo, o GroupOptions) groupState {
	gap, hamming, sceneGap := o.Gap, o.Hamming, max(o.Scene, o.Gap)
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
		if r := open[cam]; len(r) > 0 && p.TakenAt-r[len(r)-1].TakenAt > sceneGap {
			runs = append(runs, r)
			open[cam] = nil
		}
		open[cam] = append(open[cam], p)
	}
	for _, r := range open {
		runs = append(runs, r)
	}
	stack := func(shots []Photo) {
		var gs []*gphoto
		for _, p := range shots {
			gs = append(gs, &gphoto{id: p.ID, phash: p.PHash, overall: p.Overall})
		}
		for id, w := range stackFrames(gs, hamming) {
			x := out.photos[id]
			x[1], x[2], x[3] = w[0], w[1], w[2]
			out.photos[id] = x
		}
	}
	for _, r := range runs {
		var chunk []Photo
		for i := 0; i < len(r); {
			j := i + 1
			for j < len(r) && r[j].TakenAt-r[j-1].TakenAt <= gap {
				j++
			}
			b := r[i:j]
			i = j
			if len(b) >= 2 {
				var ids []int64
				for _, p := range b {
					ids = append(ids, p.ID)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				out.groups[[2]int64{GroupBurst, ids[0]}] = ids
				for _, id := range ids {
					x := out.photos[id]
					x[0] = ids[0]
					out.photos[id] = x
				}
			}
			if len(b) > maxStackRun { // alone, unstacked, ending the chunk
				stack(chunk)
				chunk = nil
				continue
			}
			if len(chunk)+len(b) > maxStackRun {
				stack(chunk)
				chunk = nil
			}
			chunk = append(chunk, b...)
		}
		stack(chunk)
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

	for _, o := range []GroupOptions{{Gap: 1, Hamming: 2}, {Gap: 20, Hamming: 10}, {Gap: 3, Hamming: 10, Scene: 600},
		{Gap: 3, Hamming: 6, Scene: 60}, {Gap: 3, Hamming: 6}} {
		if !st.GroupsStale(o) {
			t.Fatalf("%+v: not stale", o)
		}
		if _, err := st.RebuildGroups(ctx, o); err != nil {
			t.Fatal(err)
		}
		compareGroups(t, storedGroups(t, st), referenceScenes(ps, o))
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
						gs, more, err := st.GroupPage(kind, f, minSize, false)
						if err != nil {
							t.Fatalf("%s: %v", name, err)
						}
						for _, g := range gs {
							got = append(got, g.ID)
							var ids []int64
							for _, m := range g.Members {
								ids = append(ids, m.ID)
							}
							if !reflect.DeepEqual(ids, wantMembers[g.ID]) || g.Matching != len(ids) {
								t.Fatalf("%s: group %d members %v (matching %d), want %v", name, g.ID, ids, g.Matching, wantMembers[g.ID])
							}
						}
						// a summary page: the same groups, each with only its best
						// matching member and the count
						sum, _, err := st.GroupPage(kind, f, minSize, true)
						if err != nil || len(sum) != len(gs) {
							t.Fatalf("%s: summary %d groups, %v", name, len(sum), err)
						}
						for i, g := range sum {
							w := wantMembers[g.ID]
							if g.ID != gs[i].ID || len(g.Members) != 1 || g.Members[0].ID != w[0] || g.Matching != len(w) {
								t.Fatalf("%s: summary of group %d: %d members, first %d, matching %d; want %d of %d", name, g.ID,
									len(g.Members), g.Members[0].ID, g.Matching, w[0], len(w))
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
	_, info, err := st.GroupPage(GroupBurst, Filter{Limit: 10, MinOverall: 1000}, 2, false) // matches nothing
	if err != nil || !info.Partial || info.More {
		t.Fatalf("stopped at the limit: %+v %v", info, err)
	}
	maxGroupWalk = 1 << 30
	if _, info, _ = st.GroupPage(GroupBurst, Filter{Limit: 10, MinOverall: 1000}, 2, false); info.Partial {
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

// Sorts break ties as the Photos page does: equal focus by sharpness, equal
// face counts by overall score, before falling back to the group id.
func TestGroupSortTieBreaks(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	add := func(h uint64, focus, sharp, overall float64, faces int) {
		p := fmt.Sprintf("/t/%d", len(rs))
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, PHash: h,
			FocusScore: focus, Sharpness: sharp, Overall: overall, FaceCount: faces, Version: SchemaVersion}})
	}
	// set A (lower ids, so the id fallback alone would put it last): focus
	// .9 at sharpness 20, 2 faces at overall 60
	add(1, .9, 20, 60, 2)
	add(1, .4, 1, 10, 0)
	// set B: focus .9 at sharpness 10, 2 faces at overall 40; its best
	// overall (90) is on a member without faces, which must not count
	add(2, .9, 10, 40, 2)
	add(2, .5, 99, 90, 0)
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RebuildGroups(context.Background(), GroupOptions{Gap: 3, Hamming: 6}); err != nil {
		t.Fatal(err)
	}
	for _, sort := range []string{"sharpness", "faces"} {
		gs, _, err := st.GroupPage(GroupDuplicate, Filter{Sort: sort, Limit: 10}, 2, false)
		if err != nil || len(gs) != 2 {
			t.Fatalf("%s: %d groups, %v", sort, len(gs), err)
		}
		if gs[0].ID != rs[0].Photo.ID {
			t.Errorf("%s: set %d first, want the set whose tie-break is higher (%d)", sort, gs[0].ID, rs[0].Photo.ID)
		}
	}
}

// Group columns left on photos by an interrupted rebuild (their group row
// already replaced or gone) are cleared by the next one.
func TestRebuildClearsLeftovers(t *testing.T) {
	st, ps := groupFixture(t)
	o := GroupOptions{Gap: 3, Hamming: 6}
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var lone int64 // a photo in no group
	if err := st.DB.QueryRow(`SELECT id FROM photos WHERE burst_id = 0 AND dup_id = 0 LIMIT 1`).Scan(&lone); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE photos SET burst_id = 999999, stack_top = 999999, stack_size = 2, stacked = 1, dup_id = 999999 WHERE id = ?`, lone); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	compareGroups(t, storedGroups(t, st), referenceGroups(ps, o.Gap, o.Hamming))
}

// A run of more than maxStackRun frames (a camera's default clock) is a
// burst, but is not stacked: stacking compares frames pairwise.
func TestHugeRunIsNotStacked(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for i := 0; i < maxStackRun+500; i++ {
		p := fmt.Sprintf("/h/%d", i)
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: "CAM",
			TakenAt: 946684800, PHash: 42, Overall: float64(i % 50), Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	stats, err := st.RebuildGroups(context.Background(), GroupOptions{Gap: 3, Hamming: 6})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Bursts != 1 || stats.Stacks != 0 {
		t.Fatalf("%s", stats)
	}
}

// Every sort pages straight off an index: no sorting of all groups.
func TestGroupSortsUseIndexes(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, so := range []string{"", "worst", "newest", "oldest", "technical", "aesthetic", "sharpness", "colorful", "faces"} {
		rows, err := st.DB.Query(`EXPLAIN QUERY PLAN SELECT id FROM photo_groups WHERE kind = ? AND size >= ? ORDER BY `+
			groupSort(so)+`, id DESC LIMIT 30 OFFSET 0`, GroupBurst, 3)
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
		if p := strings.Join(plan, "; "); strings.Contains(p, "TEMP B-TREE") || !strings.Contains(p, "INDEX photo_groups_") {
			t.Errorf("sort %q: %s", so, p)
		}
	}
}

// Deleting any photo marks the groups stale, even one in no group yet: a
// rebuild under way may already have read it.
func TestDeleteUngroupedPhotoMakesGroupsStale(t *testing.T) {
	st, _ := groupFixture(t)
	o := GroupOptions{Gap: 3, Hamming: 6}
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var lone int64
	if err := st.DB.QueryRow(`SELECT id FROM photos WHERE burst_id = 0 AND dup_id = 0 LIMIT 1`).Scan(&lone); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`DELETE FROM photos WHERE id = ?`, lone); err != nil {
		t.Fatal(err)
	}
	if !st.GroupsStale(o) {
		t.Fatal("deleting an ungrouped photo left the groups looking current")
	}
}

// Oldest first goes by a group's earliest dated member: an undated copy
// does not send its set to the end.
func TestOldestIgnoresUndatedMembers(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	add := func(h uint64, at int64) {
		p := fmt.Sprintf("/o/%d", len(rs))
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, PHash: h, TakenAt: at, Version: SchemaVersion}})
	}
	add(1, YearStart(2025)) // dated 2025, 2025
	add(1, YearStart(2025)+100)
	add(2, 0) // undated export of a 2020 photo
	add(2, YearStart(2020))
	add(3, 0) // never dated: last
	add(3, 0)
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RebuildGroups(context.Background(), GroupOptions{Gap: 3, Hamming: 6}); err != nil {
		t.Fatal(err)
	}
	gs, _, err := st.GroupPage(GroupDuplicate, Filter{Sort: "oldest", Limit: 10}, 2, false)
	if err != nil || len(gs) != 3 {
		t.Fatalf("%d groups, %v", len(gs), err)
	}
	if got := []int64{gs[0].ID, gs[1].ID, gs[2].ID}; got[0] != rs[2].Photo.ID || got[1] != rs[0].Photo.ID || got[2] != rs[4].Photo.ID {
		t.Fatalf("oldest first: %v, want the 2020 set, then 2025, then undated", got)
	}
}

// The rebuild's writer holds no transaction between its writes: whatever
// the rebuild computes in between, another writer gets the lock at once.
// Queued writes land on commit; rollback drops them.
func TestBatchWriterHoldsNoLockBetweenWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rs := []*Result{{Photo: Photo{Path: "/w", Size: 1, MTime: 1, Fingerprint: "/w", Version: SchemaVersion}}}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	// another writer that gives up at once if the lock is held
	other, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=0&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tryWrite := func(when string) {
		t.Helper()
		if _, err := other.Exec(`UPDATE photos SET path = path WHERE id = ?`, rs[0].Photo.ID); err != nil {
			t.Fatalf("%s: another writer is blocked: %v", when, err)
		}
	}
	w := &batchWriter{s: st}
	for i := 0; i < writeBatch+10; i++ { // one flush happens along the way
		if err := w.exec(`UPDATE photos SET burst_id = ? WHERE id = ?`, i+1, rs[0].Photo.ID); err != nil {
			t.Fatal(err)
		}
		if i%97 == 0 {
			tryWrite(fmt.Sprintf("after write %d", i))
		}
	}
	tryWrite("with writes queued")
	if err := w.commit(); err != nil {
		t.Fatal(err)
	}
	var got int64
	st.DB.QueryRow(`SELECT burst_id FROM photos WHERE id = ?`, rs[0].Photo.ID).Scan(&got)
	if got != int64(writeBatch+10) {
		t.Fatalf("after commit burst_id = %d, want %d", got, writeBatch+10)
	}
	w.exec(`UPDATE photos SET burst_id = 1 WHERE id = ?`, rs[0].Photo.ID)
	w.rollback()
	if err := w.commit(); err != nil {
		t.Fatal(err)
	}
	st.DB.QueryRow(`SELECT burst_id FROM photos WHERE id = ?`, rs[0].Photo.ID).Scan(&got)
	if got != int64(writeBatch+10) {
		t.Fatal("a rolled-back write landed")
	}
}

// Cleanup stops when the rebuild is cancelled, and the groups stay stale.
func TestResetOldStopsWhenCancelled(t *testing.T) {
	st, _ := groupFixture(t)
	o := GroupOptions{Gap: 3, Hamming: 6}
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// new rules leave memberships to clear; cancel before the cleanup
	ctx, cancel := context.WithCancel(context.Background())
	r := &rebuild{s: st, ctx: ctx, o: GroupOptions{Gap: 1, Hamming: 0}, w: &batchWriter{s: st}}
	cancel()
	if err := r.resetOld(); err != context.Canceled {
		t.Fatalf("resetOld after cancel: %v", err)
	}
	if r.st.PhotoWrites != 0 {
		t.Fatalf("%d writes queued after cancel", r.st.PhotoWrites)
	}
	if _, err := st.RebuildGroups(ctx, GroupOptions{Gap: 1, Hamming: 0}); err == nil {
		t.Fatal("a cancelled rebuild succeeded")
	}
	if !st.GroupsStale(GroupOptions{Gap: 1, Hamming: 0}) {
		t.Fatal("a cancelled rebuild left the groups looking current")
	}
}

// However many photos share one second (a camera's default clock), the
// scan reads at most scanPage rows at a time, and a run too long to stack
// is read in chunks: the groups still match the reference exactly.
func TestFloodsAreReadInPages(t *testing.T) {
	st, ps := groupFixture(t) // 60 photos at one second, among others
	defer func(n, m int) { scanPage, maxStackRun = n, m }(scanPage, maxStackRun)
	scanPage, maxStackRun = 7, 5
	most := 0
	scanHook = func(n int) { most = max(most, n) }
	defer func() { scanHook = nil }()
	o := GroupOptions{Gap: 3, Hamming: 6}
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if most > scanPage {
		t.Fatalf("a scan read %d rows, more than a page of %d", most, scanPage)
	}
	compareGroups(t, storedGroups(t, st), referenceGroups(ps, o.Gap, o.Hamming))
	if again, _ := st.RebuildGroups(context.Background(), o); again.PhotoWrites != 0 || again.GroupWrites != 0 {
		t.Fatalf("second rebuild wrote: %s", again)
	}
}

// A rebuild that stops partway, after some of its writes have landed,
// leaves the groups stale (even if they were current before), so the next
// regroup repairs them.
func TestInterruptedRebuildLeavesGroupsStale(t *testing.T) {
	st, ps := groupFixture(t)
	o := GroupOptions{Gap: 3, Hamming: 6}
	defer func(n, b int) { scanPage, writeBatch = n, b }(scanPage, writeBatch)
	scanPage, writeBatch = 11, 3 // flush often
	total := 0                   // burst-scan reads in a whole run
	scanHook = func(int) { total++ }
	defer func() { scanHook = nil }()
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// cancel on the scan's last read: the bursts' writes then land, and the
	// next phase stops
	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	scanHook = func(int) {
		if reads++; reads == total {
			cancel()
		}
	}
	// new rules, so the run has writes to make before it is cancelled
	stats, err := st.RebuildGroups(ctx, GroupOptions{Gap: 20, Hamming: 10})
	if err == nil {
		t.Fatal("the cancelled rebuild succeeded")
	}
	if stats.PhotoWrites == 0 {
		t.Fatal("cancelled before any write: the test proves nothing")
	}
	for _, rules := range []GroupOptions{o, {Gap: 20, Hamming: 10}} {
		if !st.GroupsStale(rules) {
			t.Fatalf("groups look current under %+v after an interrupted rebuild", rules)
		}
	}
	scanHook = nil
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	compareGroups(t, storedGroups(t, st), referenceGroups(ps, o.Gap, o.Hamming))
}

// Shots of one scene minutes apart stack into one card, its best shot on
// top, the RAW and JPEG of each shot with it; a shot of something else in
// the scene stays apart, as does the same view after a longer pause, or
// from another camera. Without a scene gap only bursts stack, as before.
func TestSceneStacks(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const sunset, dock = uint64(0xF0F0_3C3C_AA55_1234), uint64(0x0F0F_C3C3_55AA_EDCB)
	var rs []*Result
	add := func(name, cam string, at int64, hash uint64, overall float64) {
		rs = append(rs, &Result{Photo: Photo{Path: "/s/" + name, Size: 1, MTime: 1, Fingerprint: name, CameraModel: cam,
			TakenAt: at, PHash: hash, Overall: overall, Version: SchemaVersion}})
	}
	t0 := int64(1_700_000_000)
	for i := 0; i < 6; i++ { // a shot every 2 minutes, RAW and JPEG alike but for a few bits
		at := t0 + int64(i)*120
		h := sunset ^ uint64(1)<<(i*7) ^ uint64(1)<<(i*7+3) // the light changes a little
		add(fmt.Sprintf("DSC_%d.NEF", i), "Z 8", at, h, float64(10+i))
		add(fmt.Sprintf("DSC_%d.JPG", i), "Z 8", at, h^1<<60, float64(10+i)-0.5)
	}
	add("dock.NEF", "Z 8", t0+300, dock, 30)                 // something else, mid-scene
	add("later.NEF", "Z 8", t0+5*120+11*60, sunset, 5)       // the same view after an 11-minute pause
	add("phone.HEIC", "iPhone 15 Pro", t0+60, sunset, 40)    // another camera
	add("phone2.HEIC", "iPhone 15 Pro", t0+61, sunset^3, 39) // and its burst
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	id := map[string]int64{}
	for _, r := range rs {
		id[strings.TrimPrefix(r.Photo.Path, "/s/")] = r.Photo.ID
	}
	ctx := context.Background()
	if _, err := st.RebuildGroups(ctx, GroupOptions{Gap: 3, Hamming: 10, Scene: 600}); err != nil {
		t.Fatal(err)
	}
	got := storedGroups(t, st)
	top := id["DSC_5.NEF"] // the best of the scene
	for i := 0; i < 6; i++ {
		for _, ext := range []string{"NEF", "JPG"} {
			name := fmt.Sprintf("DSC_%d.%s", i, ext)
			x := got.photos[id[name]]
			hidden := int64(1)
			if id[name] == top {
				hidden = 0
			}
			if x[1] != top || x[2] != 12 || x[3] != hidden {
				t.Fatalf("%s: stack %v, want top %d of 12", name, x, top)
			}
			if want := min(id[fmt.Sprintf("DSC_%d.NEF", i)], id[fmt.Sprintf("DSC_%d.JPG", i)]); x[0] != want {
				t.Fatalf("%s: burst %d, want its RAW+JPEG pair's %d", name, x[0], want)
			}
		}
	}
	for _, name := range []string{"dock.NEF", "later.NEF"} {
		if x := got.photos[id[name]]; x[0] != 0 || x[1] != 0 { // in no burst or stack (later.NEF is a copy of phone.HEIC)
			t.Fatalf("%s grouped: %v", name, x)
		}
	}
	if x := got.photos[id["phone2.HEIC"]]; x[1] != id["phone.HEIC"] || x[2] != 2 {
		t.Fatalf("the phone's burst: %v", x)
	}
	compareGroups(t, got, referenceScenes(photosOf(rs), GroupOptions{Gap: 3, Hamming: 10, Scene: 600}))

	// without scenes, only each RAW+JPEG pair stacks; the scene's lone top
	// (in no burst) is cleared
	if _, err := st.RebuildGroups(ctx, GroupOptions{Gap: 3, Hamming: 10}); err != nil {
		t.Fatal(err)
	}
	got = storedGroups(t, st)
	if x := got.photos[top]; x[1] != top || x[2] != 2 {
		t.Fatalf("without scenes the best shot's stack is %v, want its pair", x)
	}
	compareGroups(t, got, referenceScenes(photosOf(rs), GroupOptions{Gap: 3, Hamming: 10}))
}

func photosOf(rs []*Result) []Photo {
	var ps []Photo
	for _, r := range rs {
		ps = append(ps, r.Photo)
	}
	return ps
}

// A scene longer than maxStackRun is stacked in chunks cut between bursts,
// never through one; a burst over maxStackRun is not stacked.
func TestLongSceneChunks(t *testing.T) {
	defer func(n int) { maxStackRun = n }(maxStackRun)
	maxStackRun = 5
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	at := int64(1_700_000_000)
	for i, n := range []int{2, 3, 2, 1, 7, 2, 2, 1} { // bursts, a lone shot, a burst too big to stack
		for k := 0; k < n; k++ {
			p := fmt.Sprintf("/l/%d-%d", i, k)
			rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: "Z 9",
				TakenAt: at, PHash: 0xABCD, Overall: float64(len(rs) % 7), Version: SchemaVersion}})
			at++
		}
		at += 60
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	o := GroupOptions{Gap: 3, Hamming: 4, Scene: 600}
	if _, err := st.RebuildGroups(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	got := storedGroups(t, st)
	compareGroups(t, got, referenceScenes(photosOf(rs), o))
	for _, r := range rs {
		if x := got.photos[r.Photo.ID]; x[2] > int64(maxStackRun) {
			t.Fatalf("%s: a stack of %d, over the chunk size", r.Photo.Path, x[2])
		}
		if strings.HasPrefix(r.Photo.Path, "/l/4-") && got.photos[r.Photo.ID][2] != 0 {
			t.Fatalf("%s: a burst over maxStackRun was stacked", r.Photo.Path)
		}
	}
	// chunks are consecutive: none reaches across the oversized burst (4),
	// so the shots before it (bursts 0-3) and after it (5-7) stack apart
	burstOf := map[int64]int{}
	for _, r := range rs {
		var b, k int
		fmt.Sscanf(r.Photo.Path, "/l/%d-%d", &b, &k)
		burstOf[r.Photo.ID] = b
	}
	for _, r := range rs {
		x := got.photos[r.Photo.ID]
		if x[1] != 0 && (burstOf[r.Photo.ID] < 4) != (burstOf[x[1]] < 4) {
			t.Fatalf("%s is stacked under a shot across the oversized burst", r.Photo.Path)
		}
	}
	// and the chunks are as packed: [0 1] [2 3] | [5 6 7]
	for _, r := range rs {
		x := got.photos[r.Photo.ID]
		chunk := map[int]int{0: 0, 1: 0, 2: 1, 3: 1, 5: 2, 6: 2, 7: 2}[burstOf[r.Photo.ID]]
		if x[1] != 0 && map[int]int{0: 0, 1: 0, 2: 1, 3: 1, 5: 2, 6: 2, 7: 2}[burstOf[x[1]]] != chunk {
			t.Fatalf("%s: stacked outside its chunk", r.Photo.Path)
		}
	}
}

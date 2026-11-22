package store

import (
	"context"
	"fmt"

	"math/bits"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/vindex"
)

// subjectCLIP makes an embedding near a subject's: base plus noise of the
// given size, scaled to unit length.
func subjectCLIP(rng *rand.Rand, base []float32, noise float64) []float32 {
	v := make([]float32, len(base))
	for i := range base {
		v[i] = base[i] + float32(rng.NormFloat64()*noise)
	}
	return unitVector(v)
}

func randomCLIP(rng *rand.Rand) []float32 {
	v := make([]float32, 512)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	return unitVector(v)
}

func cosine(a, b []float32) float64 {
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d
}

// Visually similar shots stack though their perceptual hashes are far
// apart; shots less similar than the setting, or with similarity off, need
// close hashes; a shot without an embedding falls back to its hash.
func TestStackFramesBySimilarity(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	base := randomCLIP(rng)
	near := subjectCLIP(rng, base, 0.01) // the same subject, reframed
	far := subjectCLIP(rng, base, 0.03)  // the same place, something else
	near2 := subjectCLIP(rng, base, 0.01)
	if c := cosine(base, near); c < 0.95 {
		t.Fatalf("near: cosine %.3f", c)
	}
	if c := cosine(base, far); c < 0.5 || c > 0.88 {
		t.Fatalf("far: cosine %.3f", c)
	}
	f := func(id int64, h uint64, overall float64, clip []float32) *gphoto {
		return &gphoto{id: id, phash: h, overall: overall, clip: clip}
	}
	frames := func() []*gphoto {
		return []*gphoto{
			f(1, 0x0000_0000_0000_FFFF, 9, base),
			f(2, 0xFFFF_0000_0000_0000, 8, near), // 32 bits from 1, but alike to see
			f(3, 0x0000_FFFF_0000_0000, 7, far),  // 32 bits from 1 and not alike enough
			f(4, 0x0000_0000_0000_FFFE, 6, nil),  // no embedding: 1 bit from 1
			f(5, 0, 5, near2),                    // no hash: alike to see
		}
	}
	got := stackFrames(frames(), GroupOptions{Hamming: 10, Similar: 93})
	want := map[int64][3]int64{1: {1, 4, 0}, 2: {1, 4, 1}, 4: {1, 4, 1}, 5: {1, 4, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("at 93%%: got %v, want %v", got, want)
	}
	// stricter than the reframed shot: hashes alone
	got = stackFrames(frames(), GroupOptions{Hamming: 10, Similar: 99})
	want = map[int64][3]int64{1: {1, 2, 0}, 4: {1, 2, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("at 99%%: got %v, want %v", got, want)
	}
	// off: hashes alone
	got = stackFrames(frames(), GroupOptions{Hamming: 10})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("off: got %v, want %v", got, want)
	}
}

// The album in the report: three shots of a painting and three of a crane,
// reframed between shots so their hashes differ, minutes apart. Each three
// stack, and each is a set of duplicates; an identical copy of one of them
// elsewhere joins its set, a like shot in another scene does not.
func TestSimilarShotsAreDuplicates(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rng := rand.New(rand.NewSource(11))
	painting, crane := randomCLIP(rng), randomCLIP(rng)
	var rs []*Result
	add := func(name, cam string, at int64, hash uint64, overall float64, clip []float32) {
		p := Photo{Path: "/a/" + name, Size: 1, MTime: 1, Fingerprint: name, CameraModel: cam,
			TakenAt: at, PHash: hash, Overall: overall, Version: SchemaVersion}
		if clip != nil {
			p.CLIP = vindex.EncodeF16(clip)
		}
		rs = append(rs, &Result{Photo: p})
	}
	t0 := int64(1_763_000_000)
	for i := 0; i < 3; i++ {
		add(fmt.Sprintf("painting%d.NEF", i), "Z 8", t0+int64(i)*20, rng.Uint64(), float64(10+i), subjectCLIP(rng, painting, 0.01))
		add(fmt.Sprintf("crane%d.NEF", i), "Z 8", t0+200+int64(i)*25, rng.Uint64(), float64(20-i), subjectCLIP(rng, crane, 0.01))
	}
	add("copy.JPG", "", 0, 0, 1, nil)                                                                // an identical copy of painting0, below
	add("painting-next-day.NEF", "Z 8", t0+86400, rng.Uint64(), 3, subjectCLIP(rng, painting, 0.01)) // another scene
	rs[6].Photo.PHash = rs[0].Photo.PHash
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	id := map[string]int64{}
	for _, r := range rs {
		id[strings.TrimPrefix(r.Photo.Path, "/a/")] = r.Photo.ID
	}
	ids := func(names ...string) []int64 {
		var out []int64
		for _, n := range names {
			out = append(out, id[n])
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	ctx := context.Background()
	o := GroupOptions{Gap: 3, Hamming: 10, Scene: 600, Similar: 93}
	stats, err := st.RebuildGroups(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Stacks != 2 || stats.Duplicates != 2 {
		t.Fatalf("stats %+v, want 2 stacks, 2 duplicate sets", stats)
	}
	got := storedGroups(t, st)
	paintings := ids("painting0.NEF", "painting1.NEF", "painting2.NEF")
	cranes := ids("crane0.NEF", "crane1.NEF", "crane2.NEF")
	wantDups := map[[2]int64][]int64{
		{GroupDuplicate, paintings[0]}: append(append([]int64{}, paintings...), id["copy.JPG"]),
		{GroupDuplicate, cranes[0]}:    cranes,
	}
	for k, v := range got.groups {
		if k[0] != GroupDuplicate {
			delete(got.groups, k)
		} else {
			sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		}
	}
	if !reflect.DeepEqual(got.groups, wantDups) {
		t.Fatalf("duplicates %v, want %v", got.groups, wantDups)
	}
	for _, set := range [][]int64{paintings, cranes} {
		top := got.photos[set[0]][1]
		for _, p := range set {
			if x := got.photos[p]; x[1] != top || x[2] != 3 || x[4] != set[0] {
				t.Fatalf("photo %d: %v, want stacked on %d (3) in duplicates %d", p, x, top, set[0])
			}
		}
	}
	if x := got.photos[id["painting-next-day.NEF"]]; x != [5]int64{} {
		t.Fatalf("a like shot in another scene was grouped: %v", x)
	}
	// the Duplicates page lists both
	gs, _, err := st.GroupPage(GroupDuplicate, Filter{Limit: 10}, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 {
		t.Fatalf("duplicates page: %d sets", len(gs))
	}

	// a new similarity regroups; off, only the identical copy is left
	if st.GroupsStale(o) {
		t.Fatal("stale after rebuilding")
	}
	off := o
	off.Similar = 0
	if !st.GroupRulesChanged(off) {
		t.Fatal("changing the similarity does not regroup")
	}
	if _, err := st.RebuildGroups(ctx, off); err != nil {
		t.Fatal(err)
	}
	got = storedGroups(t, st)
	for k := range got.groups {
		if k[0] != GroupDuplicate {
			delete(got.groups, k)
		}
	}
	if want := map[[2]int64][]int64{{GroupDuplicate, id["painting0.NEF"]}: ids("painting0.NEF", "copy.JPG")}; !reflect.DeepEqual(got.groups, want) {
		t.Fatalf("similarity off: duplicates %v, want %v", got.groups, want)
	}
	if x := got.photos[id["painting1.NEF"]]; x != [5]int64{} {
		t.Fatalf("similarity off: painting1 still grouped: %v", x)
	}
}

// With embeddings, the rebuild matches the simple in-memory grouping:
// stacks by similarity or hash, duplicates joining stacks and identical
// copies.
func TestSimilarGroupsMatchReference(t *testing.T) {
	st, ps := groupFixture(t)
	rng := rand.New(rand.NewSource(5))
	subjects := make([][]float32, 40)
	for i := range subjects {
		subjects[i] = randomCLIP(rng)
	}
	clips := map[int64][]float32{}
	for _, p := range ps {
		if rng.Intn(5) == 0 {
			continue // not yet embedded
		}
		// noise from none to well past the threshold
		v := subjectCLIP(rng, subjects[int(p.PHash%uint64(len(subjects)))], []float64{0.005, 0.01, 0.02, 0.05}[rng.Intn(4)])
		b := vindex.EncodeF16(v)
		if _, err := st.DB.Exec(`INSERT INTO photo_clip (photo_id, clip) VALUES (?, ?)`, p.ID, b); err != nil {
			t.Fatal(err)
		}
		clips[p.ID] = unitVector(vindex.DecodeF16(b)) // as the rebuild reads it
	}
	for _, o := range []GroupOptions{{Gap: 3, Hamming: 6, Scene: 600, Similar: 93}, {Gap: 3, Hamming: 2, Scene: 120, Similar: 85}} {
		if _, err := st.RebuildGroups(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		got, want := storedGroups(t, st), referenceSimilar(ps, o, clips)
		if !reflect.DeepEqual(got.photos, want.photos) {
			n := 0
			for id, w := range want.photos {
				if g := got.photos[id]; g != w && n < 5 {
					t.Errorf("%+v: photo %d: got %v, want %v", o, id, g, w)
					n++
				}
			}
			t.FailNow()
		}
		if !reflect.DeepEqual(got.groups, want.groups) {
			t.Fatalf("%+v: groups differ (%d vs %d)", o, len(got.groups), len(want.groups))
		}
	}
}

// alike accepts a pair when either rule does: CLIP cosine at least the
// setting (with both embedded and the setting on), or both hashes set and
// within the Hamming limit. Checking the cheap rule first changes no answer.
func TestAlikeEitherRule(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	base := randomCLIP(rng)
	h0 := rng.Uint64() | 1
	pick := func() *gphoto {
		p := &gphoto{}
		switch rng.Intn(3) {
		case 0:
			p.phash = h0 ^ uint64(1)<<rng.Intn(64) ^ uint64(1)<<rng.Intn(64) // near
		case 1:
			p.phash = rng.Uint64() // far
		}
		switch rng.Intn(3) {
		case 0:
			p.clip = subjectCLIP(rng, base, []float64{0.005, 0.01, 0.02}[rng.Intn(3)]) // the same subject, more or less
		case 1:
			p.clip = randomCLIP(rng)
		}
		return p
	}
	seen := map[[2]bool]int{}
	for i := 0; i < 4000; i++ {
		a, b := pick(), pick()
		for _, o := range []GroupOptions{{Hamming: 0}, {Hamming: 10}, {Hamming: 10, Similar: 85}, {Hamming: 0, Similar: 93}, {Hamming: 10, Similar: 93}} {
			byHash := a.phash != 0 && b.phash != 0 && bits.OnesCount64(a.phash^b.phash) <= o.Hamming
			byLook := o.Similar > 0 && a.clip != nil && b.clip != nil && cosine(a.clip, b.clip) >= float64(o.Similar)/100
			if got := alike(a, b, o); got != (byHash || byLook) {
				t.Fatalf("%+v: alike %v, want hash %v or look %v (cosine %.4f)", o, got, byHash, byLook, cosine(a.clip, b.clip))
			}
			seen[[2]bool{byHash, byLook}]++
		}
	}
	for _, k := range [][2]bool{{true, true}, {true, false}, {false, true}, {false, false}} {
		if seen[k] == 0 {
			t.Fatalf("no pair where hash %v and look %v: %v", k[0], k[1], seen)
		}
	}
}

// A chunk of identical copies, each embedded: stacking them needs no
// embedding comparisons.
func BenchmarkStackFramesCopies(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	base := randomCLIP(rng)
	var frames []*gphoto
	for i := 0; i < 1000; i++ {
		frames = append(frames, &gphoto{id: int64(i + 1), phash: 0xC0FFEE, overall: rng.Float64(), clip: subjectCLIP(rng, base, 0.01)})
	}
	o := GroupOptions{Hamming: 10, Similar: 93}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stackFrames(frames, o)
	}
}

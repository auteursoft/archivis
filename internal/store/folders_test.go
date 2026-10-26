package store

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func folderStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func savePaths(t *testing.T, st *Store, overall map[string]float64) {
	t.Helper()
	var rs []*Result
	for p, o := range overall {
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: o, Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
}

// referenceFolders works the folders out the plain way: every photo
// counts in each folder above it.
func referenceFolders(t *testing.T, st *Store) map[string]Folder {
	t.Helper()
	rows, err := st.DB.Query(`SELECT id, path, overall FROM photos`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]Folder{}
	for rows.Next() {
		var id int64
		var p string
		var o float64
		if err := rows.Scan(&id, &p, &o); err != nil {
			t.Fatal(err)
		}
		for d := ParentDir(p); d != ""; d = ParentDir(d) {
			f, ok := out[d]
			if !ok {
				f = Folder{Path: d, Parent: ParentDir(d), CoverOverall: math.Inf(-1)}
			}
			f.Total++
			if o > f.CoverOverall || (o == f.CoverOverall && id < f.CoverID) {
				f.CoverID, f.CoverOverall = id, o
			}
			out[d] = f
		}
	}
	return out
}

func storedFolders(t *testing.T, st *Store) map[string]Folder {
	t.Helper()
	fs, err := st.folders(``)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Folder{}
	for _, f := range fs {
		out[f.Path] = f
	}
	return out
}

func rebuildFolders(t *testing.T, st *Store) FolderStats {
	t.Helper()
	fst, err := st.RebuildFolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.FoldersStale() {
		t.Fatal("folders stale right after a rebuild")
	}
	if got, want := storedFolders(t, st), referenceFolders(t, st); !reflect.DeepEqual(got, want) {
		for p, w := range want {
			if got[p] != w {
				t.Errorf("folder %q: got %+v, want %+v", p, got[p], w)
			}
		}
		for p := range got {
			if _, ok := want[p]; !ok {
				t.Errorf("folder %q: stored but holds no photos", p)
			}
		}
		t.FailNow()
	}
	return fst
}

// Random trees, with names that sort around the separators ("a b", "a-b",
// "a0", a Unix name with a backslash) and Windows paths, read in small
// pages: every folder's count and cover must match the plain reckoning,
// a second rebuild must write nothing, and changes must be followed.
func TestRebuildFolders(t *testing.T) {
	defer func(n int) { scanPage = n }(scanPage)
	scanPage = 7
	st := folderStore(t)
	rng := rand.New(rand.NewSource(3))
	names := []string{"a", "a b", "a-b", "a0", "a.b", `a\b`, "b", "Ä", "z"}
	ps := map[string]float64{}
	for len(ps) < 400 {
		var b strings.Builder
		win := rng.Intn(5) == 0
		sep := "/"
		if win {
			b.WriteString("C:")
			sep = `\`
		}
		for d := 1 + rng.Intn(4); d > 0; d-- {
			n := names[rng.Intn(len(names))]
			if win && strings.Contains(n, `\`) {
				n = "w"
			}
			b.WriteString(sep + n)
		}
		b.WriteString(fmt.Sprintf("%s%d.jpg", sep, rng.Intn(5)))
		ps[b.String()] = float64(rng.Intn(5)) // ties: the lower id is the cover
	}
	savePaths(t, st, ps)
	if !st.FoldersStale() {
		t.Fatal("never worked out, yet not stale")
	}
	if fst := rebuildFolders(t, st); fst.Folders == 0 || fst.Writes != fst.Folders {
		t.Fatalf("first rebuild: %+v", fst)
	}
	if fst := rebuildFolders(t, st); fst.Writes != 0 {
		t.Fatalf("second rebuild wrote %d rows", fst.Writes)
	}

	// delete some photos (emptying folders), add some, rescore one
	n := 0
	for p := range ps {
		if n++; n > 150 {
			break
		}
		if _, err := st.DB.Exec(`DELETE FROM photos WHERE path = ?`, p); err != nil {
			t.Fatal(err)
		}
	}
	if !st.FoldersStale() {
		t.Fatal("deleting photos must make the folders stale")
	}
	rebuildFolders(t, st)
	savePaths(t, st, map[string]float64{"/new/place/x.jpg": 9, "/a/a/zz.jpg": 99})
	if !st.FoldersStale() {
		t.Fatal("adding photos must make the folders stale")
	}
	rebuildFolders(t, st)
	if f, _ := st.Folder("/a/"); f == nil || f.CoverOverall != 99 {
		t.Fatalf("Folder(/a/) = %+v, want the 99 as cover", f)
	}
}

// Subfolders lists one level down, Folder finds one, and a Unix name with
// a backslash is a name, not two folders.
func TestFolderLookups(t *testing.T) {
	st := folderStore(t)
	savePaths(t, st, map[string]float64{
		"/r/a/1.jpg": 1, "/r/a/b/2.jpg": 2, "/r/c/3.jpg": 3, `/r/d\e/4.jpg`: 4, "/r0/5.jpg": 5,
	})
	rebuildFolders(t, st)
	subs, err := st.Subfolders("/r/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range subs {
		names = append(names, f.Name()+fmt.Sprint(f.Total))
	}
	if want := []string{"a2", "c1", `d\e1`}; !reflect.DeepEqual(names, want) {
		t.Fatalf("Subfolders(/r) = %v, want %v", names, want)
	}
	if f, _ := st.Folder("/r"); f == nil || f.Total != 4 {
		t.Fatalf("Folder(/r) = %+v, want 4 photos (not /r0's)", f)
	}
	if f, _ := st.Folder("/nowhere"); f != nil {
		t.Fatalf("Folder(/nowhere) = %+v", f)
	}
}

// A folder filter takes everything below the folder, and nothing beside
// it: not /a/b2 for /a/b, nor a Unix name holding a backslash.
func TestDirsFilter(t *testing.T) {
	st := folderStore(t)
	all := map[string]float64{
		"/a/b/1.jpg": 1, "/a/b/c/2.jpg": 2, "/a/b2/3.jpg": 3, "/a/b.jpg": 4, "/a/b 1/5.jpg": 5, `/a/b\x.jpg`: 6,
		`C:\p\q\7.jpg`: 7, `C:\p\q/8.jpg`: 8, `C:\p\q2\9.jpg`: 9,
		"/lib/one.photoslibrary/originals/A/1.heic": 10, "/lib/two.photoslibrary/originals/B/2.heic": 11,
	}
	savePaths(t, st, all)
	for _, c := range []struct {
		dirs   []string
		byPath bool
		want   []string
	}{
		{[]string{"/a/b"}, false, []string{"/a/b/1.jpg", "/a/b/c/2.jpg"}},
		{[]string{"/a/b/"}, true, []string{"/a/b/1.jpg", "/a/b/c/2.jpg"}},
		{[]string{`C:\p\q`}, true, []string{`C:\p\q/8.jpg`, `C:\p\q\7.jpg`}},
		{[]string{"/a/b", "/a/b2"}, true, []string{"/a/b/1.jpg", "/a/b/c/2.jpg", "/a/b2/3.jpg"}},
		{[]string{"/"}, true, nil}, // every Unix path: checked by count below
	} {
		f := Filter{Dirs: c.dirs, DirsByPath: c.byPath, Limit: 100}
		ps, total, _, err := st.BrowsePage(f)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range ps {
			got = append(got, p.Path)
		}
		sort.Strings(got)
		if c.want == nil {
			if total != 8 || len(got) != 8 {
				t.Errorf("Dirs %v: %d photos (total %d), want the 8 Unix ones", c.dirs, len(got), total)
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) || total != len(c.want) {
			t.Errorf("Dirs %v: got %v (total %d), want %v", c.dirs, got, total, c.want)
		}
	}
	// SkipCount: no count, total -1
	if _, total, _, _ := st.BrowsePage(Filter{Dirs: []string{"/a"}, SkipCount: true, Limit: 2}); total != -1 {
		t.Errorf("SkipCount: total %d, want -1", total)
	}
	rebuildFolders(t, st)
	libs, err := st.PhotosLibraries()
	if err != nil || len(libs) != 2 {
		t.Fatalf("PhotosLibraries = %v, %v; want both", libs, err)
	}
}

// A folder sorted by score reads the folder through the path index rather
// than walking the whole catalogue in score order.
func TestDirsUsePathIndex(t *testing.T) {
	st := folderStore(t)
	for _, sortBy := range []string{"overall", "newest", "worst", "faces"} {
		f := Filter{Dirs: []string{"/a/b"}, DirsByPath: true, Sort: sortBy, Limit: 10}
		where, args := f.where()
		rows, err := st.DB.Query(`EXPLAIN QUERY PLAN SELECT id FROM photos`+where+f.order(), args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var a, b, c int
			var d string
			if err := rows.Scan(&a, &b, &c, &d); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, d)
		}
		rows.Close()
		if p := strings.Join(plan, "; "); !strings.Contains(p, "(path>? AND path<?)") {
			t.Errorf("sort %s: plan %q doesn't range over the path index", sortBy, p)
		}
	}
	// a big folder walks the sort's index instead: no sorting it whole
	f := Filter{Dirs: []string{"/a/b"}, Sort: "overall", Limit: 10}
	where, args := f.where()
	var plan string
	rows, err := st.DB.Query(`EXPLAIN QUERY PLAN SELECT id FROM photos`+where+f.order(), args...)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, b, c int
		var d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		plan += d + "; "
	}
	rows.Close()
	if strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "path>?") {
		t.Errorf("big folder: plan %q sorts the folder", plan)
	}
}

// Roots are recorded absolute (C:\... on Windows), without a trailing
// separator, once each; removing one takes the same spellings.
func TestRoots(t *testing.T) {
	st := folderStore(t)
	base := t.TempDir()
	xy, a := filepath.Join(base, "x", "y"), filepath.Join(base, "a")
	if err := st.RecordRoots([]string{xy + string(filepath.Separator), xy, a}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.Roots()
	want := []string{a, xy}
	sort.Strings(want)
	if err != nil || !reflect.DeepEqual(rs, want) {
		t.Fatalf("Roots = %v, %v; want %v", rs, err, want)
	}
	if ok, _ := st.RemoveRoot(xy + string(filepath.Separator)); !ok {
		t.Fatal("RemoveRoot with a trailing separator found nothing")
	}
	if ok, _ := st.RemoveRoot(xy); ok {
		t.Fatal("removed twice")
	}
}

// A move (a file recognised at a new path) changes the folders: they must
// read as stale, and the rebuild must follow the photo.
func TestMovedPhotoMakesFoldersStale(t *testing.T) {
	st := folderStore(t)
	savePaths(t, st, map[string]float64{"/old/a.jpg": 1, "/keep/b.jpg": 2})
	rebuildFolders(t, st)
	var id int64
	if err := st.DB.QueryRow(`SELECT id FROM photos WHERE path = '/old/a.jpg'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := st.MovePath(id, "/new/a.jpg", 2); err != nil {
		t.Fatal(err)
	}
	if !st.FoldersStale() {
		t.Fatal("a move left the folders current")
	}
	rebuildFolders(t, st)
	if f, _ := st.Folder("/old"); f != nil {
		t.Fatalf("/old still listed after the move: %+v", f)
	}
	if f, _ := st.Folder("/new"); f == nil || f.Total != 1 {
		t.Fatalf("Folder(/new) = %+v", f)
	}
}

// "/" is a folder like any other: a recorded root of "/" has a count,
// a cover and subfolders.
func TestSlashIsAFolder(t *testing.T) {
	st := folderStore(t)
	savePaths(t, st, map[string]float64{"/pics/a.jpg": 1, "/docs/b.jpg": 5, `C:\w\c.jpg`: 9})
	rebuildFolders(t, st)
	f, _ := st.Folder("/")
	if f == nil || f.Total != 2 || f.CoverOverall != 5 {
		t.Fatalf("Folder(/) = %+v, want the 2 Unix photos, the 5 as cover", f)
	}
	subs, _ := st.Subfolders("/")
	if len(subs) != 2 || subs[0].Path != "/docs" || subs[1].Path != "/pics" {
		t.Fatalf("Subfolders(/) = %+v", subs)
	}
	if ParentDir("/") != "" || ParentDir(`C:`) != "" || ParentDir(`C:\w`) != "C:" || ParentDir("/pics") != "/" {
		t.Fatal("ParentDir tops")
	}
}

// Libraries inside a library count once, even when another name sorts
// between them (a.photoslibrary-old.photoslibrary).
func TestNestedLibrariesCountOnce(t *testing.T) {
	st := folderStore(t)
	savePaths(t, st, map[string]float64{
		"/a.photoslibrary/originals/1.heic":                      1,
		"/a.photoslibrary-old.photoslibrary/originals/2.heic":    2,
		"/a.photoslibrary/nested.photoslibrary/originals/3.heic": 3,
		"/b/Photos Library.PHOTOSLIBRARY/originals/4.heic":       4,
	})
	rebuildFolders(t, st)
	libs, err := st.PhotosLibraries()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	total := 0
	for _, l := range libs {
		got = append(got, l.Path)
		total += l.Total
	}
	want := []string{"/a.photoslibrary", "/a.photoslibrary-old.photoslibrary", "/b/Photos Library.PHOTOSLIBRARY"}
	if !reflect.DeepEqual(got, want) || total != 4 {
		t.Fatalf("PhotosLibraries = %v (%d photos), want %v (4)", got, total, want)
	}
}

// A cancelled rebuild stops and leaves the folders stale.
func TestCancelledFolderRebuildStaysStale(t *testing.T) {
	st := folderStore(t)
	savePaths(t, st, map[string]float64{"/a/1.jpg": 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.RebuildFolders(ctx); err != context.Canceled {
		t.Fatalf("RebuildFolders on a cancelled context: %v", err)
	}
	if !st.FoldersStale() {
		t.Fatal("a cancelled rebuild left the folders current")
	}
}

// Writing a photo's path unchanged (as every re-save does) is no move: it
// leaves the folders' move counter alone; a real move bumps it. A catalogue
// with the earlier trigger (firing on any path write) gets the new one.
func TestUnchangedPathIsNoMove(t *testing.T) {
	st := folderStore(t)
	savePaths(t, st, map[string]float64{"/a/1.jpg": 1})
	moves := func() string { return st.GetMeta("folders_dirty") }
	before := moves()
	if _, err := st.DB.Exec(`UPDATE photos SET path = path, mtime = mtime + 1`); err != nil {
		t.Fatal(err)
	}
	savePaths(t, st, map[string]float64{"/a/1.jpg": 2}) // re-indexed: same path, new score
	if got := moves(); got != before {
		t.Fatalf("unchanged paths counted as moves: folders_dirty %q -> %q", before, got)
	}
	var id int64
	if err := st.DB.QueryRow(`SELECT id FROM photos`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := st.MovePath(id, "/b/1.jpg", 3); err != nil {
		t.Fatal(err)
	}
	if moves() == before {
		t.Fatal("a real move was not counted")
	}
	var old int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'photos_moved'`).Scan(&old); err != nil || old != 0 {
		t.Fatalf("the earlier photos_moved trigger is still there (%d, %v)", old, err)
	}
}

// Memory holds the open folders and a batch of finished ones, not every
// folder: 5,000 one-photo folders (Unix and Windows alike) never have more
// than a batch held at once, and stale ones are still removed.
func TestFolderRebuildHoldsABatch(t *testing.T) {
	st := folderStore(t)
	ps := map[string]float64{}
	for i := 0; i < 2500; i++ {
		ps[fmt.Sprintf("/archive/%05d/a.jpg", i)] = 1
		ps[fmt.Sprintf(`C:\archive\%05d\a.jpg`, i)] = 1
	}
	savePaths(t, st, ps)
	fst := rebuildFolders(t, st)
	if fst.Folders != 5004 || fst.Held > 2100 {
		t.Fatalf("%d folders, %d held at once; want 5004, at most a batch (2000) plus the open chain", fst.Folders, fst.Held)
	}
	if _, err := st.DB.Exec(`DELETE FROM photos WHERE path LIKE '/archive/0000%'`); err != nil {
		t.Fatal(err)
	}
	rebuildFolders(t, st) // compares with the plain reckoning: the emptied folders are gone
	// one path with "/" splits one folder (C:\archive\00001): only it is
	// held to be merged, not every Windows folder, and the counts still match
	savePaths(t, st, map[string]float64{`C:\archive\00001/b.jpg`: 2})
	if fst := rebuildFolders(t, st); fst.Held > 2100 {
		t.Fatalf("one split folder: %d held at once; only it should be held beyond a batch", fst.Held)
	}
	if f, _ := st.Folder(`C:\archive\00001`); f == nil || f.Total != 2 {
		t.Fatalf(`Folder(C:\archive\00001) = %+v, want its 2 photos merged`, f)
	}
}

// Windows paths spelt with "/" beside ones with "\" split a folder in path
// order even when no single path holds both (C:/x/…, C:Z…, C:\y/…): its
// parts must still be added up.
func TestWindowsFolderSplitAcrossPaths(t *testing.T) {
	st := folderStore(t)
	savePaths(t, st, map[string]float64{`C:/x/a.jpg`: 1, `C:Z\c.jpg`: 2, `C:\y\b.jpg`: 3})
	rebuildFolders(t, st) // compares with the plain reckoning
	if f, _ := st.Folder("C:"); f == nil || f.Total != 2 {
		t.Fatalf("Folder(C:) = %+v, want both photos below it", f)
	}
}

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePhotosDB writes a library's database/Photos.sqlite with the given
// assets table and rows (directory, file name, kind subtype, detected), as
// Photos does: in WAL mode, left open (as by Photos running) until the
// test ends.
func fakePhotosDB(t *testing.T, lib, table string, detected bool, rows [][4]any) {
	t.Helper()
	dir := filepath.Join(lib, "database")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "Photos.sqlite")+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cols := `Z_PK INTEGER PRIMARY KEY, ZDIRECTORY VARCHAR, ZFILENAME VARCHAR, ZKINDSUBTYPE INTEGER`
	if detected {
		cols += `, ZISDETECTEDSCREENSHOT INTEGER`
	}
	if _, err := db.Exec(`CREATE TABLE ` + table + ` (` + cols + `)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		q, args := `INSERT INTO `+table+` (ZDIRECTORY, ZFILENAME, ZKINDSUBTYPE) VALUES (?, ?, ?)`, []any{r[0], r[1], r[2]}
		if detected {
			q, args = `INSERT INTO `+table+` (ZDIRECTORY, ZFILENAME, ZKINDSUBTYPE, ZISDETECTEDSCREENSHOT) VALUES (?, ?, ?, ?)`, append(args, r[3])
		}
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// Photos Apple calls screenshots (kind subtype 10, or detected ones where
// the library records that) are marked memes and screenshots, in every
// catalogued library, whatever their analysis said; the rest keep theirs.
// A library whose database cannot be read is reported, the others still
// marked. Marking again changes nothing.
//
// The database URI: a Windows path must become file:///C:/…, not
// file://C:/… (see TestReadOnlyURI).
func TestMarkLibraryScreenshots(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	newer := filepath.Join(root, "Photos Library.photoslibrary") // macOS 13+: ZASSET, detected flag
	older := filepath.Join(root, "Old #1.photoslibrary")         // Photos 5: ZGENERICASSET
	broken := filepath.Join(root, "Gone.photoslibrary")          // no database
	fakePhotosDB(t, newer, "ZASSET", true, [][4]any{
		{"A", "AAAA.PNG", 10, 0}, // a screenshot
		{"B", "BBBB.HEIC", 0, 0}, // a photo
		{"C", "CCCC.PNG", 0, 1},  // detected as a screenshot
		{"D", "DDDD.PNG", 10, 0}, // a screenshot not catalogued
		{"E", "EEEE.HEIC", 2, 0}, // a live photo
	})
	fakePhotosDB(t, older, "ZGENERICASSET", false, [][4]any{{"F", "FFFF.PNG", 10, nil}, {"G", "GGGG.JPG", 0, nil}})
	var rs []*Result
	for _, p := range []string{
		filepath.Join(newer, "originals", "A", "AAAA.PNG"), filepath.Join(newer, "originals", "B", "BBBB.HEIC"),
		filepath.Join(newer, "originals", "C", "CCCC.PNG"), filepath.Join(newer, "originals", "E", "EEEE.HEIC"),
		filepath.Join(older, "originals", "F", "FFFF.PNG"), filepath.Join(older, "originals", "G", "GGGG.JPG"),
		filepath.Join(broken, "originals", "H", "HHHH.PNG"),
	} {
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Source: SourceSaved, Version: SchemaVersion}})
	}
	rs[1].Photo.Source = SourceCamera
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RebuildFolders(context.Background()); err != nil { // finds the libraries
		t.Fatal(err)
	}
	dirty := st.GetMeta("folders_dirty")
	n, err := st.MarkLibraryScreenshots(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Gone.photoslibrary") || strings.Contains(err.Error(), "Photos Library") || strings.Contains(err.Error(), "Old #1") {
		t.Fatalf("only the library with no database should fail: err %v", err)
	}
	if n != 3 {
		t.Fatalf("marked %d, want 3", n)
	}
	want := []int{SourceMeme, SourceCamera, SourceMeme, SourceSaved, SourceMeme, SourceSaved, SourceSaved}
	for i, r := range rs {
		got, _ := st.Photos([]int64{r.Photo.ID})
		if len(got) != 1 || got[0].Source != want[i] {
			t.Errorf("%s: source %v, want %d", r.Photo.Path, got, want[i])
		}
	}
	if st.GetMeta("folders_dirty") == dirty {
		t.Error("marking left the folders (their covers) current")
	}
	if n, _ := st.MarkLibraryScreenshots(context.Background()); n != 0 {
		t.Errorf("marking again changed %d", n)
	}
}

// openReadOnly's URI keeps the whole path as a path, on Windows too, with
// spaces and '#' escaped.
func TestReadOnlyURI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a #b c")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "x.sqlite")
	w, err := sql.Open("sqlite3", p)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	db, err := openReadOnly(p)
	if err != nil {
		t.Fatalf("opening %s: %v", p, err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO t VALUES (1)`); err == nil {
		t.Fatal("a read-only database accepted a write")
	}
}

// Apple's flag outranks a re-analysis: a screenshot re-indexed (its
// analysis calls it a saved image) or reclassified stays a screenshot;
// any other photo still takes its new source.
func TestAppleScreenshotSurvivesReindex(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for _, p := range []string{"/L.photoslibrary/originals/A/SHOT.PNG", "/L.photoslibrary/originals/B/PHOTO.HEIC"} {
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Source: SourceSaved, Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if n, err := st.markScreenshots([]string{rs[0].Photo.Path}); err != nil || n != 1 {
		t.Fatalf("marked %d (%v)", n, err)
	}
	source := func(r *Result) int {
		t.Helper()
		got, err := st.Photos([]int64{r.Photo.ID})
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %v %v", r.Photo.Path, got, err)
		}
		return got[0].Source
	}
	// an index run re-analyses both (the files changed): analysis says saved, camera
	rs[0].Photo.MTime, rs[0].Photo.Source = 2, SourceSaved
	rs[1].Photo.MTime, rs[1].Photo.Source = 2, SourceCamera
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if got := source(rs[0]); got != SourceMeme {
		t.Fatalf("re-indexed screenshot: source %d, want it to stay a screenshot", got)
	}
	if got := source(rs[1]); got != SourceCamera {
		t.Fatalf("re-indexed photo: source %d, want its new source", got)
	}
	// a full reclassify (retag) leaves it a screenshot too
	in, err := st.SourceInputs(0, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	var set []SourceSet
	for _, x := range in {
		set = append(set, SourceSet{ID: x.ID, Seq: x.Seq, Source: SourceSaved})
	}
	if err := st.SetSources(set); err != nil {
		t.Fatal(err)
	}
	if got := source(rs[0]); got != SourceMeme {
		t.Fatalf("reclassified screenshot: source %d", got)
	}
	if got := source(rs[1]); got != SourceSaved {
		t.Fatalf("reclassified photo: source %d, want saved", got)
	}
}

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
	if err == nil || !strings.Contains(err.Error(), "Gone.photoslibrary") {
		t.Fatalf("the library with no database: err %v", err)
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

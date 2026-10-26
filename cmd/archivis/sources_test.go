package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/store"
)

// An index run marks Apple Photos' screenshots in a library first
// catalogued by that very run: the folders (which list the libraries) are
// worked out first, then the screenshots marked, then the folders again so
// the album covers leave them out.
func TestRegroupMarksScreenshotsOfNewLibrary(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(indexer.Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib := filepath.Join(t.TempDir(), "Photos Library.photoslibrary")
	if err := os.MkdirAll(filepath.Join(lib, "database"), 0o755); err != nil {
		t.Fatal(err)
	}
	pdb, err := sql.Open("sqlite3", filepath.Join(lib, "database", "Photos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE ZASSET (Z_PK INTEGER PRIMARY KEY, ZDIRECTORY VARCHAR, ZFILENAME VARCHAR, ZKINDSUBTYPE INTEGER)`,
		`INSERT INTO ZASSET (ZDIRECTORY, ZFILENAME, ZKINDSUBTYPE) VALUES ('A', 'SHOT.PNG', 10), ('B', 'PHOTO.HEIC', 0)`} {
		if _, err := pdb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	pdb.Close()
	shot, photo := filepath.Join(lib, "originals", "A", "SHOT.PNG"), filepath.Join(lib, "originals", "B", "PHOTO.HEIC")
	var rs []*store.Result
	for p, overall := range map[string]float64{shot: 90, photo: 40} { // the screenshot scores best
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: overall, Source: store.SourceSaved, Version: store.SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if libs, _ := st.PhotosLibraries(); len(libs) != 0 {
		t.Fatal("the library is known before the folders are worked out")
	}
	regroupAndMark(context.Background(), dir, st)
	if n, _ := st.MemeCount(nil); n != 1 {
		t.Fatalf("%d screenshots marked, want 1", n)
	}
	if st.FoldersStale() {
		t.Fatal("folders left stale after marking")
	}
	f, err := st.Folder(lib)
	if err != nil || f == nil {
		t.Fatalf("library folder: %v %v", f, err)
	}
	for _, r := range rs {
		if r.Photo.Path == photo && f.CoverID != r.Photo.ID {
			t.Fatalf("cover %d, want the photo %d, not the screenshot", f.CoverID, r.Photo.ID)
		}
	}
}

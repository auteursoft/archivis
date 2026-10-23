package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCleanPhotosLibraries(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib := "/Users/sean/Pictures/Photos Library.photoslibrary"
	const u1 = "AAAA1111-0000-4000-8000-000000000001"
	const u2 = "BBBB2222-0000-4000-8000-000000000002"
	face := func(x float64) Face {
		return Face{X1: x, Y1: .1, X2: x + .2, Y2: .4, Score: .9, Embedding: []byte{1}}
	}
	mk := func(path string, faces ...Face) *Result {
		return &Result{Photo: Photo{Path: path, Size: 1, MTime: 1, Fingerprint: path, Version: SchemaVersion, FaceCount: len(faces)}, Faces: faces}
	}
	orig := mk(lib+"/originals/A/"+u1+".heic", face(.1), face(.6))
	copy1 := mk(lib+"/resources/derivatives/A/"+u1+"_1_105_c.jpeg", face(.11), face(.6))
	copy2 := mk(lib + "/resources/renders/A/" + u1 + "_1_201_a.jpeg")
	orphan := mk(lib + "/resources/derivatives/B/" + u2 + "_1_105_c.jpeg")
	other := mk("/Volumes/Archive/trip/resources/beach.jpg")
	if err := st.Save([]*Result{orig, copy1, copy2, orphan, other}); err != nil {
		t.Fatal(err)
	}
	// on the copy: Ada named by hand on its first face, an automatic label on its
	// second; a judgement of its aesthetic score
	if _, err := st.LabelFaces("Ada", []int64{copy1.Faces[0].ID}); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`UPDATE faces SET person_id = (SELECT id FROM people WHERE name = 'Ada'), person_source = 'auto' WHERE id = ?`, copy1.Faces[1].ID)
	st.DB.Exec(`INSERT INTO aesthetic_feedback (photo_id, rater, rating, created_at) VALUES (?, 'sean', 8, 1)`, copy1.Photo.ID)

	res, err := st.CleanPhotosLibraries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res != (LibraryCleanup{Libraries: 1, Removed: 2, LabelsMoved: 1, FeedbackMoved: 1, Kept: 1}) {
		t.Fatalf("cleanup: %+v", res)
	}
	exists := func(p *Result) bool {
		var n int
		st.DB.QueryRow(`SELECT COUNT(*) FROM photos WHERE id = ?`, p.Photo.ID).Scan(&n)
		return n == 1
	}
	if exists(copy1) || exists(copy2) || !exists(orig) || !exists(orphan) || !exists(other) {
		t.Fatal("wrong photos removed")
	}
	// the hand-given name moved to the original's matching face, the automatic one did not
	var src string
	var person int64
	st.DB.QueryRow(`SELECT COALESCE(person_source,''), COALESCE(person_id,0) FROM faces WHERE id = ?`, orig.Faces[0].ID).Scan(&src, &person)
	if src != "manual" || person == 0 {
		t.Fatalf("original's first face: source %q person %d", src, person)
	}
	st.DB.QueryRow(`SELECT COALESCE(person_source,'') FROM faces WHERE id = ?`, orig.Faces[1].ID).Scan(&src)
	if src != "" {
		t.Fatalf("an automatic label was moved: %q", src)
	}
	var fb int64
	st.DB.QueryRow(`SELECT photo_id FROM aesthetic_feedback`).Scan(&fb)
	if fb != orig.Photo.ID {
		t.Fatalf("feedback on photo %d, want the original %d", fb, orig.Photo.ID)
	}

	// the orphan stays pending: once its original is catalogued, it goes
	if st.GetMeta(libraryCleanupKey) != `["`+lib+`"]` {
		t.Fatalf("pending libraries: %s", st.GetMeta(libraryCleanupKey))
	}
	if res, _ := st.CleanPhotosLibraries(context.Background()); res.Removed != 0 || res.Kept != 1 {
		t.Fatalf("second run: %+v", res)
	}
	if err := st.Save([]*Result{mk(lib + "/originals/B/" + u2 + ".jpeg")}); err != nil {
		t.Fatal(err)
	}
	if res, _ := st.CleanPhotosLibraries(context.Background()); res.Removed != 1 || res.Kept != 0 {
		t.Fatalf("after the original arrived: %+v", res)
	}
	if exists(orphan) || st.GetMeta(libraryCleanupKey) != "[]" {
		t.Fatalf("not finished: orphan exists %v, meta %s", exists(orphan), st.GetMeta(libraryCleanupKey))
	}
	// done: later runs do nothing
	if res, _ := st.CleanPhotosLibraries(context.Background()); res != (LibraryCleanup{}) {
		t.Fatalf("after completion: %+v", res)
	}
}

// A damaged progress record means looking again, not "done"; an originals
// folder spelled in capitals (as the walker accepts) still finds originals.
func TestCleanPhotosLibrariesRecovers(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib := "/p/Photos Library.photoslibrary"
	const u = "CCCC3333-0000-4000-8000-000000000003"
	mk := func(path string) *Result {
		return &Result{Photo: Photo{Path: path, Size: 1, MTime: 1, Fingerprint: path, Version: SchemaVersion}}
	}
	orig, cp := mk(lib+"/ORIGINALS/C/"+u+".heic"), mk(lib+"/resources/derivatives/C/"+u+"_1_105_c.jpeg")
	if err := st.Save([]*Result{orig, cp}); err != nil {
		t.Fatal(err)
	}
	for _, damaged := range []string{"{damaged", "null"} {
		if err := st.Save([]*Result{cp}); err != nil { // the copy back, if removed
			t.Fatal(err)
		}
		st.SetMeta(libraryCleanupKey, damaged)
		res, err := st.CleanPhotosLibraries(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Removed != 1 || res.Kept != 0 || st.GetMeta(libraryCleanupKey) != "[]" {
			t.Fatalf("%s: cleanup %+v, meta %s", damaged, res, st.GetMeta(libraryCleanupKey))
		}
	}
}

func TestLibraryParts(t *testing.T) {
	for path, want := range map[string][2]string{
		"/p/Photos Library.photoslibrary/originals/A/x.heic": {"/p/Photos Library.photoslibrary", "originals/A/x.heic"},
		"/p/Old.PhotosLibrary/Masters/2012/a.jpg":            {"/p/Old.PhotosLibrary", "Masters/2012/a.jpg"},
		"/p/notes.photoslibrary.txt":                         {"", ""},
		"/p/trip/a.jpg":                                      {"", ""},
	} {
		lib, rest := libraryParts(path)
		if lib != want[0] || rest != want[1] {
			t.Errorf("%s: %q %q", path, lib, rest)
		}
	}
}

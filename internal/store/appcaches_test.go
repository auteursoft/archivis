package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestInAppCache(t *testing.T) {
	for p, want := range map[string]bool{
		"/Pictures/Weddings.cocatalog/Originals/2024/IMG_1.CR3":              false,
		"/Pictures/Weddings.cocatalog/Cache/Thumbnails/IMG_1.jpg":            true,
		"/Sessions/Smith/Capture/CaptureOne/Cache/Proxies/IMG_2.jpg":         true,
		"/Sessions/Smith/Capture/IMG_2.CR3":                                  false,
		"/Pictures/iPhoto Library.photolibrary/Thumbnails/2012/IMG_3.jpg":    true,
		"/Pictures/iPhoto Library.photolibrary/Masters/2012/IMG_3.jpg":       false,
		"/Pictures/Aperture Library.aplibrary/Previews/IMG_4.jpg":            true,
		"/Pictures/Photos Library.photoslibrary/resources/derivatives/A.jpg": true,
		"/Pictures/Photos Library.photoslibrary/originals/A/A.heic":          false,
		`C:\Pictures\Weddings.cocatalog\Cache\Thumbnails\IMG_1.jpg`:          true,
		"/Volumes/Archive/trip/resources/beach.jpg":                          false,
		"/Volumes/Archive/Previews/beach.jpg":                                false, // an ordinary folder of that name
	} {
		if got := InAppCache(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}

// App copies catalogued before the walker skipped them go: their hand-given
// face names and ratings move to the photo they copy (the same hash, or
// alike and taken in the same second by the same camera). A copy with no
// twin goes too, unless it holds hand-named faces or ratings, or it is in a
// Photos library (perhaps the photo's only copy). Photos are left alone; a
// second run changes nothing; once nothing is kept, later runs do nothing.
func TestCleanAppCaches(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	face := Face{X1: .1, Y1: .1, X2: .3, Y2: .4, Score: .9, Embedding: []byte{1}}
	mk := func(path string, hash uint64, at int64, faces ...Face) *Result {
		return &Result{Photo: Photo{Path: path, Size: 1, MTime: 1, Fingerprint: path, PHash: hash, TakenAt: at, CameraModel: "Z 8",
			FaceCount: len(faces), Version: SchemaVersion}, Faces: faces}
	}
	orig := mk("/Shoots/Weddings.cocatalog/Originals/2024/IMG_1.NEF", 0xF0F0, 1_700_000_000, face)
	thumb := mk("/Shoots/Weddings.cocatalog/Cache/Thumbnails/IMG_1.jpg", 0xF0F0^0b111, 1_700_000_000, face) // resized: a few bits off, same second
	proxy := mk("/Sessions/Smith/Capture/CaptureOne/Cache/Proxies/IMG_9.jpg", 0x1234, 0)                    // no twin, nothing personal
	named := mk("/Pictures/iPhoto Library.photolibrary/Thumbnails/IMG_5.jpg", 0x5555, 0, face)              // no twin, a hand-named face
	google := mk("/Volumes/super_40/Google Photos/2023/IMG_7.jpg", 0xABCD, 0)
	derivative := mk("/Pictures/Photos Library.photoslibrary/resources/derivatives/A/AAAA_1_105_c.jpeg", 0xABCD, 0) // twin in Google Photos
	only := mk("/Pictures/Photos Library.photoslibrary/resources/derivatives/B/BBBB_1_105_c.jpeg", 0x7777, 0)       // perhaps the only copy
	if err := st.Save([]*Result{orig, thumb, proxy, named, google, derivative, only}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LabelFaces("Ada", []int64{thumb.Faces[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LabelFaces("Bo", []int64{named.Faces[0].ID}); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`INSERT INTO aesthetic_feedback (photo_id, rater, rating, created_at) VALUES (?, 'sean', 8, 1)`, thumb.Photo.ID)

	res, err := st.CleanAppCaches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res != (AppCacheCleanup{Removed: 3, LabelsMoved: 1, FeedbackMoved: 1, Kept: 2}) {
		t.Fatalf("cleanup: %+v", res)
	}
	exists := func(r *Result) bool {
		var n int
		st.DB.QueryRow(`SELECT COUNT(*) FROM photos WHERE id = ?`, r.Photo.ID).Scan(&n)
		return n == 1
	}
	for _, c := range []struct {
		r    *Result
		want bool
	}{{orig, true}, {thumb, false}, {proxy, false}, {named, true}, {google, true}, {derivative, false}, {only, true}} {
		if exists(c.r) != c.want {
			t.Errorf("%s: exists %v, want %v", c.r.Photo.Path, !c.want, c.want)
		}
	}
	var src string
	st.DB.QueryRow(`SELECT COALESCE(person_source, '') FROM faces WHERE id = ?`, orig.Faces[0].ID).Scan(&src)
	if src != "manual" {
		t.Fatal("the thumbnail's hand-given name did not move to the original")
	}
	var fb int
	st.DB.QueryRow(`SELECT COUNT(*) FROM aesthetic_feedback WHERE photo_id = ?`, orig.Photo.ID).Scan(&fb)
	if fb != 1 {
		t.Fatal("the thumbnail's rating did not move to the original")
	}
	if again, _ := st.CleanAppCaches(context.Background()); again.Removed != 0 || again.Kept != 2 {
		t.Fatalf("second run: %+v", again)
	}
	// once nothing is kept, the cleanup is done for good
	st.DB.Exec(`DELETE FROM photos WHERE id IN (?, ?)`, named.Photo.ID, only.Photo.ID)
	if r, _ := st.CleanAppCaches(context.Background()); r != (AppCacheCleanup{}) || st.GetMeta(appCacheCleanupKey) != "1" {
		t.Fatalf("clean catalogue: %+v, done %q", r, st.GetMeta(appCacheCleanupKey))
	}
}

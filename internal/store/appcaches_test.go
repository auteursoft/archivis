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

// The twin must be safe to merge into: a copy whose hand-given names can't
// all move (the twin has no matching face) stays; photos with no camera
// named are matched by identical hash only; among the frames a camera took
// around the copy's second, the closest-looking (then the closest in time)
// is its twin, and two equally close ones are no twin at all.
func TestAppCacheTwinsAreSafe(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	face := Face{X1: .1, Y1: .1, X2: .3, Y2: .4, Score: .9, Embedding: []byte{1}}
	mk := func(path, cam string, hash uint64, at int64, faces ...Face) *Result {
		return &Result{Photo: Photo{Path: path, Size: 1, MTime: 1, Fingerprint: path, PHash: hash, TakenAt: at, CameraModel: cam,
			FaceCount: len(faces), Version: SchemaVersion}, Faces: faces}
	}
	const t0 = 1_700_000_000
	noFaces := mk("/Shoots/A.cocatalog/Originals/IMG_1.NEF", "Z 8", 0x0F0F, t0)             // indexed without faces
	named := mk("/Shoots/A.cocatalog/Cache/Thumbnails/IMG_1.jpg", "Z 8", 0x0F0F, t0, face)  // its name can't move
	camless := mk("/Scans/scan_1.jpg", "", 0xF000, t0)                                      // no camera named
	camlessCopy := mk("/Shoots/A.cocatalog/Cache/Thumbnails/scan.jpg", "", 0xF000^0b11, t0) // near it, but no camera: no twin
	farFrame := mk("/Shoots/B.cocatalog/Originals/IMG_2.NEF", "Z 9", 0xAA00^0x3FF, t0+9)    // a second off, 10 bits off, saved first
	nearFrame := mk("/Shoots/B.cocatalog/Originals/IMG_3.NEF", "Z 9", 0xAA00^0b11, t0+10)   // same second, 2 bits off: the original
	burstCopy := mk("/Shoots/B.cocatalog/Cache/Previews/IMG_3.jpg", "Z 9", 0xAA00, t0+10)
	tieA := mk("/Shoots/C.cocatalog/Originals/IMG_4.NEF", "Z 6", 0x5500^0b1, t0+20)
	tieB := mk("/Shoots/C.cocatalog/Originals/IMG_5.NEF", "Z 6", 0x5500^0b10, t0+20)
	tieCopy := mk("/Shoots/C.cocatalog/Cache/Previews/IMG_4.jpg", "Z 6", 0x5500, t0+20)
	if err := st.Save([]*Result{noFaces, named, camless, camlessCopy, farFrame, nearFrame, burstCopy, tieA, tieB, tieCopy}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LabelFaces("Ada", []int64{named.Faces[0].ID}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Result{camlessCopy, burstCopy, tieCopy} { // rated: kept unless a twin takes the rating
		st.DB.Exec(`INSERT INTO aesthetic_feedback (photo_id, rater, rating, created_at) VALUES (?, 'sean', 8, 1)`, r.Photo.ID)
	}
	res, err := st.CleanAppCaches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exists := func(r *Result) bool {
		var n int
		st.DB.QueryRow(`SELECT COUNT(*) FROM photos WHERE id = ?`, r.Photo.ID).Scan(&n)
		return n == 1
	}
	if !exists(named) {
		t.Error("a copy whose hand-given name had no face to move to was removed")
	}
	if !exists(camlessCopy) {
		t.Error("a camera-less copy was merged into a camera-less photo by a near hash")
	}
	if exists(burstCopy) {
		t.Error("the burst frame's preview was kept")
	}
	var on int64
	st.DB.QueryRow(`SELECT photo_id FROM aesthetic_feedback WHERE rater = 'sean' AND photo_id IN (?, ?)`, farFrame.Photo.ID, nearFrame.Photo.ID).Scan(&on)
	if on != nearFrame.Photo.ID {
		t.Errorf("the preview's rating moved to photo %d, want the nearest frame %d", on, nearFrame.Photo.ID)
	}
	if !exists(tieCopy) {
		t.Error("a copy with two equally close frames was merged into one of them")
	}
	if res.Kept != 3 || res.Removed != 1 {
		t.Errorf("cleanup: %+v, want 3 kept, 1 removed", res)
	}
}

package store

import (
	"context"
	"database/sql"
	"math/bits"
	"path/filepath"
	"strings"
)

// Photo apps keep their own copies of photos (thumbnails, previews,
// proxies) beside, or instead of, the originals. Those are not photos to
// catalogue: the walker skips them, and CleanAppCaches removes the ones
// catalogued before it did.
//
// A package (a folder named with one of these extensions) holds originals
// only in the folders listed; everything else in it is the app's own.
var appPackages = map[string][]string{
	".photoslibrary": {"originals", "Masters"}, // Apple Photos (Masters/: Photos 4 and older)
	".photolibrary":  {"Masters", "Originals"}, // iPhoto (Originals/: iPhoto 8 and older)
	".aplibrary":     {"Masters"},              // Aperture
	".cocatalog":     {"Originals"},            // Capture One catalog
}

// appCacheDirs are folders that hold only an app's own files, wherever
// they are: a Capture One session keeps its thumbnails, previews and
// settings in a CaptureOne folder beside the photos.
var appCacheDirs = []string{"CaptureOne"}

// PackageChildAllowed reports whether base, a direct child of parent, may
// hold photos to catalogue: anything, unless parent is an app's package
// and base is not one of its originals folders, or base is an app's cache
// folder.
func PackageChildAllowed(parent, base string) bool {
	for _, d := range appCacheDirs {
		if strings.EqualFold(base, d) {
			return false
		}
	}
	allowed, ok := appPackages[strings.ToLower(filepath.Ext(parent))]
	if !ok {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(base, a) {
			return true
		}
	}
	return false
}

// InAppCache reports whether path lies where PackageChildAllowed rules
// out, at any level (either separator), as the walker would have.
func InAppCache(path string) bool {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	for i := 1; i < len(parts); i++ {
		if !PackageChildAllowed(parts[i-1], parts[i]) {
			return true
		}
	}
	return false
}

// AppCacheCleanup reports what CleanAppCaches did.
type AppCacheCleanup struct {
	Removed       int // copies removed
	LabelsMoved   int // manual face names moved to the twin
	FeedbackMoved int // aesthetic judgements moved to the twin
	Kept          int // copies kept: no twin, and either hand-named faces or ratings on them, or the only copy of a Photos library photo
}

const appCacheCleanupKey = "app_cache_cleanup" // "1" once nothing is left to do

// CleanAppCaches removes the app copies catalogued before the walker
// skipped them: Apple Photos copies CleanPhotosLibraries kept (no original
// in the library), and iPhoto, Aperture and Capture One caches. Each copy's
// twin is looked for (the same perceptual hash, or a photo taken within a
// second by the same camera that looks alike) outside any cache; manual
// face names and aesthetic judgements move to it, and the copy goes. A
// copy without a twin goes too, unless it holds hand-named faces or
// ratings, or it is in a Photos library (it may be a photo's only copy, its
// original kept in iCloud): those stay, and are reported. Once nothing is
// left, later runs do nothing.
func (s *Store) CleanAppCaches(ctx context.Context) (AppCacheCleanup, error) {
	var res AppCacheCleanup
	if s.GetMeta(appCacheCleanupKey) == "1" {
		return res, nil
	}
	type cacheCopy struct {
		id, at int64
		path   string
		camera string
		phash  uint64
	}
	var copies []cacheCopy
	rows, err := s.DB.Query(`SELECT id, path, COALESCE(taken_at, 0), TRIM(COALESCE(camera_make, '') || ' ' || COALESCE(camera_model, '')), phash FROM photos
		WHERE path LIKE '%.photoslibrary%' OR path LIKE '%.photolibrary%' OR path LIKE '%.aplibrary%' OR path LIKE '%.cocatalog%' OR path LIKE '%CaptureOne%'`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var c cacheCopy
		if err := rows.Scan(&c.id, &c.path, &c.at, &c.camera, (*int64ptr)(&c.phash)); err != nil {
			rows.Close()
			return res, err
		}
		if InAppCache(c.path) {
			copies = append(copies, c)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return res, err // a partial list must not count as all of them
	}
	for start := 0; start < len(copies); start += 200 { // short transactions
		if err := ctx.Err(); err != nil {
			return res, err
		}
		batch := copies[start:min(start+200, len(copies))]
		err := s.inTx(func(tx *sql.Tx) error {
			for _, c := range batch {
				var same int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM photos WHERE id = ? AND path = ?`, c.id, c.path).Scan(&same); err != nil {
					return err
				}
				if same == 0 {
					continue // gone meanwhile (its id may be reused)
				}
				twin, err := twinOf(tx, c.id, c.phash, c.at, c.camera)
				if err != nil {
					return err
				}
				if twin == 0 {
					var personal int
					if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM faces WHERE photo_id = ? AND person_source = 'manual')
						+ (SELECT COUNT(*) FROM aesthetic_feedback WHERE photo_id = ?)`, c.id, c.id).Scan(&personal); err != nil {
						return err
					}
					if lib, _ := libraryParts(c.path); personal > 0 || lib != "" {
						res.Kept++
						continue
					}
				} else {
					n, err := moveManualLabels(tx, c.id, twin)
					if err != nil {
						return err
					}
					res.LabelsMoved += n
					r, err := tx.Exec(`UPDATE aesthetic_feedback SET photo_id = ? WHERE photo_id = ?`, twin, c.id)
					if err != nil {
						return err
					}
					m, _ := r.RowsAffected()
					res.FeedbackMoved += int(m)
				}
				if _, err := tx.Exec(`DELETE FROM photos WHERE id = ?`, c.id); err != nil {
					return err
				}
				res.Removed++
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	if res.Kept == 0 {
		return res, s.SetMeta(appCacheCleanupKey, "1")
	}
	return res, nil // the kept ones are looked at again next time: their twins may be catalogued by then
}

// twinMaxBits is how far a copy's perceptual hash may be from its twin's
// when both were taken in the same second by the same camera: app copies
// are resized and recompressed, which moves a few bits.
const twinMaxBits = 10

// twinOf finds a catalogued photo, outside any app cache, that copy id is
// a copy of (0 if none): one with the same perceptual hash, or one taken
// within a second of it by the same camera that looks alike.
func twinOf(tx *sql.Tx, id int64, phash uint64, at int64, camera string) (int64, error) {
	pick := func(q string, args []any, ok func(path string, h uint64) bool) (int64, error) {
		rows, err := tx.Query(q, args...)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var tid int64
			var path string
			var h uint64
			if err := rows.Scan(&tid, &path, (*int64ptr)(&h)); err != nil {
				return 0, err
			}
			if tid != id && !InAppCache(path) && ok(path, h) {
				return tid, nil
			}
		}
		return 0, rows.Err()
	}
	if phash != 0 {
		tid, err := pick(`SELECT id, path, phash FROM photos WHERE phash = ? ORDER BY id`, []any{int64(phash)},
			func(string, uint64) bool { return true })
		if err != nil || tid != 0 {
			return tid, err
		}
	}
	if at == 0 || phash == 0 {
		return 0, nil
	}
	return pick(`SELECT id, path, phash FROM photos WHERE taken_at BETWEEN ? AND ?
		AND TRIM(COALESCE(camera_make, '') || ' ' || COALESCE(camera_model, '')) = ? ORDER BY id`, []any{at - 1, at + 1, camera},
		func(_ string, h uint64) bool { return h != 0 && bits.OnesCount64(h^phash) <= twinMaxBits })
}

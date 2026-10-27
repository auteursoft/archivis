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
		id, at      int64
		path        string
		make, model string
		phash       uint64
	}
	var copies []cacheCopy
	rows, err := s.DB.Query(`SELECT id, path, COALESCE(taken_at, 0), COALESCE(camera_make, ''), COALESCE(camera_model, ''), phash FROM photos
		WHERE path LIKE '%.photoslibrary%' OR path LIKE '%.photolibrary%' OR path LIKE '%.aplibrary%' OR path LIKE '%.cocatalog%' OR path LIKE '%CaptureOne%'`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var c cacheCopy
		if err := rows.Scan(&c.id, &c.path, &c.at, &c.make, &c.model, (*int64ptr)(&c.phash)); err != nil {
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
				twin, err := twinOf(tx, c.id, c.phash, c.at, c.make, c.model)
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
					// a hand-given name that found no face on the twin (none
					// overlapping, or faces not detected there) would be lost
					// with the copy: keep it
					var lost int
					if err := tx.QueryRow(`SELECT COUNT(*) FROM faces c WHERE c.photo_id = ? AND c.person_source = 'manual'
						AND NOT EXISTS (SELECT 1 FROM faces t WHERE t.photo_id = ? AND t.person_source = 'manual' AND t.person_id = c.person_id)`,
						c.id, twin).Scan(&lost); err != nil {
						return err
					}
					if lost > 0 {
						res.Kept++
						continue
					}
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
// a copy of (0 if none): the one with the same perceptual hash (of several,
// the one also taken by its camera in its second); else, if the
// copy names its camera model, the closest-looking photo that camera (the
// same make and model, each) took within
// a second of it (nearest hash, then nearest time). Two equally close
// candidates (a burst) are no twin: which one it copies is not known.
func twinOf(tx *sql.Tx, id int64, phash uint64, at int64, make, model string) (int64, error) {
	if phash != 0 {
		// the same hash: one photo is the twin; several (identical frames
		// of a burst, say) only if exactly one also has the copy's camera
		// and second, else which one it copies is not known
		rows, err := tx.Query(`SELECT id, path, COALESCE(taken_at, 0), COALESCE(camera_make, ''), COALESCE(camera_model, '') FROM photos WHERE phash = ?`, int64(phash))
		if err != nil {
			return 0, err
		}
		var all, same []int64
		for rows.Next() {
			var tid, tat int64
			var path, tmake, tmodel string
			if err := rows.Scan(&tid, &path, &tat, &tmake, &tmodel); err != nil {
				rows.Close()
				return 0, err
			}
			if tid == id || InAppCache(path) {
				continue
			}
			all = append(all, tid)
			if model != "" && tmake == make && tmodel == model && at != 0 && max(tat-at, at-tat) <= 1 {
				same = append(same, tid)
			}
		}
		err = rows.Err()
		rows.Close()
		switch {
		case err != nil:
			return 0, err
		case len(all) == 1:
			return all[0], nil
		case len(same) == 1:
			return same[0], nil
		case len(all) > 1:
			return 0, nil // ambiguous
		}
	}
	if at == 0 || phash == 0 || model == "" {
		return 0, nil // a photo with no camera model, or undated, has nothing safe to match by
	}
	rows, err := tx.Query(`SELECT id, path, phash, taken_at FROM photos WHERE taken_at BETWEEN ? AND ?
		AND COALESCE(camera_make, '') = ? AND COALESCE(camera_model, '') = ?`, at-1, at+1, make, model) // make and model each the same
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type cand struct {
		id         int64
		bits, secs int
	}
	var best *cand
	tie := false
	for rows.Next() {
		var c cand
		var path string
		var h uint64
		var tat int64
		if err := rows.Scan(&c.id, &path, (*int64ptr)(&h), &tat); err != nil {
			return 0, err
		}
		if c.id == id || h == 0 || InAppCache(path) {
			continue
		}
		c.bits, c.secs = bits.OnesCount64(h^phash), int(max(tat-at, at-tat))
		if c.bits > twinMaxBits {
			continue
		}
		switch {
		case best == nil || c.bits < best.bits || (c.bits == best.bits && c.secs < best.secs):
			best, tie = &c, false
		case c.bits == best.bits && c.secs == best.secs:
			tie = true
		}
	}
	if err := rows.Err(); err != nil || best == nil || tie {
		return 0, err
	}
	return best.id, nil
}

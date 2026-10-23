package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
)

// Apple Photos libraries ("…/Photos Library.photoslibrary") keep each
// photo's original under originals/<first char of UUID>/<UUID>.<ext>, and
// Apple's own resized JPEG copies under resources/ (derivatives, renders)
// named <UUID>_<variant>.jpeg. Older versions of archivis indexed the whole
// package, cataloguing those copies as extra photos. The indexer now reads
// only originals/ (and Masters/ of older libraries); CleanPhotosLibraries
// removes the copies already catalogued.

// LibraryCleanup reports what CleanPhotosLibraries did.
type LibraryCleanup struct {
	Libraries     int // Photos libraries with catalogued copies
	Removed       int // Apple's copies removed (their original is catalogued)
	LabelsMoved   int // manual face names moved from a copy to its original
	FeedbackMoved int // aesthetic judgements moved from a copy to its original
	Kept          int // copies kept: no catalogued original (e.g. iCloud "Optimize Mac Storage")
}

var uuidPrefix = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}`)

// libraryParts splits a path inside a Photos library into the library
// package's path and the rest ("" when the path is not in a library).
func libraryParts(path string) (lib, rest string) {
	i := strings.Index(strings.ToLower(path), ".photoslibrary")
	if i < 0 {
		return "", ""
	}
	end := i + len(".photoslibrary")
	if end >= len(path) || (path[end] != '/' && path[end] != '\\') {
		return "", ""
	}
	return path[:end], path[end+1:]
}

// isLibraryOriginal reports whether rest (a path inside a library) is under
// originals/ or Masters/.
func isLibraryOriginal(rest string) bool {
	first, _, _ := strings.Cut(strings.ReplaceAll(rest, `\`, "/"), "/")
	return strings.EqualFold(first, "originals") || strings.EqualFold(first, "Masters")
}

const libraryCleanupKey = "photoslibrary_cleanup" // "" never run; else JSON list of libraries still to check

// CleanPhotosLibraries removes Apple's own copies (from inside Photos
// libraries, outside originals/) from the catalogue, once each copy's
// original is catalogued: manual face names and aesthetic judgements on the
// copy are moved to the original first. Copies whose original is not
// catalogued are kept. The first run scans every path once to find the
// libraries; later runs read only those libraries' paths, and none once no
// copies remain.
func (s *Store) CleanPhotosLibraries(ctx context.Context) (LibraryCleanup, error) {
	var res LibraryCleanup
	var libs []string
	if v := s.GetMeta(libraryCleanupKey); v != "" {
		if err := json.Unmarshal([]byte(v), &libs); err != nil {
			libs = nil
		}
		if len(libs) == 0 {
			return res, nil // done
		}
	} else {
		found, err := s.findLibraries()
		if err != nil {
			return res, err
		}
		libs = found
	}
	var pending []string
	for _, lib := range libs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		kept, err := s.cleanLibrary(ctx, lib, &res)
		if err != nil {
			return res, err
		}
		res.Libraries++
		if kept > 0 {
			pending = append(pending, lib) // its originals may be catalogued later
		}
	}
	b, _ := json.Marshal(append([]string{}, pending...))
	return res, s.SetMeta(libraryCleanupKey, string(b))
}

// findLibraries lists the Photos libraries that have catalogued copies.
func (s *Store) findLibraries() ([]string, error) {
	rows, err := s.DB.Query(`SELECT path FROM photos WHERE path LIKE '%.photoslibrary%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var libs []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if lib, rest := libraryParts(p); lib != "" && !isLibraryOriginal(rest) && !seen[lib] {
			seen[lib] = true
			libs = append(libs, lib)
		}
	}
	return libs, rows.Err()
}

type libCopy struct {
	id   int64
	path string
}

// cleanLibrary handles one library; it returns how many copies it kept.
func (s *Store) cleanLibrary(ctx context.Context, lib string, res *LibraryCleanup) (int, error) {
	// the library's catalogued paths, through the path index
	var copies []libCopy
	for _, sep := range []string{"/", `\`} {
		rows, err := s.DB.Query(`SELECT id, path FROM photos WHERE path >= ? AND path < ?`, lib+sep, lib+string(sep[0]+1))
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var c libCopy
			if err := rows.Scan(&c.id, &c.path); err != nil {
				rows.Close()
				return 0, err
			}
			if _, rest := libraryParts(c.path); !isLibraryOriginal(rest) {
				copies = append(copies, c)
			}
		}
		rows.Close()
	}
	kept := 0
	for start := 0; start < len(copies); start += 200 { // short transactions
		if err := ctx.Err(); err != nil {
			return kept, err
		}
		batch := copies[start:min(start+200, len(copies))]
		err := s.inTx(func(tx *sql.Tx) error {
			for _, c := range batch {
				orig, err := originalOf(tx, lib, c.path)
				if err != nil {
					return err
				}
				if orig == 0 {
					kept++
					continue
				}
				n, err := moveManualLabels(tx, c.id, orig)
				if err != nil {
					return err
				}
				res.LabelsMoved += n
				r, err := tx.Exec(`UPDATE aesthetic_feedback SET photo_id = ? WHERE photo_id = ?`, orig, c.id)
				if err != nil {
					return err
				}
				m, _ := r.RowsAffected()
				res.FeedbackMoved += int(m)
				if _, err := tx.Exec(`DELETE FROM photos WHERE id = ?`, c.id); err != nil {
					return err
				}
				res.Removed++
			}
			return nil
		})
		if err != nil {
			return kept, err
		}
	}
	res.Kept += kept
	return kept, nil
}

// originalOf finds the catalogued original of a copy inside lib: the photo
// at originals/<X>/<UUID>.<ext>, by the UUID the copy's name begins with
// (0 if none).
func originalOf(tx *sql.Tx, lib, copyPath string) (int64, error) {
	base := copyPath[strings.LastIndexAny(copyPath, `/\`)+1:]
	uuid := uuidPrefix.FindString(base)
	if uuid == "" {
		return 0, nil
	}
	for _, sep := range []string{"/", `\`} {
		for _, first := range []string{strings.ToUpper(uuid[:1]), strings.ToLower(uuid[:1])} {
			prefix := lib + sep + "originals" + sep + first + sep + uuid + "."
			var id int64
			err := tx.QueryRow(`SELECT id FROM photos WHERE path >= ? AND path < ? ORDER BY path LIMIT 1`, prefix, prefix+"\xff").Scan(&id)
			if err == nil {
				return id, nil
			}
			if err != sql.ErrNoRows {
				return 0, err
			}
		}
	}
	return 0, nil
}

type faceBox struct {
	id             int64
	x1, y1, x2, y2 float64
	person         sql.NullInt64
	source         sql.NullString
}

func facesOf(tx *sql.Tx, photo int64) ([]faceBox, error) {
	rows, err := tx.Query(`SELECT id, x1, y1, x2, y2, person_id, person_source FROM faces WHERE photo_id = ?`, photo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []faceBox
	for rows.Next() {
		var f faceBox
		if err := rows.Scan(&f.id, &f.x1, &f.y1, &f.x2, &f.y2, &f.person, &f.source); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// boxOverlap is the overlap of two face boxes (normalised), 0..1.
func boxOverlap(a, b faceBox) float64 {
	ix := max(0, min(a.x2, b.x2)-max(a.x1, b.x1))
	iy := max(0, min(a.y2, b.y2)-max(a.y1, b.y1))
	inter := ix * iy
	union := (a.x2-a.x1)*(a.y2-a.y1) + (b.x2-b.x1)*(b.y2-b.y1) - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

// moveManualLabels gives each manually named face on the copy's photo the
// same name on the original's matching face (the one it overlaps most, if
// by at least half), unless that face is already named by hand. Copies are
// rendered upright like the analysed originals, so the boxes line up.
func moveManualLabels(tx *sql.Tx, from, to int64) (int, error) {
	src, err := facesOf(tx, from)
	if err != nil {
		return 0, err
	}
	dst, err := facesOf(tx, to)
	if err != nil {
		return 0, err
	}
	moved := 0
	used := map[int64]bool{}
	for _, f := range src {
		if !f.person.Valid || f.source.String != "manual" {
			continue
		}
		best, bestIoU := -1, 0.5
		for i, g := range dst {
			if used[g.id] || g.source.String == "manual" {
				continue
			}
			if v := boxOverlap(f, g); v >= bestIoU {
				best, bestIoU = i, v
			}
		}
		if best < 0 {
			continue
		}
		used[dst[best].id] = true
		if _, err := tx.Exec(`UPDATE faces SET person_id = ?, person_source = 'manual', person_sim = NULL WHERE id = ?`, f.person.Int64, dst[best].id); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

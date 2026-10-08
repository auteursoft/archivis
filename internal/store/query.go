package store

import (
	"fmt"
	"golang.org/x/text/unicode/norm"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Filter narrows a browse/search query. Zero values mean "no constraint".
type Filter struct {
	MinOverall    float64
	MinTechnical  float64
	MinFocus      float64 // 0..1
	MinExposure   float64 // 0..1
	MinAesthetic  float64 // 0..10
	MaxCast       float64 // max colour cast strength (grey-edge angle, degrees)
	MaxNoise      float64
	MaxBlockiness float64 // exclude heavily JPEG-compressed photos
	Camera        string  // substring of make/model
	Lens          string
	PathContains  string
	Tag           string
	PersonID      int64
	Faces         string // "", "any", "none", "1", "group" (3+)
	Mono          string // "", "only", "exclude"
	From, To      int64  // unix seconds (taken_at)
	Year          int
	Sort          string // overall|technical|aesthetic|sharpness|newest|oldest|colorful|faces|worst
	Limit         int
	Offset        int
}

func (f Filter) where() (string, []any) {
	var w []string
	var a []any
	add := func(cond string, args ...any) { w = append(w, cond); a = append(a, args...) }
	if f.MinOverall > 0 {
		add("overall >= ?", f.MinOverall)
	}
	if f.MinTechnical > 0 {
		add("technical >= ?", f.MinTechnical)
	}
	if f.MinFocus > 0 {
		add("focus_score >= ?", f.MinFocus)
	}
	if f.MinExposure > 0 {
		add("exposure_score >= ?", f.MinExposure)
	}
	if f.MinAesthetic > 0 {
		add("aesthetic >= ?", f.MinAesthetic)
	}
	if f.MaxCast > 0 {
		add("(cast_strength <= ? OR monochrome = 1)", f.MaxCast)
	}
	if f.MaxBlockiness > 0 {
		add("blockiness <= ?", f.MaxBlockiness)
	}
	if f.MaxNoise > 0 {
		add("noise <= ?", f.MaxNoise)
	}
	if f.Camera != "" {
		add(`(camera_make || ' ' || camera_model) LIKE ? ESCAPE '\'`, likeContains(f.Camera))
	}
	if f.Lens != "" {
		add(`lens LIKE ? ESCAPE '\'`, likeContains(f.Lens))
	}
	if f.PathContains != "" {
		add(`path LIKE ? ESCAPE '\'`, likeContains(f.PathContains))
	}
	if f.Tag != "" {
		add("id IN (SELECT photo_id FROM tags WHERE tag = ?)", f.Tag)
	}
	if f.PersonID > 0 {
		add("id IN (SELECT photo_id FROM faces WHERE person_id = ?)", f.PersonID)
	}
	switch f.Faces {
	case "any":
		add("face_count > 0")
	case "none":
		add("face_count = 0")
	case "1":
		add("face_count = 1")
	case "group":
		add("face_count >= 3")
	}
	switch f.Mono {
	case "only":
		add("monochrome = 1")
	case "exclude":
		add("monochrome = 0")
	}
	if f.From > 0 {
		add("taken_at >= ?", f.From)
	}
	if f.To > 0 {
		add("taken_at < ?", f.To)
	}
	if f.Year > 0 {
		add("strftime('%Y', taken_at, 'unixepoch') = ?", fmt.Sprint(f.Year))
	}
	if len(w) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(w, " AND "), a
}

func (f Filter) order() string {
	switch f.Sort {
	case "technical":
		return " ORDER BY technical DESC"
	case "aesthetic":
		return " ORDER BY aesthetic DESC"
	case "sharpness":
		return " ORDER BY focus_score DESC, sharpness DESC"
	case "newest":
		return " ORDER BY taken_at DESC"
	case "oldest":
		return " ORDER BY CASE WHEN taken_at > 0 THEN 0 ELSE 1 END, taken_at ASC"
	case "colorful":
		return " ORDER BY colorfulness DESC"
	case "faces":
		return " ORDER BY face_count DESC, overall DESC"
	case "worst":
		return " ORDER BY overall ASC"
	case "path":
		return " ORDER BY path"
	default:
		return " ORDER BY overall DESC"
	}
}

// Browse returns a page of photos matching f, plus the total match count.
func (s *Store) Browse(f Filter) ([]Photo, int, error) {
	where, args := f.where()
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM photos`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	q := `SELECT ` + photoCols + ` FROM photos` + where + f.order() + ` LIMIT ? OFFSET ?`
	ps, err := s.queryPhotos(q, append(args, f.Limit, f.Offset)...)
	return ps, total, err
}

// FilterIDs returns the subset of ids that satisfy f (order not preserved).
func (s *Store) FilterIDs(f Filter, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	where, args := f.where()
	if where == "" {
		where = " WHERE 1"
	}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		q := `SELECT id FROM photos` + where + ` AND id IN (` + placeholders(len(chunk)) + `)`
		rows, err := s.DB.Query(q, append(append([]any{}, args...), int64sToAny(chunk)...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			out[id] = true
		}
		rows.Close()
	}
	return out, nil
}

// EachEmbedding streams (id, blob) pairs for photos (CLIP) or faces with
// id > after, used to (incrementally) build the in-memory indexes.
func (s *Store) EachEmbedding(faces bool, after int64, fn func(id int64, blob []byte)) (int64, error) {
	q := `SELECT photo_id, clip FROM photo_clip WHERE photo_id > ? ORDER BY photo_id`
	if faces {
		q = `SELECT id, embedding FROM faces WHERE id > ? ORDER BY id`
	}
	rows, err := s.DB.Query(q, after)
	if err != nil {
		return after, err
	}
	defer rows.Close()
	last := after
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return last, err
		}
		fn(id, b)
		last = id
	}
	return last, rows.Err()
}

// EachChangedEmbedding streams CLIP embeddings of photos saved after change
// number since (see MaxSeq), so in-place updates reach the search index.
func (s *Store) EachChangedEmbedding(since int64, fn func(id int64, blob []byte)) error {
	rows, err := s.DB.Query(`SELECT c.photo_id, c.clip FROM photo_clip c JOIN photos p ON p.id = c.photo_id WHERE p.seq > ?`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return err
		}
		fn(id, b)
	}
	return rows.Err()
}

// FacePhotoMap returns face id -> photo id for all faces with id > after.
func (s *Store) FacePhotoMap(after int64, into map[int64]int64) error {
	rows, err := s.DB.Query(`SELECT id, photo_id FROM faces WHERE id > ?`, after)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f, p int64
		rows.Scan(&f, &p)
		into[f] = p
	}
	return rows.Err()
}

// Count returns the number of rows in photos or faces.
func (s *Store) Count(faces bool) int64 {
	var n int64
	t := "photos"
	if faces {
		t = "faces"
	}
	s.DB.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n)
	return n
}

// Burst is a run of frames shot within a short interval on one camera.
type Burst struct {
	Photos []Photo // best first
}

// Bursts groups photos taken on the same camera within gap seconds of each
// other (only groups of at least minSize), newest first, best frame first.
// Useful for picking the keeper out of a sequence. It scans newest-first and
// stops once f.Limit bursts are found, so it stays fast on large archives.
func (s *Store) Bursts(f Filter, gap int64, minSize int) ([]Burst, error) {
	where, args := f.where()
	if where == "" {
		where = " WHERE taken_at > 0"
	} else {
		where += " AND taken_at > 0"
	}
	limit := max(1, f.Limit)
	rows, err := s.DB.Query(`SELECT id, camera_make || camera_model, taken_at FROM photos`+where+` ORDER BY taken_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	type run struct {
		ids  []int64
		last int64
	}
	open := map[string]*run{}
	var groups [][]int64
	emit := func(r *run) {
		if len(r.ids) >= minSize {
			groups = append(groups, r.ids)
		}
	}
	for rows.Next() && len(groups) < limit {
		var id, t int64
		var cam string
		rows.Scan(&id, &cam, &t)
		r := open[cam]
		if r != nil && r.last-t > gap {
			emit(r)
			r = nil
		}
		if r == nil {
			r = &run{}
			open[cam] = r
		}
		r.ids = append(r.ids, id)
		r.last = t
	}
	rows.Close()
	for _, r := range open {
		if len(groups) >= limit {
			break
		}
		emit(r)
	}
	var out []Burst
	for _, ids := range groups {
		ps, err := s.Photos(ids)
		if err != nil {
			return nil, err
		}
		sortPhotos(ps)
		out = append(out, Burst{ps})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].newest() > out[j].newest() })
	return out, nil
}

func (b Burst) newest() int64 {
	var t int64
	for _, p := range b.Photos {
		t = max(t, p.TakenAt)
	}
	return t
}

func sortPhotos(ps []Photo) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Overall > ps[j-1].Overall; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

// DuplicateGroups returns sets of photos with identical content fingerprint
// or identical perceptual hash (e.g. the same image exported twice).
func (s *Store) DuplicateGroups(limit int) ([][]Photo, error) {
	rows, err := s.DB.Query(`SELECT group_concat(id) FROM photos WHERE phash != 0
		GROUP BY phash HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var groups [][]int64
	for rows.Next() {
		var csv string
		rows.Scan(&csv)
		var ids []int64
		for _, p := range strings.Split(csv, ",") {
			var id int64
			fmt.Sscan(p, &id)
			ids = append(ids, id)
		}
		groups = append(groups, ids)
	}
	rows.Close()
	var out [][]Photo
	for _, g := range groups {
		ps, err := s.Photos(g)
		if err != nil {
			return nil, err
		}
		out = append(out, ps)
	}
	return out, nil
}

// PruneResult reports what Prune removed and what it held back.
type PruneResult struct {
	Gone []string // entries removed (or that would be, in a dry run)
	Held []string // folders whose files all look missing, kept as possibly offline
}

// Prune deletes catalogue entries whose files no longer exist, but only under
// the given roots. A drive that is offline leaves its mount point missing or
// empty, which is indistinguishable from every file being deleted; so when
// all catalogued files of a folder are missing and the folder is gone or
// empty, those entries are held back unless allMissing is set.
func (s *Store) Prune(roots []string, dryRun, allMissing bool) (PruneResult, error) {
	var res PruneResult
	for _, root := range roots {
		root, _ = filepath.Abs(root)
		if _, err := os.Stat(root); err != nil && !allMissing {
			return res, fmt.Errorf("root %s is not reachable; is the drive mounted? (pass --all-missing if it was deleted)", root)
		}
		prefix := strings.TrimSuffix(root, string(filepath.Separator)) + string(filepath.Separator)
		paths, err := s.pathsUnder(prefix)
		if err != nil {
			return res, err
		}
		type folder struct {
			total   int
			missing []string
		}
		dirs := map[string]*folder{}
		for _, p := range paths {
			d := dirs[filepath.Dir(p)]
			if d == nil {
				d = &folder{}
				dirs[filepath.Dir(p)] = d
			}
			d.total++
			// only a definite "does not exist" counts; I/O errors from a
			// flaky network share keep the entry
			if _, err := os.Lstat(p); os.IsNotExist(err) {
				d.missing = append(d.missing, p)
			}
		}
		for dir, d := range dirs {
			if len(d.missing) == 0 {
				continue
			}
			if len(d.missing) == d.total && !allMissing && missingOrEmpty(dir) {
				res.Held = append(res.Held, dir)
				continue
			}
			res.Gone = append(res.Gone, d.missing...)
		}
	}
	sort.Strings(res.Gone)
	sort.Strings(res.Held)
	if dryRun || len(res.Gone) == 0 {
		return res, nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, p := range res.Gone {
		if _, err := tx.Exec(`DELETE FROM photos WHERE path = ?`, p); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}

// pathsUnder lists catalogued paths starting with prefix. SQLite's substr
// counts characters, not bytes, and macOS may store names decomposed (NFD)
// while a typed path is composed (NFC), so both spellings are tried.
func (s *Store) pathsUnder(prefix string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, pre := range []string{prefix, norm.NFC.String(prefix), norm.NFD.String(prefix)} {
		if seen["\x00"+pre] {
			continue
		}
		seen["\x00"+pre] = true
		rows, err := s.DB.Query(`SELECT path FROM photos WHERE substr(path, 1, ?) = ?`, utf8.RuneCountInString(pre), pre)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// missingOrEmpty reports whether dir is absent or has no entries at all.
func missingOrEmpty(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) == 0
}

// Errors lists recorded indexing errors.
func (s *Store) Errors(limit int) ([][2]string, error) {
	rows, err := s.DB.Query(`SELECT path, error FROM errors ORDER BY at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, b string
		rows.Scan(&a, &b)
		out = append(out, [2]string{a, b})
	}
	return out, rows.Err()
}

// TagCounts lists tags with their photo counts.
func (s *Store) TagCounts() ([]Tag, error) {
	rows, err := s.DB.Query(`SELECT tag, COUNT(*) FROM tags GROUP BY tag ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		rows.Scan(&t.Tag, &t.Score)
		out = append(out, t)
	}
	return out, rows.Err()
}

// EachChangedFace streams the current faces of photos saved after change
// number since. A photo that now has no faces is reported once with faceID 0,
// so the caller learns every changed photo.
func (s *Store) EachChangedFace(since int64, fn func(photoID, faceID int64, blob []byte)) error {
	rows, err := s.DB.Query(`SELECT p.id, COALESCE(f.id, 0), f.embedding FROM photos p
		LEFT JOIN faces f ON f.photo_id = p.id WHERE p.seq > ?`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid, fid int64
		var b []byte
		if err := rows.Scan(&pid, &fid, &b); err != nil {
			return err
		}
		fn(pid, fid, b)
	}
	return rows.Err()
}

// EachEmbeddingWithTechnical streams every photo's CLIP embedding with its
// technical score.
func (s *Store) EachEmbeddingWithTechnical(fn func(id int64, technical float64, blob []byte)) error {
	rows, err := s.DB.Query(`SELECT c.photo_id, p.technical, c.clip FROM photo_clip c JOIN photos p ON p.id = c.photo_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var t float64
		var b []byte
		if err := rows.Scan(&id, &t, &b); err != nil {
			return err
		}
		fn(id, t, b)
	}
	return rows.Err()
}

// SetScores updates aesthetic and overall scores: id -> {aesthetic, overall}.
func (s *Store) SetScores(scores map[int64][2]float64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`UPDATE photos SET aesthetic = ?, overall = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer st.Close()
	for id, v := range scores {
		if _, err := st.Exec(v[0], v[1], id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Restricts reports whether f has any condition (sorting and paging aside).
func (f Filter) Restricts() bool {
	w, _ := f.where()
	return w != ""
}

// FilterCount counts the photos matching f.
func (s *Store) FilterCount(f Filter) (int, error) {
	where, args := f.where()
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM photos`+where, args...).Scan(&n)
	return n, err
}

// FilterSet returns the ids of all photos matching f.
func (s *Store) FilterSet(f Filter) (map[int64]bool, error) {
	where, args := f.where()
	rows, err := s.DB.Query(`SELECT id FROM photos`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// likeContains is a LIKE pattern matching text anywhere, with the LIKE
// wildcards in text taken literally (use with ESCAPE '\').
func likeContains(text string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(text) + "%"
}

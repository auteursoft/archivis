package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
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
	Source        string // "" (any), "photos" (no memes or screenshots), "camera", "saved", "memes"
	From, To      int64  // unix seconds (taken_at); 0 = no bound
	Year          int    // one year; shorthand for YearFrom = YearTo = Year
	YearFrom      int    // first year taken (inclusive); 0 = no bound
	YearTo        int    // last year taken (inclusive); 0 = no bound
	Sort          string // overall|technical|aesthetic|sharpness|newest|oldest|colorful|faces|worst
	Limit         int
	Offset        int

	// Dirs limits photos to those anywhere below these folders (exact
	// stored paths, either separator). DirsByPath says the folders hold few
	// photos, so reading them through the path index and sorting beats
	// walking the sort's own index.
	Dirs       []string
	DirsByPath bool
	// SkipCount leaves BrowsePage's total at -1 (the caller knows it, as
	// Albums does from the folders table).
	SkipCount bool

	// Stacked shows each stack of near-identical burst frames as its top
	// frame alone. Only BrowsePage reads it: counts and searches are of
	// every photo.
	Stacked bool
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
	switch f.Source {
	case "photos":
		add("source != ?", SourceMeme)
	case "camera":
		add("source = ?", SourceCamera)
	case "saved":
		add("source = ?", SourceSaved)
	case "memes":
		add("source = ?", SourceMeme)
	}
	if f.From != 0 {
		add("taken_at >= ?", f.From)
	}
	if f.To != 0 {
		add("taken_at < ?", f.To)
	}
	// Folders as ranges of paths. A big folder is not read through the
	// path index (+path), but found walking the sort's own index, which
	// stops after a page instead of sorting the whole folder.
	if len(f.Dirs) > 0 {
		col := "path"
		if !f.DirsByPath {
			col = "+path"
		}
		var or []string
		for _, d := range f.Dirs {
			for _, r := range dirRanges(d) {
				or = append(or, "("+col+" >= ? AND "+col+" < ?)")
				a = append(a, r[0], r[1])
			}
		}
		w = append(w, "("+strings.Join(or, " OR ")+")")
	}
	// Years as a range of capture times, which the taken_at index serves.
	// taken_at is the camera's wall-clock time stored as if UTC, so years
	// start at UTC midnight. Undated photos (taken_at 0) never match.
	if from, to := f.years(); from != 0 || to != 0 {
		if from != 0 {
			add("taken_at >= ?", YearStart(from))
		}
		if to != 0 {
			add("taken_at < ?", YearStart(to+1))
		}
		add("taken_at != 0")
	}
	if len(w) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(w, " AND "), a
}

// years is the filter's inclusive year range (0 = no bound).
func (f Filter) years() (from, to int) {
	if f.Year != 0 {
		return f.Year, f.Year
	}
	if f.YearFrom != 0 && f.YearTo != 0 && f.YearFrom > f.YearTo {
		return f.YearTo, f.YearFrom // reversed: every caller gets the range meant
	}
	return f.YearFrom, f.YearTo
}

// YearStart is the first second of year y as a taken_at value.
func YearStart(y int) int64 { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Unix() }

// Years lists the years that have dated photos, oldest first. It jumps from
// year to year through the taken_at index, so it reads one row per year, not
// one per photo.
func (s *Store) Years() ([]int, error) {
	var out []int
	cursor := YearStart(1800)
	for i := 0; i < 500; i++ {
		var t sql.NullInt64
		if err := s.DB.QueryRow(`SELECT MIN(taken_at) FROM photos WHERE taken_at >= ? AND taken_at != 0`, cursor).Scan(&t); err != nil {
			return nil, err
		}
		if !t.Valid {
			break
		}
		y := time.Unix(t.Int64, 0).UTC().Year()
		if y > time.Now().Year()+1 {
			break // clock errors in the far future
		}
		out = append(out, y)
		cursor = YearStart(y + 1)
	}
	return out, nil
}

// dirRanges are the path ranges holding everything below folder d: after
// d+sep and before d+(sep+1), for each separator (just "/" in a Unix
// path). They scan the path index.
func dirRanges(d string) [][2]string {
	d = TrimDir(d)
	if d == "/" {
		d = ""
	}
	r := [][2]string{{d + "/", d + "0"}}
	if seps(d) != "/" && d != "" {
		r = append(r, [2]string{d + `\`, d + "]"})
	}
	return r
}

func (f Filter) order() string {
	if len(f.Dirs) > 0 && f.DirsByPath {
		// "+col" keeps SQLite off the sort column's index: the path ranges
		// pick the (few) rows, then they are sorted
		o := f.orderBy()
		return strings.NewReplacer(" BY ", " BY +", ", ", ", +").Replace(o)
	}
	return f.orderBy()
}

func (f Filter) orderBy() string {
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
	ps, total, _, err := s.BrowsePage(f)
	return ps, total, err
}

// BrowsePage is Browse, also saying whether more photos follow the page.
// With f.Stacked, a frame hidden in a stack is left out, unless the stack's
// top does not itself match the filter (then the frame shows for it). The
// total stays the count of every matching photo: counting what stacking
// leaves would take a full scan.
func (s *Store) BrowsePage(f Filter) ([]Photo, int, bool, error) {
	where, args := f.where()
	total := -1
	if !f.SkipCount {
		var err error
		if total, err = s.FilterCount(f); err != nil {
			return nil, 0, false, err
		}
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	pageWhere, pageArgs := where, args
	if f.Stacked {
		hide := `stacked = 0`
		if where != "" {
			// hidden, unless its top is filtered out: the subquery's own
			// columns (t) are what the filter's names refer to inside it
			hide = `(stacked = 0 OR NOT EXISTS (SELECT 1 FROM photos t` + where + ` AND t.id = photos.stack_top))`
			pageArgs = append(append([]any{}, args...), args...)
		}
		if pageWhere == "" {
			pageWhere = " WHERE " + hide
		} else {
			pageWhere += " AND " + hide
		}
	}
	q := `SELECT ` + readCols + ` FROM photos` + pageWhere + f.order() + ` LIMIT ? OFFSET ?`
	ps, err := s.queryPhotos(q, append(pageArgs, f.Limit+1, f.Offset)...)
	more := len(ps) > f.Limit
	if more {
		ps = ps[:f.Limit]
	}
	return ps, total, more, err
}

// StackMembers returns the frames of the stack whose top is top, best
// first (the top among them), or nil if it is not a stack's top.
func (s *Store) StackMembers(top int64) ([]Photo, error) {
	ps, err := s.queryPhotos(`SELECT `+readCols+` FROM photos WHERE (stacked = 1 AND stack_top = ?) OR (id = ? AND stack_top = ?)`, top, top, top)
	if err != nil || len(ps) < 2 {
		return nil, err
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Overall != ps[j].Overall {
			return ps[i].Overall > ps[j].Overall
		}
		return ps[i].ID < ps[j].ID
	})
	return ps, nil
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
	if len(scores) > 0 {
		if err := markGroupsDirty(tx); err != nil { // groups rank by these scores
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

// Narrows reports whether f has a condition beyond leaving out memes and
// screenshots, which leaves nearly every photo: such a filter is applied to
// candidates found without it, never by listing everything it matches.
func (f Filter) Narrows() bool {
	if f.Source == "photos" {
		f.Source = ""
	}
	return f.Restricts()
}

// FilterCount counts the photos matching f. Leaving out memes, alone or
// with folders, is counted as all the photos (there) less the memes (an
// index of memes alone), not by reading every photo.
func (s *Store) FilterCount(f Filter) (int, error) {
	var n int
	if g := f; f.Source == "photos" {
		g.Source, g.Dirs = "", nil
		if !g.Restricts() {
			// one statement, so one snapshot: a backfill changing sources
			// between two reads cannot skew the difference
			g.Dirs, g.DirsByPath = f.Dirs, true
			where, args := g.where()
			mq, margs := memeCountSQL(f.Dirs)
			err := s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM photos`+where+`) - (`+mq+`)`, append(args, margs...)...).Scan(&n)
			return max(0, n), err
		}
	}
	where, args := f.where()
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

package store

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
)

// walkShare: each group must hold at least 1/walkShare of the catalogue for
// walking the sort's index to find every group's best in a few rows.
const walkShare = 200

// BestMatches finds, for each group of folders (an album card: one folder,
// or every Photos library), its best photo matching f in f's sort order,
// or nil if none matches; sizes[i] is about how many photos group i holds,
// and less orders photos as f's sort does.
//
// With no filter narrowing the photos and a sort that has an index, it
// walks that index once, stopping when every group has its first (best)
// photo, which takes a few rows. Otherwise it reads the matching photos
// once, in whatever order is quickest (through a filter's own index, if
// any), keeping each group's best: one pass, as counting the matches for
// the Photos page takes. That pass is done in SQL, unless the groups nest
// (a photo in two of them), when it is done here with less.
func (s *Store) BestMatches(f Filter, groups [][]string, sizes []int, less func(a, b Photo) bool) ([]*Photo, error) {
	out := make([]*Photo, len(groups))
	if len(groups) == 0 {
		return out, nil
	}
	smallest, sum := math.MaxInt, 0
	for _, n := range sizes {
		smallest, sum = min(smallest, n), sum+n
	}
	switch f.Sort {
	case "", "overall", "worst", "newest":
		if f.Narrows() {
			break
		}
		// the walk meets a group's photos about once in every total/size
		// rows: worth it for the groups that are a good share of them all;
		// the others (few photos) are read through the path index
		total := int(s.Count(false))
		if smallest*walkShare >= total {
			return s.bestByWalk(f, groups)
		}
		var big, small []int
		for i, n := range sizes {
			if n*walkShare >= total {
				big = append(big, i)
			} else {
				small = append(small, i)
			}
		}
		if len(big) == 0 {
			break
		}
		pick := func(idx []int) [][]string {
			var out [][]string
			for _, i := range idx {
				out = append(out, groups[i])
			}
			return out
		}
		bigBest, err := s.bestByWalk(f, pick(big))
		if err != nil {
			return nil, err
		}
		var smallBest []*Photo
		smallGroups := pick(small)
		if parent, ok := siblings(smallGroups); ok { // an album's subfolders, however many: one read of the album
			smallBest, err = s.bestByChild(f, smallGroups, parent, sum < 200000)
		} else if nested(smallGroups) {
			smallBest, err = s.bestByScan(f, smallGroups, less, false)
		} else {
			smallBest, err = s.bestByGroup(f, smallGroups, true)
		}
		if err != nil {
			return nil, err
		}
		for k, i := range big {
			out[i] = bigBest[k]
		}
		for k, i := range small {
			out[i] = smallBest[k]
		}
		return out, nil
	}
	if compound(f.Sort) { // a second key on ties: compared here, by less
		return s.bestByScan(f, groups, less, sum < 200000)
	}
	if parent, ok := siblings(groups); ok {
		return s.bestByChild(f, groups, parent, sum < 200000)
	}
	if !nested(groups) {
		return s.bestByGroup(f, groups, sum < 200000)
	}
	return s.bestByScan(f, groups, less, sum < 200000)
}

// maxQueryGroups bounds the groups one query names by their folders: each
// folder adds a path range to an OR, and SQLite refuses an expression
// deeper than 1000.
const maxQueryGroups = 200

// inChunks runs best over the groups at most maxQueryGroups at a time: a
// group's photos all lie in its own folders, so its best is found within
// its chunk.
func inChunks(groups [][]string, best func([][]string) ([]*Photo, error)) ([]*Photo, error) {
	var out []*Photo
	for start := 0; start < len(groups); start += maxQueryGroups {
		b, err := best(groups[start:min(start+maxQueryGroups, len(groups))])
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

// nested reports whether a folder of one group lies in another group's.
func nested(groups [][]string) bool {
	for i, a := range groups {
		for j, b := range groups {
			if i == j {
				continue
			}
			for _, x := range a {
				for _, y := range b {
					if InDir(x, y) {
						return true
					}
				}
			}
		}
	}
	return false
}

// siblings reports whether every group is one folder, all in the same
// (Unix-style) parent, each once: an album's subfolders. The same folder
// twice (an Apple Photos library that is also an indexed root, as its own
// card and the iPhone card) is not: each photo must reach both.
func siblings(groups [][]string) (string, bool) {
	parent := ""
	seen := map[string]bool{}
	for _, ds := range groups {
		if len(ds) != 1 || !strings.HasPrefix(ds[0], "/") {
			return "", false
		}
		d := TrimDir(ds[0])
		p := ParentDir(d)
		if p == "" || (parent != "" && p != parent) || seen[d] {
			return "", false
		}
		parent, seen[d] = p, true
	}
	return parent, parent != ""
}

// bestByChild is bestByGroup for an album's subfolders: one read of the
// album's photos (through the path index, unless they are many), each labelled with the
// subfolder its path continues into, so the work does not grow with the
// number of subfolders.
func (s *Store) bestByChild(f Filter, groups [][]string, parent string, byPath bool) ([]*Photo, error) {
	prefix := strings.TrimSuffix(parent, "/") + "/"
	g := f
	g.Dirs, g.DirsByPath = []string{parent}, byPath // few photos: through the path index; many: a scan of them all
	where, args := g.where()
	key, isMax := bestKey(f.Sort)
	agg := "MIN"
	if isMax {
		agg = "MAX"
	}
	// byte-wise: the path as a blob, past the parent and its separator
	rest := fmt.Sprintf("substr(CAST(path AS BLOB), %d)", len(prefix)+1)
	rows, err := s.DB.Query(`SELECT c, id, `+agg+`(k) FROM (SELECT CASE WHEN instr(`+rest+`, CAST('/' AS BLOB)) > 0
		THEN substr(`+rest+`, 1, instr(`+rest+`, CAST('/' AS BLOB)) - 1) END AS c, id, `+key+` AS k FROM photos`+where+
		`) WHERE c IS NOT NULL AND k IS NOT NULL GROUP BY c`, args...)
	if err != nil {
		return nil, err
	}
	at := map[string]int{}
	for i, ds := range groups {
		at[TrimDir(ds[0])] = i
	}
	ids := make([]int64, len(groups))
	for rows.Next() {
		var c []byte
		var id int64
		var k any
		if err := rows.Scan(&c, &id, &k); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := at[prefix+string(c)]; ok {
			ids[i] = id
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return s.photosFor(ids)
}

// compound reports whether a sort orders by a second key when the first
// ties (see Filter.orderBy), which min() or max() of one column cannot.
func compound(sort string) bool {
	return sort == "sharpness" || sort == "faces"
}

// bestKey is, for each one-key sort, the column whose largest (max) or
// smallest value marks a group's best photo. An unscored photo counts as
// bestByScan and the Photos page's comparator count it (no aesthetic score:
// below any, as it sorts last), so it is never left out.
func bestKey(sort string) (expr string, max bool) {
	switch sort {
	case "worst":
		return "COALESCE(overall, 0)", false
	case "technical":
		return "COALESCE(technical, 0)", true
	case "aesthetic":
		return "COALESCE(aesthetic, -1)", true
	case "colorful":
		return "COALESCE(colorfulness, 0)", true
	case "newest":
		return "taken_at", true
	case "oldest":
		// as Filter.orderBy: dated from 1970 on, then before 1970, then
		// undated; exact integers, each range above the one before
		return "CASE WHEN taken_at > 0 THEN taken_at WHEN taken_at < 0 THEN 4611686018427387904 + taken_at ELSE 9223372036854775807 END", false
	case "path":
		return "path", false
	}
	return "COALESCE(overall, 0)", true
}

// bestByGroup reads the matching photos once in SQL, each labelled with
// its group (groups that do not nest: one each), keeping each group's best
// by its sort's key (a row SQLite picks with min() or max()).
func (s *Store) bestByGroup(f Filter, groups [][]string, byPath bool) ([]*Photo, error) {
	if len(groups) > maxQueryGroups {
		return inChunks(groups, func(c [][]string) ([]*Photo, error) { return s.bestByGroup(f, c, byPath) })
	}
	g := f
	g.Dirs, g.DirsByPath = nil, true
	var label []string
	var largs []any
	for i, ds := range groups {
		if byPath { // few photos: read through the path index; else the labels pick them from a scan
			g.Dirs = append(g.Dirs, ds...)
		}
		for _, d := range ds {
			for _, r := range dirRanges(d) {
				label = append(label, fmt.Sprintf("WHEN path >= ? AND path < ? THEN %d", i))
				largs = append(largs, r[0], r[1])
			}
		}
	}
	where, args := g.where()
	key, isMax := bestKey(f.Sort)
	agg := "MIN"
	if isMax {
		agg = "MAX"
	}
	rows, err := s.DB.Query(`SELECT g, id, `+agg+`(k) FROM (SELECT CASE `+strings.Join(label, " ")+` END AS g, id, `+key+` AS k FROM photos`+where+
		`) WHERE g IS NOT NULL AND k IS NOT NULL GROUP BY g`, append(largs, args...)...)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(groups))
	for rows.Next() {
		var gi int
		var id int64
		var k any
		if err := rows.Scan(&gi, &id, &k); err != nil {
			rows.Close()
			return nil, err
		}
		if gi >= 0 && gi < len(ids) {
			ids[gi] = id
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return s.photosFor(ids)
}

// groupIndex finds the groups holding a path, remembering the answer for
// each folder (photos come many to a folder).
type groupIndex struct {
	groups [][]string
	byDir  map[string][]int
}

func newGroupIndex(groups [][]string) *groupIndex {
	return &groupIndex{groups: groups, byDir: map[string][]int{}}
}

// of lists the groups holding path.
func (g *groupIndex) of(path string) []int {
	dir := ParentDir(path)
	if in, ok := g.byDir[dir]; ok {
		return in
	}
	var in []int
	for i, ds := range g.groups {
		for _, d := range ds {
			if InDir(path, d) {
				in = append(in, i)
				break
			}
		}
	}
	if len(g.byDir) < 1<<20 { // bounded: a pathological catalogue of one folder per photo
		g.byDir[dir] = in
	}
	return in
}

// bestByScan reads the matching photos' sort keys once (through the path
// index, with byPath: few photos), keeping each group's best by less: an
// album's subfolders, through the album's own range; other groups, by
// their folders, in chunks.
func (s *Store) bestByScan(f Filter, groups [][]string, less func(a, b Photo) bool, byPath bool) ([]*Photo, error) {
	g := f
	g.Dirs, g.DirsByPath = nil, byPath
	if parent, ok := siblings(groups); ok {
		g.Dirs = []string{parent}
	} else if len(groups) > maxQueryGroups {
		return inChunks(groups, func(c [][]string) ([]*Photo, error) { return s.bestByScan(f, c, less, byPath) })
	} else {
		for _, ds := range groups {
			g.Dirs = append(g.Dirs, ds...)
		}
	}
	where, args := g.where()
	rows, err := s.DB.Query(`SELECT id, path, COALESCE(overall, 0), COALESCE(technical, 0), aesthetic, COALESCE(focus_score, 0), COALESCE(sharpness, 0),
		taken_at, COALESCE(colorfulness, 0), face_count FROM photos`+where, args...)
	if err != nil {
		return nil, err
	}
	best := make([]*Photo, len(groups))
	gi := newGroupIndex(groups)
	for rows.Next() {
		var p Photo
		var aes sql.NullFloat64
		if err := rows.Scan(&p.ID, &p.Path, &p.Overall, &p.Technical, &aes, &p.FocusScore, &p.Sharpness, &p.TakenAt, &p.Colorfulness, &p.FaceCount); err != nil {
			rows.Close()
			return nil, err
		}
		p.Aesthetic = aes
		for _, i := range gi.of(p.Path) {
			if b := best[i]; b == nil || less(p, *b) || (!less(*b, p) && p.ID < b.ID) {
				q := p
				best[i] = &q
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(groups))
	for i, b := range best {
		if b != nil {
			ids[i] = b.ID
		}
	}
	return s.photosFor(ids)
}

// bestByWalk walks f's sort order (by its index), each group's first
// photo met its best, until every group has one.
func (s *Store) bestByWalk(f Filter, groups [][]string) ([]*Photo, error) {
	g := f
	g.Dirs, g.DirsByPath = nil, false
	for _, ds := range groups {
		g.Dirs = append(g.Dirs, ds...)
	}
	where, args := g.where() // the path ranges as +path: the sort's own index is walked
	rows, err := s.DB.Query(`SELECT id, path FROM photos`+where+g.order(), args...)
	if err != nil {
		return nil, err
	}
	best := make([]int64, len(groups))
	open := len(groups)
	gi := newGroupIndex(groups)
	for open > 0 && rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return nil, err
		}
		for _, i := range gi.of(path) {
			if best[i] == 0 {
				best[i] = id
				open--
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return s.photosFor(best)
}

// photosFor loads the photos by id, in place (0: none).
func (s *Store) photosFor(ids []int64) ([]*Photo, error) {
	var want []int64
	for _, id := range ids {
		if id != 0 {
			want = append(want, id)
		}
	}
	ps, err := s.Photos(want)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*Photo{}
	for i := range ps {
		byID[ps[i].ID] = &ps[i]
	}
	out := make([]*Photo, len(ids))
	for i, id := range ids {
		out[i] = byID[id]
	}
	return out, nil
}

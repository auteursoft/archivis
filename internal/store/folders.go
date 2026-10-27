package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Folders: every folder holding photos (at any depth below it), with how
// many and the best of them, worked out from the catalogue's paths by
// RebuildFolders, for the Albums pages. Roots are the folders indexed.

// memeCover is taken off a meme's or screenshot's score as a cover, below
// any photo's (scores are 0-100).
const memeCover = 1000

// foldersVersion changes when the rules do, forcing a rebuild.
const foldersVersion = "5"

// Folder is a folder holding photos somewhere below it.
type Folder struct {
	Path         string
	Parent       string // "" at the top
	Total        int    // photos anywhere below
	CoverID      int64  // the best of them (highest overall, then lowest id; a meme only if all are)
	CoverOverall float64
	NewestAt     int64   // the latest taken_at below (0: none dated, as taken_at)
	Scored       int     // photos below with an overall score
	ScoreSum     float64 // their scores added up
	Memes        int     // photos below that are memes or screenshots (pages leave them out by default)
}

// AvgScore is the mean overall score of the photos below that have one
// (false if none has).
func (f Folder) AvgScore() (float64, bool) {
	if f.Scored == 0 {
		return 0, false
	}
	return f.ScoreSum / float64(f.Scored), true
}

// Add folds another folder's photos into f's figures (keeping f's path).
func (f *Folder) Add(o Folder) {
	f.Total += o.Total
	if o.CoverOverall > f.CoverOverall || (o.CoverOverall == f.CoverOverall && o.CoverID < f.CoverID) {
		f.CoverID, f.CoverOverall = o.CoverID, o.CoverOverall
	}
	if o.NewestAt != 0 && (f.NewestAt == 0 || o.NewestAt > f.NewestAt) { // 0 is undated; before 1970 is not
		f.NewestAt = o.NewestAt
	}
	f.Scored += o.Scored
	f.ScoreSum += o.ScoreSum
	f.Memes += o.Memes
}

// Name is the folder's last path element.
func (f Folder) Name() string {
	if i := strings.LastIndexAny(f.Path, seps(f.Path)); i >= 0 && i < len(f.Path)-1 {
		return f.Path[i+1:]
	}
	return f.Path
}

// seps are the separators in path p: "/" in a Unix path (where a name may
// hold a backslash), either in a Windows one.
func seps(p string) string {
	if strings.HasPrefix(p, "/") {
		return "/"
	}
	return `/\`
}

// TrimDir drops a folder path's trailing separators ("/" stays "/").
func TrimDir(p string) string {
	if t := strings.TrimRight(p, seps(p)); t != "" || p == "" {
		return t
	}
	return "/"
}

// ParentDir is the folder holding p: "/" for /pics, and "" for "/" and
// the top of a Windows path ("C:").
func ParentDir(p string) string {
	i := strings.LastIndexAny(p, seps(p))
	switch {
	case p == "/" || i < 0:
		return ""
	case i == 0:
		if seps(p) == "/" {
			return "/"
		}
		return ""
	}
	return p[:i]
}

// InDir reports whether p is folder dir or below it.
func InDir(p, dir string) bool {
	dir = TrimDir(dir)
	if dir == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == dir || (len(p) > len(dir) && strings.HasPrefix(p, dir) && strings.IndexByte(seps(dir), p[len(dir)]) >= 0)
}

// RecordRoots notes folders as indexed roots (absolute paths), shown as
// the top of Albums.
func (s *Store) RecordRoots(paths []string) error {
	for _, p := range paths {
		p = rootPath(p)
		if _, err := s.DB.Exec(`INSERT OR IGNORE INTO roots (path, added_at) VALUES (?, ?)`, p, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// rootPath is how a root is recorded: absolute, no trailing separator.
func rootPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return TrimDir(p)
}

// RemoveRoot forgets a root (its photos stay catalogued). It reports
// whether it was recorded.
func (s *Store) RemoveRoot(path string) (bool, error) {
	r, err := s.DB.Exec(`DELETE FROM roots WHERE path = ?`, rootPath(path))
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// Roots lists the recorded roots, by path.
func (s *Store) Roots() ([]string, error) {
	return s.strings(`SELECT path FROM roots ORDER BY path`)
}

// IPhoneAlbum is the album SetAlbumName and AlbumNames call the Apple
// Photos libraries' (a root's path is absolute, so never this).
const IPhoneAlbum = "iphone"

// MaxAlbumName is the longest display name, in bytes.
const MaxAlbumName = 200

// AlbumNames returns the display names set for the top-level albums: by
// root path, and the iPhone album's under IPhoneAlbum.
func (s *Store) AlbumNames() (map[string]string, error) {
	rows, err := s.DB.Query(`SELECT path, name FROM roots WHERE name != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, n string
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out[p] = n
	}
	if n := s.GetMeta("album_name_iphone"); n != "" {
		out[IPhoneAlbum] = n
	}
	return out, rows.Err()
}

// SetAlbumName gives a top-level album (a recorded root, or IPhoneAlbum)
// a display name; "" goes back to the folder's own. It reports whether the
// album exists.
func (s *Store) SetAlbumName(album, name string) (bool, error) {
	name = strings.Join(strings.Fields(name), " ") // one line, no stray spaces
	if len(name) > MaxAlbumName {
		return false, fmt.Errorf("an album name may be at most %d bytes", MaxAlbumName)
	}
	if album == IPhoneAlbum {
		return true, s.SetMeta("album_name_iphone", name)
	}
	// as recorded (the web pages send the stored path), else as typed
	for _, p := range []string{TrimDir(album), rootPath(album)} {
		r, err := s.DB.Exec(`UPDATE roots SET name = ? WHERE path = ?`, name, p)
		if err != nil {
			return false, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) strings(q string, args ...any) ([]string, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) folders(q string, args ...any) ([]Folder, error) {
	rows, err := s.DB.Query(`SELECT path, parent, total, cover_id, cover_overall, newest_at, scored, score_sum, memes FROM folders `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.Path, &f.Parent, &f.Total, &f.CoverID, &f.CoverOverall, &f.NewestAt, &f.Scored, &f.ScoreSum, &f.Memes); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Folder returns the folder at path (nil if it holds no photos).
func (s *Store) Folder(path string) (*Folder, error) {
	fs, err := s.folders(`WHERE path = ?`, TrimDir(path))
	if err != nil || len(fs) == 0 {
		return nil, err
	}
	return &fs[0], nil
}

// Subfolders lists the folders directly inside path, by name.
func (s *Store) Subfolders(path string) ([]Folder, error) {
	return s.folders(`WHERE parent = ? ORDER BY path`, TrimDir(path))
}

// PhotosLibraries lists the Apple Photos libraries (*.photoslibrary
// folders, not counting one inside another) that hold photos, by path, as
// the last RebuildFolders found them.
func (s *Store) PhotosLibraries() ([]Folder, error) {
	var paths []string
	if v := s.GetMeta("folders_libraries"); v != "" {
		if err := json.Unmarshal([]byte(v), &paths); err != nil {
			return nil, err
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	args := make([]any, len(paths))
	for i, p := range paths {
		args[i] = p
	}
	return s.folders(`WHERE path IN (`+placeholders(len(paths))+`) ORDER BY path`, args...)
}

func isLibrary(p string) bool {
	const ext = ".photoslibrary"
	return len(p) > len(ext) && strings.EqualFold(p[len(p)-len(ext):], ext)
}

// outermost keeps the libraries not inside another of them, by path.
func outermost(libs []string) []string {
	in := map[string]bool{}
	for _, l := range libs {
		in[l] = true
	}
	var out []string
	for _, l := range libs {
		nested := false
		for d := ParentDir(l); d != "" && !nested; d = ParentDir(d) {
			nested = in[d]
		}
		if !nested {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// windowsSlashes reports whether any Windows-style path (not starting with
// "/"; looked up by the index) holds a "/". The indexer writes only "\"
// there, so this is normally false.
func (s *Store) windowsSlashes() (bool, error) {
	var any bool
	err := s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM photos WHERE (path < '/' OR path >= '0') AND instr(path, '/') > 0)`).Scan(&any)
	return any, err
}

// foldersDirty changes whenever photos are deleted or moved.
func (s *Store) foldersDirty() string {
	return s.GetMeta("groups_dirty") + "/" + s.GetMeta("folders_dirty")
}

// FoldersStale reports whether the folders may be out of date: photos
// added, changed, moved or deleted since, or never worked out.
func (s *Store) FoldersStale() bool {
	if s.GetMeta("folders_version") != foldersVersion || s.foldersDirty() != s.GetMeta("folders_clean") {
		return true
	}
	seq, err := strconv.ParseInt(s.GetMeta("folders_seq"), 10, 64)
	return err != nil || s.MaxSeq() > seq
}

// FolderStats reports what RebuildFolders did.
type FolderStats struct {
	Folders, Writes int
	Held            int // the most folders held in memory at once
	Took            time.Duration
}

// RebuildFolders works out every folder's count, best photo, newest date
// and scores from the
// catalogue's paths, writing only what changed, in short transactions.
// Photos are read in path order, where everything below a folder comes
// together, so each folder is finished as the scan leaves it; memory holds
// only the open folders and a batch of finished ones, however many there are.
func (s *Store) RebuildFolders(ctx context.Context) (FolderStats, error) {
	start := time.Now()
	var st FolderStats
	seq := s.MaxSeq()
	dirty := s.foldersDirty()
	if err := s.SetMeta("folders_version", ""); err != nil { // stale until finished
		return st, err
	}
	w := &batchWriter{s: s}
	// whether any photo lies in a path range: one indexed look
	holds, err := s.DB.PrepareContext(ctx, `SELECT EXISTS(SELECT 1 FROM photos WHERE path >= ? AND path < ?)`)
	if err != nil {
		return st, err
	}
	defer holds.Close()
	has := func(r [2]string) (bool, error) {
		var any bool
		err := holds.QueryRowContext(ctx, r[0], r[1]).Scan(&any)
		return any, err
	}
	// A Windows folder with photos below it through both separators
	// (C:\a\x and C:\a/z, with C:\a2\y sorting between) is split apart in
	// path order: only such folders are held to the end, their parts added
	// up. Without a "/" in any Windows path (as the indexer writes them)
	// there are none, and no folder is looked at.
	slashes, err := s.windowsSlashes()
	if err != nil {
		return st, err
	}
	split := func(dir string) (bool, error) {
		r := dirRanges(dir) // "/" range, then "\" range
		if !slashes || len(r) < 2 {
			return false, nil
		}
		for _, x := range r {
			if any, err := has(x); err != nil || !any {
				return false, err
			}
		}
		return true, nil
	}

	// the open folders: the chain of the current photo's folders, top first
	type open struct {
		f Folder
	}
	var chain []open
	var pending []Folder // finished folders, to compare with the stored ones
	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		old := map[string]Folder{}
		for start := 0; start < len(pending); start += 500 {
			chunk := pending[start:min(len(pending), start+500)]
			args := make([]any, len(chunk))
			for i, f := range chunk {
				args[i] = f.Path
			}
			if err := w.commit(); err != nil {
				return err
			}
			fs, err := s.folders(`WHERE path IN (`+placeholders(len(chunk))+`)`, args...)
			if err != nil {
				return err
			}
			for _, f := range fs {
				old[f.Path] = f
			}
		}
		for _, f := range pending {
			if o, ok := old[f.Path]; ok && o == f {
				continue
			}
			if err := w.exec(`INSERT OR REPLACE INTO folders (path, parent, total, cover_id, cover_overall, newest_at, scored, score_sum, memes)
				VALUES (?,?,?,?,?,?,?,?,?)`, f.Path, f.Parent, f.Total, f.CoverID, f.CoverOverall, f.NewestAt, f.Scored, f.ScoreSum, f.Memes); err != nil {
				return err
			}
			st.Writes++
		}
		pending = pending[:0]
		return nil
	}
	win := map[string]Folder{} // split Windows folders, held to the end
	held := func() {
		st.Held = max(st.Held, len(chain)+len(pending)+len(win))
	}
	var libs []string             // Apple Photos libraries
	finish := func(n int) error { // close the deepest n open folders
		for ; n > 0; n-- {
			f := chain[len(chain)-1].f
			chain = chain[:len(chain)-1]
			if isLibrary(f.Path) && (len(libs) == 0 || libs[len(libs)-1] != f.Path) {
				libs = append(libs, f.Path)
			}
			isSplit, err := split(f.Path)
			if err != nil {
				return err
			}
			if isSplit {
				if o, ok := win[f.Path]; ok {
					f.Add(o)
				}
				win[f.Path] = f
				held()
				continue
			}
			pending = append(pending, f)
			st.Folders++
		}
		held()
		if len(pending) >= 2000 {
			return flushPending()
		}
		return nil
	}

	after := ""
	for {
		if err := ctx.Err(); err != nil {
			w.rollback()
			return st, err
		}
		if err := w.commit(); err != nil {
			return st, err
		}
		rows, err := s.DB.Query(`SELECT path, id, overall, COALESCE(taken_at, 0), source FROM photos WHERE path > ? ORDER BY path LIMIT ?`, after, scanPage)
		if err != nil {
			return st, err
		}
		type ph struct {
			path    string
			id      int64
			overall sql.NullFloat64
			at      int64
			source  int
		}
		var batch []ph
		for rows.Next() {
			var p ph
			if err := rows.Scan(&p.path, &p.id, &p.overall, &p.at, &p.source); err != nil {
				rows.Close()
				return st, err
			}
			batch = append(batch, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return st, err
		}
		for _, p := range batch {
			after = p.path
			// the photo's folders, top first
			var dirs []string
			for d := ParentDir(p.path); d != ""; d = ParentDir(d) {
				dirs = append(dirs, d)
			}
			for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
				dirs[i], dirs[j] = dirs[j], dirs[i]
			}
			// close the open folders this photo is not in
			same := 0
			for same < len(chain) && same < len(dirs) && chain[same].f.Path == dirs[same] {
				same++
			}
			if err := finish(len(chain) - same); err != nil {
				return st, err
			}
			for _, d := range dirs[same:] {
				chain = append(chain, open{Folder{Path: d, Parent: ParentDir(d), CoverOverall: math.Inf(-1)}})
			}
			held()
			one := Folder{Total: 1, CoverID: p.id, CoverOverall: p.overall.Float64, NewestAt: p.at} // unscored counts as 0 for the cover, as before
			if p.source == SourceMeme {
				one.CoverOverall -= memeCover // pages leave memes out: one is a cover only when nothing else is below
				one.Memes = 1
			}
			if p.overall.Valid {
				one.Scored, one.ScoreSum = 1, p.overall.Float64
			}
			for i := range chain {
				chain[i].f.Add(one)
			}
		}
		if len(batch) < scanPage {
			break
		}
	}
	if err := finish(len(chain)); err != nil {
		return st, err
	}
	for _, f := range win {
		pending = append(pending, f)
		st.Folders++
	}
	if err := flushPending(); err != nil {
		return st, err
	}
	// folders no longer holding photos: an indexed look below each
	empty := func(dir string) (bool, error) {
		for _, r := range dirRanges(dir) {
			if any, err := has(r); err != nil || any {
				return false, err
			}
		}
		return true, nil
	}
	at := ""
	for {
		if err := ctx.Err(); err != nil {
			w.rollback()
			return st, err
		}
		if err := w.commit(); err != nil {
			return st, err
		}
		paths, err := s.strings(`SELECT path FROM folders WHERE path > ? ORDER BY path LIMIT ?`, at, scanPage)
		if err != nil {
			return st, err
		}
		for _, p := range paths {
			at = p
			if gone, err := empty(p); err != nil {
				return st, err
			} else if !gone {
				continue
			}
			if err := w.exec(`DELETE FROM folders WHERE path = ?`, p); err != nil {
				return st, err
			}
			st.Writes++
		}
		if len(paths) < scanPage {
			break
		}
	}
	if err := w.commit(); err != nil {
		return st, err
	}
	lj, err := json.Marshal(outermost(slices.Compact(slices.Sorted(slices.Values(libs)))))
	if err != nil {
		return st, err
	}
	// the version last: until it is written, the folders read as stale
	for _, kv := range [][2]string{{"folders_libraries", string(lj)}, {"folders_seq", strconv.FormatInt(seq, 10)}, {"folders_clean", dirty}, {"folders_version", foldersVersion}} {
		if err := s.SetMeta(kv[0], kv[1]); err != nil {
			return st, err
		}
	}
	st.Took = time.Since(start)
	return st, nil
}

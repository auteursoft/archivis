package store

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Folders: every folder holding photos (at any depth below it), with how
// many and the best of them, worked out from the catalogue's paths by
// RebuildFolders, for the Albums pages. Roots are the folders indexed.

// foldersVersion changes when the rules do, forcing a rebuild.
const foldersVersion = "1"

// Folder is a folder holding photos somewhere below it.
type Folder struct {
	Path         string
	Parent       string // "" at the top
	Total        int    // photos anywhere below
	CoverID      int64  // the best of them (highest overall, then lowest id)
	CoverOverall float64
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

// ParentDir is the folder holding p ("" at the top: below "/" or "C:").
func ParentDir(p string) string {
	i := strings.LastIndexAny(p, seps(p))
	if i <= 0 {
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
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		p = TrimDir(p)
		if _, err := s.DB.Exec(`INSERT OR IGNORE INTO roots (path, added_at) VALUES (?, ?)`, p, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// RemoveRoot forgets a root (its photos stay catalogued). It reports
// whether it was recorded.
func (s *Store) RemoveRoot(path string) (bool, error) {
	r, err := s.DB.Exec(`DELETE FROM roots WHERE path = ?`, TrimDir(path))
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
	rows, err := s.DB.Query(`SELECT path, parent, total, cover_id, cover_overall FROM folders `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.Path, &f.Parent, &f.Total, &f.CoverID, &f.CoverOverall); err != nil {
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
// folders) that hold photos, by path.
func (s *Store) PhotosLibraries() ([]Folder, error) {
	fs, err := s.folders(`WHERE path LIKE '%.photoslibrary' ORDER BY path`)
	if err != nil {
		return nil, err
	}
	// a library inside another library's folder would count twice
	var out []Folder
	for _, f := range fs {
		if len(out) > 0 && InDir(f.Path, out[len(out)-1].Path) {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// FoldersStale reports whether the folders may be out of date: photos
// added, changed or deleted since, or never worked out.
func (s *Store) FoldersStale() bool {
	if s.GetMeta("folders_version") != foldersVersion || s.GetMeta("groups_dirty") != s.GetMeta("folders_clean") {
		return true
	}
	seq, err := strconv.ParseInt(s.GetMeta("folders_seq"), 10, 64)
	return err != nil || s.MaxSeq() > seq
}

// FolderStats reports what RebuildFolders did.
type FolderStats struct {
	Folders, Writes int
	Took            time.Duration
}

// RebuildFolders works out every folder's count and best photo from the
// catalogue's paths, writing only what changed, in short transactions.
// Photos are read in path order, where everything below a folder comes
// together, so each folder is finished as the scan leaves it.
func (s *Store) RebuildFolders(ctx context.Context) (FolderStats, error) {
	start := time.Now()
	var st FolderStats
	seq := s.MaxSeq()
	dirty := s.GetMeta("groups_dirty")
	if err := s.SetMeta("folders_version", ""); err != nil { // stale until finished
		return st, err
	}
	w := &batchWriter{s: s}
	var keep []string // every folder found, in path order

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
			if err := w.exec(`INSERT OR REPLACE INTO folders (path, parent, total, cover_id, cover_overall) VALUES (?,?,?,?,?)`,
				f.Path, f.Parent, f.Total, f.CoverID, f.CoverOverall); err != nil {
				return err
			}
			st.Writes++
		}
		pending = pending[:0]
		return nil
	}
	// Windows folders, kept to the end: with both separators allowed, a
	// folder's photos needn't come together (C:\a\x, C:\a2\y, C:\a/z), so
	// its parts are added up here.
	win := map[string]Folder{}
	finish := func(n int) error { // close the deepest n open folders
		for ; n > 0; n-- {
			f := chain[len(chain)-1].f
			chain = chain[:len(chain)-1]
			if seps(f.Path) != "/" {
				if o, ok := win[f.Path]; ok {
					f.Total += o.Total
					if o.CoverOverall > f.CoverOverall || (o.CoverOverall == f.CoverOverall && o.CoverID < f.CoverID) {
						f.CoverID, f.CoverOverall = o.CoverID, o.CoverOverall
					}
				}
				win[f.Path] = f
				continue
			}
			pending = append(pending, f)
			keep = append(keep, f.Path)
			st.Folders++
		}
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
		rows, err := s.DB.Query(`SELECT path, id, COALESCE(overall, 0) FROM photos WHERE path > ? ORDER BY path LIMIT ?`, after, scanPage)
		if err != nil {
			return st, err
		}
		type ph struct {
			path    string
			id      int64
			overall float64
		}
		var batch []ph
		for rows.Next() {
			var p ph
			if err := rows.Scan(&p.path, &p.id, &p.overall); err != nil {
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
			for i := range chain {
				f := &chain[i].f
				f.Total++
				if p.overall > f.CoverOverall || (p.overall == f.CoverOverall && p.id < f.CoverID) {
					f.CoverID, f.CoverOverall = p.id, p.overall
				}
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
		keep = append(keep, f.Path)
		st.Folders++
	}
	if err := flushPending(); err != nil {
		return st, err
	}
	// folders no longer holding photos
	sort.Strings(keep)
	at := ""
	for {
		if err := w.commit(); err != nil {
			return st, err
		}
		paths, err := s.strings(`SELECT path FROM folders WHERE path > ? ORDER BY path LIMIT ?`, at, scanPage)
		if err != nil {
			return st, err
		}
		for _, p := range paths {
			at = p
			if i := sort.SearchStrings(keep, p); i < len(keep) && keep[i] == p {
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
	for k, v := range map[string]string{"folders_seq": strconv.FormatInt(seq, 10), "folders_clean": dirty, "folders_version": foldersVersion} {
		if err := s.SetMeta(k, v); err != nil {
			return st, err
		}
	}
	st.Took = time.Since(start)
	return st, nil
}

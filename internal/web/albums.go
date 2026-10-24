package web

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/auteursoft/archivis/internal/store"
)

// byPathMax is the most photos an album may hold to be read through the
// path index and sorted (beyond it, walking the sort's own index and
// skipping photos elsewhere is quicker).
const byPathMax = 200000

// albumCard is a folder shown as a card: its best photo and how many.
type albumCard struct {
	Name  string
	Href  string
	Path  string
	Total int
	Cover *store.Photo // nil if none
}

type crumb struct{ Name, Href string }

type albumsData struct {
	*browseData         // the photos (nil on the top page)
	Top         bool    // the top page: the indexed folders
	NoRoots     bool    // no indexed folders recorded yet
	Name        string  // the album shown
	Crumbs      []crumb // the way up to it
	Folders     []albumCard
	Stale       bool // the folders are being worked out
}

func albumHref(v url.Values) string { return "/albums?" + v.Encode() }

func dirHref(dir string) string { return albumHref(url.Values{"dir": {dir}}) }

// handleAlbums serves Albums: the indexed folders (and every Apple Photos
// library, together as "iPhone"); a folder's subfolders, then everything
// below it, with the Photos page's filters, sorts and search.
func (s *Server) handleAlbums(w http.ResponseWriter, r *http.Request) {
	st := s.cat.Store
	q := r.URL.Query()
	dir, iphone := store.TrimDir(q.Get("dir")), q.Get("iphone") != ""
	p := &page{Title: "Albums", Nav: "albums"}
	d := &albumsData{Stale: st.FoldersStale()}
	p.Data = d
	if d.Stale {
		s.regroupInBackground(false)
	}
	fail := func(err error) {
		if err != nil && p.Error == "" {
			p.Error = err.Error()
		}
	}
	roots, err := st.Roots()
	fail(err)
	libs, err := st.PhotosLibraries()
	fail(err)

	if dir == "" && !iphone {
		d.Top = true
		d.NoRoots = len(roots) == 0
		var fs []store.Folder
		for _, rt := range roots {
			f, err := st.Folder(rt)
			fail(err)
			if f == nil {
				f = &store.Folder{Path: rt}
			}
			fs = append(fs, *f)
		}
		d.Folders = s.albumCards(fs, fail)
		if lib := iphoneAlbum(libs); lib != nil {
			c := s.albumCards([]store.Folder{*lib}, fail)[0]
			c.Name, c.Href, c.Path = "iPhone", albumHref(url.Values{"iphone": {"1"}}), "Apple Photos libraries"
			d.Folders = append(d.Folders, c)
		}
		s.render(w, r, "albums", p)
		return
	}

	f := parseFilter(r)
	text := strings.TrimSpace(q.Get("q"))
	p.Filter, p.Query = f, text
	total := 0
	reset := url.Values{}
	if iphone {
		d.Name = "iPhone"
		reset.Set("iphone", "1")
		for _, l := range libs {
			f.Dirs = append(f.Dirs, l.Path)
			total += l.Total
		}
		if len(f.Dirs) == 0 {
			f.Dirs = []string{"\x00none"} // no libraries: nothing
		}
	} else {
		reset.Set("dir", dir)
		d.Name = store.Folder{Path: dir}.Name()
		f.Dirs = []string{dir}
		if fo, err := st.Folder(dir); err != nil {
			fail(err)
		} else if fo != nil {
			total = fo.Total
		}
		subs, err := st.Subfolders(dir)
		fail(err)
		d.Folders = s.albumCards(subs, fail)
		d.Crumbs = crumbs(dir, roots)
	}
	p.Title = d.Name + " · Albums"
	f.DirsByPath = total < byPathMax
	known := -1
	plain := f
	plain.Dirs = nil
	if !plain.Restricts() && !d.Stale {
		known = total // the folders say how many: no count
	}
	s.filterForm(r, p, "/albums")
	if text != "" {
		reset.Set("q", text)
	}
	p.Params["reset"] = albumHref(reset)
	d.browseData = s.browse(r, p, f, text, "/albums", known)
	s.render(w, r, "albums", p)
}

// iphoneAlbum folds the Apple Photos libraries into one album (nil if none).
func iphoneAlbum(libs []store.Folder) *store.Folder {
	if len(libs) == 0 {
		return nil
	}
	a := libs[0]
	for _, l := range libs[1:] {
		a.Total += l.Total
		if l.CoverOverall > a.CoverOverall {
			a.CoverID, a.CoverOverall = l.CoverID, l.CoverOverall
		}
	}
	return &a
}

// albumCards makes cards for folders, with their covers.
func (s *Server) albumCards(fs []store.Folder, fail func(error)) []albumCard {
	var ids []int64
	for _, f := range fs {
		if f.CoverID != 0 {
			ids = append(ids, f.CoverID)
		}
	}
	covers := map[int64]*store.Photo{}
	if len(ids) > 0 {
		ps, err := s.cat.Store.Photos(ids)
		fail(err)
		for i := range ps {
			covers[ps[i].ID] = &ps[i]
		}
	}
	out := make([]albumCard, 0, len(fs))
	for _, f := range fs {
		out = append(out, albumCard{Name: f.Name(), Href: dirHref(f.Path), Path: f.Path, Total: f.Total, Cover: covers[f.CoverID]})
	}
	return out
}

// crumbs is the way from Albums down to dir's parent: through the deepest
// indexed folder holding it, or from the top if none does.
func crumbs(dir string, roots []string) []crumb {
	root := ""
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	for _, rt := range roots {
		if store.InDir(dir, rt) {
			root = rt
			break
		}
	}
	var up []crumb
	for d := store.ParentDir(dir); d != "" && len(d) >= len(root); d = store.ParentDir(d) {
		up = append(up, crumb{store.Folder{Path: d}.Name(), dirHref(d)})
		if d == root {
			break
		}
	}
	out := []crumb{{"Albums", "/albums"}}
	for i := len(up) - 1; i >= 0; i-- {
		out = append(out, up[i])
	}
	return out
}

package web

import (
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/auteursoft/archivis/internal/store"
)

// byPathMax is the most photos an album may hold to be read through the
// path index and sorted (beyond it, walking the sort's own index and
// skipping photos elsewhere is quicker).
const byPathMax = 200000

// albumCard is a folder shown as a card: its best photo and how many.
type albumCard struct {
	Name     string
	Own      string // the folder's own name (Name may be a display name)
	Album    string // a top-level album's key for renaming: its root, or store.IPhoneAlbum
	Href     string
	Path     string
	Total    int
	Cover    *store.Photo // nil if none
	NewestAt int64        // 0: none dated
	Avg      float64
	Scored   bool // Avg means something
}

// Newest is the month of the newest photo below ("" if none is dated).
func (c albumCard) Newest() string {
	if c.NewestAt == 0 {
		return ""
	}
	return time.Unix(c.NewestAt, 0).UTC().Format("Jan 2006")
}

type crumb struct{ Name, Href, Path string }

type albumsData struct {
	*browseData         // the photos (nil on the top page)
	Top         bool    // the top page: the indexed folders
	NoRoots     bool    // no indexed folders recorded yet
	Name        string  // the album shown
	Path        string  // its folder, when Name is a display name
	Crumbs      []crumb // the way up to it
	Folders     []albumCard
	Sort        string      // how the album cards are ordered (albumSorts)
	Keep        [][2]string // the page's other parameters, for the album sort form
	Stale       bool        // the folders are being worked out
}

// albumSorts are the orders album cards can be shown in (asort=), the
// first the default.
var albumSorts = []string{"name", "newest", "score"}

func albumHref(v url.Values) string { return "/albums?" + v.Encode() }

// dirHref links to a folder's album, keeping the album cards' order.
func dirHref(dir, asort string) string {
	v := url.Values{"dir": {dir}}
	if asort != albumSorts[0] {
		v.Set("asort", asort)
	}
	return albumHref(v)
}

// sortCards orders album cards: by name (case aside, then path); by the
// newest photo, undated last; or by average score, unscored last.
func sortCards(cs []albumCard, by string) {
	byName := func(a, b albumCard) bool {
		if x, y := strings.ToLower(a.Name), strings.ToLower(b.Name); x != y {
			return x < y
		}
		return a.Path < b.Path
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		switch by {
		case "newest":
			if (a.NewestAt == 0) != (b.NewestAt == 0) {
				return b.NewestAt == 0
			}
			if a.NewestAt != b.NewestAt {
				return a.NewestAt > b.NewestAt
			}
		case "score":
			if a.Scored != b.Scored {
				return a.Scored
			}
			if a.Avg != b.Avg {
				return a.Avg > b.Avg
			}
		}
		return byName(a, b)
	})
}

// handleAlbums serves Albums: the indexed folders (and every Apple Photos
// library, together as "iPhone"); a folder's subfolders, then everything
// below it, with the Photos page's filters, sorts and search.
func (s *Server) handleAlbums(w http.ResponseWriter, r *http.Request) {
	st := s.cat.Store
	q := r.URL.Query()
	dir, iphone := store.TrimDir(q.Get("dir")), q.Get("iphone") != ""
	p := &page{Title: "Albums", Nav: "albums"}
	d := &albumsData{Stale: st.FoldersStale(), Sort: albumSorts[0]}
	if v := q.Get("asort"); slices.Contains(albumSorts, v) {
		d.Sort = v
	}
	for k, vs := range q {
		if k != "asort" && k != "page" {
			for _, v := range vs {
				d.Keep = append(d.Keep, [2]string{k, v})
			}
		}
	}
	sort.Slice(d.Keep, func(i, j int) bool { return d.Keep[i][0] < d.Keep[j][0] })
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
	names, err := st.AlbumNames()
	fail(err)
	var libs []store.Folder
	if dir == "" { // the top page or the iPhone album
		libs, err = st.PhotosLibraries()
		fail(err)
	}

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
		d.Folders = s.albumCards(fs, d.Sort, fail)
		for i := range d.Folders {
			c := &d.Folders[i]
			c.Album = c.Path
			if n := names[c.Path]; n != "" {
				c.Name = n
			}
		}
		if lib := iphoneAlbum(libs); lib != nil {
			c := s.albumCards([]store.Folder{*lib}, d.Sort, fail)[0]
			v := url.Values{"iphone": {"1"}}
			if d.Sort != albumSorts[0] {
				v.Set("asort", d.Sort)
			}
			c.Name, c.Own, c.Album, c.Href, c.Path = "iPhone", "iPhone", store.IPhoneAlbum, albumHref(v), "Apple Photos libraries"
			if n := names[store.IPhoneAlbum]; n != "" {
				c.Name = n
			}
			d.Folders = append(d.Folders, c)
		}
		sortCards(d.Folders, d.Sort)
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
		if n := names[store.IPhoneAlbum]; n != "" {
			d.Name = n
		}
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
		if n := names[dir]; n != "" && slices.Contains(roots, dir) {
			d.Name, d.Path = n, dir
		}
		f.Dirs = []string{dir}
		if fo, err := st.Folder(dir); err != nil {
			fail(err)
		} else if fo != nil {
			total = fo.Total
		}
		subs, err := st.Subfolders(dir)
		fail(err)
		d.Folders = s.albumCards(subs, d.Sort, fail)
		sortCards(d.Folders, d.Sort)
		d.Crumbs = crumbs(dir, roots, names, d.Sort)
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
	if d.Sort != albumSorts[0] {
		reset.Set("asort", d.Sort)
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
		a.Add(l) // the cover as folders choose theirs
	}
	return &a
}

// albumCards makes cards for folders, with their covers, linking to them
// with the cards' order kept.
func (s *Server) albumCards(fs []store.Folder, asort string, fail func(error)) []albumCard {
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
		avg, scored := f.AvgScore()
		out = append(out, albumCard{Name: f.Name(), Own: f.Name(), Href: dirHref(f.Path, asort), Path: f.Path, Total: f.Total,
			Cover: covers[f.CoverID], NewestAt: f.NewestAt, Avg: avg, Scored: scored})
	}
	return out
}

// crumbs is the way from Albums down to dir's parent: through the deepest
// indexed folder holding it (by its display name, if it has one), or from
// the top if none does; the links keep the album cards' order.
func crumbs(dir string, roots []string, names map[string]string, asort string) []crumb {
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
		name := store.Folder{Path: d}.Name()
		if n := names[d]; n != "" && d == root {
			name = n
		}
		up = append(up, crumb{name, dirHref(d, asort), d})
		if d == root {
			break
		}
	}
	top := "/albums"
	if asort != albumSorts[0] {
		top += "?asort=" + url.QueryEscape(asort)
	}
	out := []crumb{{"Albums", top, ""}}
	for i := len(up) - 1; i >= 0; i-- {
		out = append(out, up[i])
	}
	return out
}

// renameAlbum sets a top-level album's display name (admins, as /admin/
// is; anyone without accounts), then goes back to Albums.
func (s *Server) renameAlbum(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	album, name := r.PostFormValue("album"), r.PostFormValue("name")
	ok, err := s.cat.Store.SetAlbumName(album, name)
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case !ok:
		http.Error(w, "no such album: "+album, http.StatusNotFound)
		return
	}
	log.Printf("%s named album %s %q", rater(r), album, strings.Join(strings.Fields(name), " "))
	back := "/albums"
	if v := r.PostFormValue("asort"); slices.Contains(albumSorts, v) && v != albumSorts[0] {
		back += "?asort=" + v
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

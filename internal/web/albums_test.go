package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/store"
)

func albumsServer(t *testing.T, roots []string) *Server {
	t.Helper()
	s, cat, _ := newTestServer(t)
	seedAlbums(t, cat, roots)
	return s
}

// seedAlbums catalogues photos in a few folders and two Apple Photos
// libraries, records roots and works the folders out. Only /pics/2020 and
// /pics2 have dated photos (2020 and 2023).
func seedAlbums(t *testing.T, cat *catalog.Catalog, roots []string) {
	t.Helper()
	var rs []*store.Result
	for p, o := range map[string]float64{
		"/pics/2019/trip/a.jpg": 50, "/pics/2019/trip/b.jpg": 70, "/pics/2019/c.jpg": 60, "/pics/2020/d.jpg": 40,
		"/pics2/e.jpg": 99,
		"/m/Photos Library.photoslibrary/originals/A/f.heic": 30,
		"/n/Old.photoslibrary/originals/B/g.heic":            80,
	} {
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: o, Version: store.SchemaVersion}})
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	for _, r := range roots { // as recorded (RecordRoots would add a drive on Windows)
		if _, err := cat.Store.DB.Exec(`INSERT INTO roots (path) VALUES (?)`, r); err != nil {
			t.Fatal(err)
		}
	}
	for p, at := range map[string]int64{"/pics/2020/d.jpg": 1590000000, "/pics2/e.jpg": 1690000000} {
		if _, err := cat.Store.DB.Exec(`UPDATE photos SET taken_at = ? WHERE path = ?`, at, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.Store.RebuildFolders(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAlbumsTop(t *testing.T) {
	s := albumsServer(t, nil)
	h := s.Handler()
	body := get(t, h, "/albums")
	mustContain(t, "albums without roots", body, "archivis roots add", `href="/albums?iphone=1"`)

	s = albumsServer(t, []string{"/pics", "/pics2", "/gone"})
	body = html.UnescapeString(get(t, s.Handler(), "/albums"))
	mustContain(t, "albums", body,
		`href="/albums?dir=%2Fpics"`, `href="/albums?dir=%2Fpics2"`, `href="/albums?dir=%2Fgone"`, `href="/albums?iphone=1"`,
		">pics<", ">gone<", ">iPhone<")
	if strings.Contains(body, "archivis roots add") {
		t.Error("the no-roots hint shows with roots recorded")
	}
	// the iPhone card counts both libraries, with the better cover
	i := strings.Index(body, `href="/albums?iphone=1"`)
	if card := body[i:min(len(body), i+400)]; !strings.Contains(card, `<span class="count">2</span>`) || !strings.Contains(card, "Old.photoslibrary") {
		t.Errorf("iPhone card: %s", card)
	}
}

func TestAlbumFolder(t *testing.T) {
	s := albumsServer(t, []string{"/pics"})
	h := s.Handler()
	body := html.UnescapeString(get(t, h, "/albums?dir="+url.QueryEscape("/pics/2019")))
	mustContain(t, "folder", body,
		`<a href="/albums">Albums</a> › <a href="/albums?dir=%2Fpics" title="/pics">pics</a> › <b>2019</b>`, // the way up, from the root
		`href="/albums?dir=%2Fpics%2F2019%2Ftrip"`, ">trip<",                                                // the subfolder
		"3 photos", "/pics/2019/trip/a.jpg", "/pics/2019/trip/b.jpg", "/pics/2019/c.jpg", // everything below
		`<input type="hidden" name="dir" value="/pics/2019">`, `action="/albums"`)
	if strings.Contains(body, "/pics/2020/d.jpg") || strings.Contains(body, "/pics2/e.jpg") {
		t.Error("a folder page shows photos from beside it")
	}
	// filters apply within the folder, and the pager keeps the folder
	body = html.UnescapeString(get(t, h, "/albums?dir=%2Fpics&min_overall=55"))
	mustContain(t, "filtered folder", body, "2 photos")
	if strings.Contains(body, "/pics/2019/trip/a.jpg") {
		t.Error("filter ignored in a folder")
	}
	body = html.UnescapeString(get(t, h, "/albums?dir=%2Fpics&min_overall=55&page=1"))
	mustContain(t, "folder pager", body, `href="/albums?dir=%2Fpics&min_overall=55&page=0"`)
}

func TestAlbumIPhone(t *testing.T) {
	s := albumsServer(t, []string{"/pics"})
	body := html.UnescapeString(get(t, s.Handler(), "/albums?iphone=1"))
	mustContain(t, "iPhone", body, "2 photos", "originals/A/f.heic", "originals/B/g.heic", `<input type="hidden" name="iphone" value="1">`)
	if strings.Contains(body, "/pics/") {
		t.Error("the iPhone album shows photos from elsewhere")
	}
}

// The header's search, in an album, searches the album.
func TestAlbumSearchStaysInAlbum(t *testing.T) {
	s := albumsServer(t, []string{"/pics"})
	h := s.Handler()
	body := html.UnescapeString(get(t, h, "/albums?dir=%2Fpics%2F2019"))
	i := strings.Index(body, `<form class="search"`)
	if i < 0 {
		t.Fatal("no search form")
	}
	form := body[i : i+strings.Index(body[i:], "</form>")]
	mustContain(t, "album search form", form, `action="/albums"`, `<input type="hidden" name="dir" value="/pics/2019">`, "Search this album")
	body = html.UnescapeString(get(t, h, "/albums?iphone=1"))
	i = strings.Index(body, `<form class="search"`)
	mustContain(t, "iPhone search form", body[i:i+strings.Index(body[i:], "</form>")], `action="/albums"`, `name="iphone" value="1"`)
	body = get(t, h, "/")
	i = strings.Index(body, `<form class="search"`)
	mustContain(t, "photos search form", body[i:i+strings.Index(body[i:], "</form>")], `action="/"`)
}

// The iPhone album's cover is chosen as the folders choose theirs: the
// highest score, then the lowest photo id, whichever library comes first.
func TestIPhoneCoverTieBreak(t *testing.T) {
	for _, libs := range [][]store.Folder{
		{{Path: "/a.photoslibrary", Total: 1, CoverID: 9, CoverOverall: 50}, {Path: "/b.photoslibrary", Total: 2, CoverID: 4, CoverOverall: 50}},
		{{Path: "/a.photoslibrary", Total: 2, CoverID: 4, CoverOverall: 50}, {Path: "/b.photoslibrary", Total: 1, CoverID: 9, CoverOverall: 50}},
	} {
		if a := iphoneAlbum(libs); a.CoverID != 4 || a.Total != 3 {
			t.Errorf("iphoneAlbum(%v) = cover %d, %d photos; want cover 4, 3 photos", libs, a.CoverID, a.Total)
		}
	}
}

// order returns where each of the strings first appears in body, failing
// if one is missing.
func order(t *testing.T, body string, want ...string) []int {
	t.Helper()
	var out []int
	for _, w := range want {
		i := strings.Index(body, w)
		if i < 0 {
			t.Fatalf("missing %q", w)
		}
		out = append(out, i)
	}
	return out
}

// Album cards sort by name, newest photo (undated last) or average score
// (unscored last), and the order is kept in the links down and back up.
func TestAlbumSort(t *testing.T) {
	s := albumsServer(t, []string{"/pics", "/pics2", "/gone"})
	h := s.Handler()
	cards := []string{`href="/albums?dir=%2Fgone`, `href="/albums?iphone=1`, `href="/albums?dir=%2Fpics"`, `href="/albums?dir=%2Fpics2`}
	cardsSorted := []string{`href="/albums?asort=%s&amp;dir=%%2Fgone"`, `href="/albums?asort=%s&amp;iphone=1"`, `href="/albums?asort=%s&amp;dir=%%2Fpics"`, `href="/albums?asort=%s&amp;dir=%%2Fpics2"`}
	for _, c := range []struct {
		sort string
		want []int // the cards above, in the order expected
	}{
		{"", []int{0, 1, 2, 3}},
		{"name", []int{0, 1, 2, 3}},
		{"newest", []int{3, 2, 0, 1}}, // 2023, 2020, then the undated by name
		{"score", []int{3, 1, 2, 0}},  // 99, 55 and 55 (by name), unscored
		{"bogus", []int{0, 1, 2, 3}},
	} {
		body := get(t, h, "/albums?asort="+c.sort)
		var want []string
		for _, i := range c.want {
			if c.sort == "newest" || c.sort == "score" {
				want = append(want, fmt.Sprintf(cardsSorted[i], c.sort))
			} else {
				want = append(want, cards[i])
			}
		}
		at := order(t, body, want...)
		if !sort.IntsAreSorted(at) {
			t.Errorf("asort=%s: cards out of order: %v", c.sort, at)
		}
		if c.sort == "newest" || c.sort == "score" {
			mustContain(t, "sorted albums", body, fmt.Sprintf(`<option value="%s" selected>`, c.sort))
		}
	}
	// cards show the newest month and the average
	body := html.UnescapeString(get(t, h, "/albums"))
	mustContain(t, "card details", body, "Jul 2023 · avg 99", "May 2020 · avg 55", "undated · avg 55")
	// a folder keeps the order for its subfolders, the crumbs and the photos' filter form
	body = get(t, h, "/albums?dir=%2Fpics&asort=newest")
	at := order(t, body, `href="/albums?asort=newest&amp;dir=%2Fpics%2F2020"`, `href="/albums?asort=newest&amp;dir=%2Fpics%2F2019"`)
	if at[0] > at[1] {
		t.Error("subfolders not newest first")
	}
	mustContain(t, "folder keeps the order", body, `<a href="/albums?asort=newest">Albums</a>`, `<input type="hidden" name="asort" value="newest">`)
	// with filters but no photo sort, the album sort form orders the
	// (filtered) subfolders and keeps the photos' filters
	body = get(t, h, "/albums?dir=%2Fpics&min_overall=10")
	i := strings.Index(body, `<form class="albumsort"`)
	if i < 0 {
		t.Fatal("no album sort form")
	}
	form := body[i : i+strings.Index(body[i:], "</form>")]
	mustContain(t, "album sort form", form, `name="dir" value="/pics"`, `name="min_overall" value="10"`, `<option value="name" selected>`)
	// a photo sort orders the subfolders by their best photo instead
	if body := get(t, h, "/albums?dir=%2Fpics&sort=newest"); strings.Contains(body, `<form class="albumsort"`) {
		t.Error("album sort form shown while a photo sort orders the albums")
	}
}

// Display names: admins (anyone, without accounts) name the top-level
// albums; the cards, the album's title and the way back show the name, with
// the folder still to be seen.
func TestAlbumNamesWithoutAccounts(t *testing.T) {
	s, cat, _ := newTestServer(t)
	seedAlbums(t, cat, []string{"/pics", "/pics2"})
	b := &browser{t: t, h: s.Handler()}
	if body := b.do("GET", "/albums", nil).Body.String(); !strings.Contains(body, `action="/admin/albums/name"`) {
		t.Fatal("no naming form without accounts")
	}
	if rec := b.do("POST", "/admin/albums/name", url.Values{"album": {"/pics"}, "name": {"Photo stage"}, "asort": {"score"}}); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/albums?asort=score" {
		t.Fatalf("naming: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := b.do("POST", "/admin/albums/name", url.Values{"album": {store.IPhoneAlbum}, "name": {"Phones"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("naming iPhone: %d", rec.Code)
	}
	if rec := b.do("POST", "/admin/albums/name", url.Values{"album": {"/elsewhere"}, "name": {"x"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("naming an unknown album: %d", rec.Code)
	}
	body := html.UnescapeString(b.do("GET", "/albums", nil).Body.String())
	mustContain(t, "named albums", body, ">Photo stage</span><small>", ">Phones</span><small>", ">pics2</span><small>", `title="/pics"`, `placeholder="pics"`, `value="Photo stage"`)
	body = html.UnescapeString(b.do("GET", "/albums?dir=%2Fpics%2F2019", nil).Body.String())
	mustContain(t, "named crumbs", body, `<a href="/albums?dir=%2Fpics" title="/pics">Photo stage</a>`)
	body = html.UnescapeString(b.do("GET", "/albums?dir=%2Fpics", nil).Body.String())
	mustContain(t, "named album", body, "<title>Photo stage · Albums", `<b title="/pics">Photo stage</b> <small class="path">/pics</small>`)
	body = html.UnescapeString(b.do("GET", "/albums?iphone=1", nil).Body.String())
	mustContain(t, "named iPhone", body, "<title>Phones · Albums")
}

func TestAlbumNamesAdminOnly(t *testing.T) {
	s, cat, _ := accountsServer(t)
	seedAlbums(t, cat, []string{"/pics"})
	h := s.Handler()
	admin := &browser{t: t, h: h}
	admin.login("root", testPassword)
	newUser(t, cat, "eve", store.RoleEditor)
	editor := &browser{t: t, h: h}
	editor.login("eve", testPassword)
	if body := editor.do("GET", "/albums", nil).Body.String(); strings.Contains(body, "/admin/albums/name") {
		t.Fatal("an editor sees the naming form")
	}
	if rec := editor.do("POST", "/admin/albums/name", url.Values{"album": {"/pics"}, "name": {"Mine"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("editor naming: %d", rec.Code)
	}
	if body := admin.do("GET", "/albums", nil).Body.String(); !strings.Contains(body, `action="/admin/albums/name"`) {
		t.Fatal("an admin lacks the naming form")
	}
	admin.hdr = map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}
	if rec := admin.do("POST", "/admin/albums/name", url.Values{"album": {"/pics"}, "name": {"Theirs"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site naming: %d", rec.Code)
	}
	admin.hdr = nil
	if rec := admin.do("POST", "/admin/albums/name", url.Values{"album": {"/pics"}, "name": {"Ours"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("admin naming: %d", rec.Code)
	}
	if names, _ := cat.Store.AlbumNames(); names["/pics"] != "Ours" {
		t.Fatalf("names = %v", names)
	}
}

// A root inside another root is named the same in its parent's list as at
// the top, and sorts there by that name.
func TestNamedNestedRootInParent(t *testing.T) {
	s, cat, _ := newTestServer(t)
	seedAlbums(t, cat, []string{"/pics", "/pics/2019"})
	b := &browser{t: t, h: s.Handler()}
	if rec := b.do("POST", "/admin/albums/name", url.Values{"album": {"/pics/2019"}, "name": {"Zebra trip"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("naming: %d", rec.Code)
	}
	body := html.UnescapeString(b.do("GET", "/albums?dir=%2Fpics", nil).Body.String())
	mustContain(t, "named subfolder", body, ">Zebra trip</span><small>")
	at := order(t, body, `>2020</span>`, `>Zebra trip</span>`) // by name: 2020 before Zebra (it was 2019, first)
	if at[0] > at[1] {
		t.Error("the named subfolder sorts by its folder name, not its display name")
	}
}

// Filters on the album list narrow it to albums holding a match, each shown
// by its best match (under the photos' sort, which then orders the cards);
// links into an album carry the filters and the list to come back to (lf),
// which the way back keeps even once the album's own filters are cleared.
func TestAlbumListFilters(t *testing.T) {
	s := albumsServer(t, []string{"/pics", "/pics2"})
	h := s.Handler()
	// a filter hides albums without a match
	body := get(t, h, "/albums?min_overall=90")
	if !strings.Contains(body, `href="/albums?dir=%2Fpics2&amp;lf=min_overall%3D90&amp;min_overall=90"`) || strings.Contains(body, "dir=%2Fpics&amp;") || strings.Contains(body, "iphone=1") {
		t.Error("min_overall=90 should leave /pics2 alone")
	}
	// a photo sort: covers are each album's best match, cards in its order
	body = get(t, h, "/albums?min_overall=55&sort=overall")
	at := order(t, body, `dir=%2Fpics2&amp;`, `iphone=1&amp;`, `dir=%2Fpics&amp;`)
	if !sort.IntsAreSorted(at) {
		t.Errorf("cards not ordered by their best match: %v", at)
	}
	mustContain(t, "best-match cover", body, `src="/t//pics/2019/trip/b.jpg.jpg"`, // 70, not the folder's own 60/50 mix
		`href="/albums?dir=%2Fpics2&amp;lf=min_overall%3D55%26sort%3Doverall&amp;min_overall=55&amp;sort=overall"`)
	if strings.Contains(body, `<form class="albumsort"`) {
		t.Error("album sort form shown while a photo sort orders the albums")
	}
	// nothing matches: say so, and how to see every album
	mustContain(t, "no match", get(t, h, "/albums?min_overall=100"), "No albums hold photos matching these filters", `href="/albums">Show every album`)
	// inside an album: its subfolders are filtered by its own filters, and
	// links carry them and the list (lf) along
	body = get(t, h, "/albums?dir=%2Fpics&min_overall=65&lf=min_overall%3D55%26sort%3Doverall")
	mustContain(t, "filtered subfolders", body, `href="/albums?dir=%2Fpics%2F2019&amp;lf=min_overall%3D55%26sort%3Doverall&amp;min_overall=65"`,
		`<input type="hidden" name="lf" value="min_overall=55&amp;sort=overall">`)
	if strings.Contains(body, `dir=%2Fpics%2F2020`) {
		t.Error("a subfolder with no match (2020: 40) still shown")
	}
	// the album's filters cleared: the way back still leads to the filtered
	// list, through ancestors with its filters; reset keeps lf
	body = get(t, h, "/albums?dir=%2Fpics%2F2019&lf=min_overall%3D55%26sort%3Doverall")
	mustContain(t, "way back", body, `<a href="/albums?min_overall=55&amp;sort=overall">Albums</a>`,
		`href="/albums?dir=%2Fpics&amp;lf=min_overall%3D55%26sort%3Doverall&amp;min_overall=55&amp;sort=overall"`)
	body = get(t, h, "/albums?dir=%2Fpics%2F2019&min_overall=90&lf=min_overall%3D55%26sort%3Doverall")
	mustContain(t, "reset keeps the list", body, `href="/albums?dir=%2Fpics%2F2019&amp;lf=min_overall%3D55%26sort%3Doverall">Reset</a>`)
	// no filters: the list as before, every album, no lf on its links
	body = get(t, h, "/albums")
	mustContain(t, "unfiltered", body, `href="/albums?dir=%2Fpics"`, `href="/albums?dir=%2Fpics2"`, `href="/albums?iphone=1"`)
	if strings.Contains(body, "lf=") {
		t.Error("unfiltered list links carry lf")
	}
}

// Review follow-ups: "everything" chosen on the list goes into the albums
// opened; the iPhone album leads back to the filtered list; an album's own
// folder is no filter, so an unfiltered album lists every subfolder (one
// of memes alone too) as before; and the list's filter form leaves the
// cards in their order unless a photo sort is chosen.
func TestAlbumListFilterFollowUps(t *testing.T) {
	s, cat, _ := newTestServer(t)
	seedAlbums(t, cat, []string{"/pics", "/pics2"})
	meme := &store.Result{Photo: store.Photo{Path: "/pics/memes/x.png", Size: 1, MTime: 1, Fingerprint: "meme", Overall: 20, Source: store.SourceMeme, Version: store.SchemaVersion}}
	if err := cat.Store.Save([]*store.Result{meme}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Store.RebuildFolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	mustContain(t, "everything kept", get(t, h, "/albums?source=all"), `href="/albums?dir=%2Fpics&amp;lf=source%3Dall&amp;source=all"`)
	mustContain(t, "iPhone way back", get(t, h, "/albums?iphone=1&min_overall=25&lf=min_overall%3D25"), `<a href="/albums?min_overall=25">Albums</a>`)
	body := get(t, h, "/albums?dir=%2Fpics")
	mustContain(t, "unfiltered album", body, `href="/albums?dir=%2Fpics%2Fmemes"`)
	body = get(t, h, "/albums")
	mustContain(t, "list form", body, `<option value="" selected>Albums in their order</option>`, `<form class="albumsort"`)
	if strings.Contains(get(t, h, "/albums?dir=%2Fpics"), "Albums in their order") {
		t.Error("an album's photo sort offers the list's order")
	}
}

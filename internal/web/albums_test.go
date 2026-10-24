package web

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/store"
)

func albumsServer(t *testing.T, roots []string) *Server {
	t.Helper()
	s, cat, _ := newTestServer(t)
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
	if _, err := cat.Store.RebuildFolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
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
		`<a href="/albums">Albums</a> › <a href="/albums?dir=%2Fpics">pics</a> › <b>2019</b>`, // the way up, from the root
		`href="/albums?dir=%2Fpics%2F2019%2Ftrip"`, ">trip<",                                  // the subfolder
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

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/auth"
	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/store"
)

func starPhotos(t *testing.T, cat *catalog.Catalog, n int) []int64 {
	t.Helper()
	var rs []*store.Result
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/st/%d.jpg", i)
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Overall: float64(50 + i), Version: store.SchemaVersion}})
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range rs {
		ids = append(ids, r.Photo.ID)
	}
	return ids
}

func starredOf(t *testing.T, b *browser, ids []int64) []int64 {
	t.Helper()
	var q []string
	for _, id := range ids {
		q = append(q, fmt.Sprint(id))
	}
	rec := b.do("GET", "/api/stars?ids="+strings.Join(q, ","), nil)
	var got struct{ Starred []int64 }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("stars lookup: %d %s", rec.Code, rec.Body)
	}
	return got.Starred
}

// Each account stars photos for itself, viewers too; another account sees
// none of them; Starred (and the Starred only filter) list one's own.
func TestStarsPerAccount(t *testing.T) {
	s, cat, _ := accountsServer(t)
	ids := starPhotos(t, cat, 3)
	hash, _ := auth.HashPassword(testPassword)
	for _, name := range []string{"ann", "bob"} {
		if _, err := cat.Store.CreateUser(name, store.RoleViewer, hash); err != nil {
			t.Fatal(err)
		}
	}
	ann, bob := &browser{t: t, h: s.Handler()}, &browser{t: t, h: s.Handler()}
	ann.login("ann", testPassword)
	bob.login("bob", testPassword)
	if rec := ann.api(fmt.Sprintf("/api/star/%d", ids[1]), `{"on":true}`); rec.Code != 200 {
		t.Fatalf("a viewer starring: %d %s", rec.Code, rec.Body)
	}
	if got := starredOf(t, ann, ids); len(got) != 1 || got[0] != ids[1] {
		t.Fatalf("ann's stars: %v", got)
	}
	if got := starredOf(t, bob, ids); len(got) != 0 {
		t.Fatalf("bob sees ann's stars: %v", got)
	}
	for _, path := range []string{"/?starred=1", "/?starred=1&sort=newest"} {
		body := ann.do("GET", path, nil).Body.String()
		if !strings.Contains(body, `title="/st/1.jpg`) || strings.Contains(body, `title="/st/0.jpg`) || strings.Contains(body, `title="/st/2.jpg`) {
			t.Fatalf("%s: ann's starred page does not list only her star", path)
		}
		if !strings.Contains(body, `name="starred" value="1" checked`) || !strings.Contains(body, "<title>Starred photos") {
			t.Fatalf("%s: the filter or title does not say starred", path)
		}
		if !strings.Contains(body, `class="btn ghost" href="/?starred=1">Reset`) {
			t.Fatalf("%s: Reset leaves the Starred page", path)
		}
	}
	if body := bob.do("GET", "/?starred=1", nil).Body.String(); strings.Contains(body, `title="/st/`) {
		t.Fatal("bob's starred page lists ann's star")
	}
	// unstar; a missing photo is a 404; a cross-site star is refused
	if rec := ann.api(fmt.Sprintf("/api/star/%d", ids[1]), `{"on":false}`); rec.Code != 200 || len(starredOf(t, ann, ids)) != 0 {
		t.Fatalf("unstarring: %d", rec.Code)
	}
	if rec := ann.api("/api/star/999999", `{"on":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("starring a missing photo: %d", rec.Code)
	}
	if rec := ann.api("/api/star/999999", `{"on":false}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unstarring a missing photo: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/star/%d", ids[0]), strings.NewReader(`{"on":true}`))
	req.Host = "photos.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(ann.cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(starredOf(t, ann, ids)) != 0 {
		t.Fatalf("cross-site star: %d", rec.Code)
	}
}

// Without accounts there is one shared list, and every card has a star.
func TestStarsWithoutAccounts(t *testing.T) {
	s, cat, _ := newTestServer(t)
	ids := starPhotos(t, cat, 2)
	b := &browser{t: t, h: s.Handler()}
	if rec := b.api(fmt.Sprintf("/api/star/%d", ids[0]), `{"on":true}`); rec.Code != 200 {
		t.Fatalf("starring: %d %s", rec.Code, rec.Body)
	}
	if n, _ := cat.Store.StarCount(0); n != 1 {
		t.Fatalf("shared list holds %d", n)
	}
	body := get(t, s.Handler(), "/")
	mustContain(t, "photos", body, fmt.Sprintf(`data-star="%d"`, ids[0]), fmt.Sprintf(`data-star="%d"`, ids[1]), `href="/?starred=1"`)
	mustContain(t, "photo page", get(t, s.Handler(), fmt.Sprintf("/photo/%d", ids[0])), fmt.Sprintf(`class="star inline" data-star="%d"`, ids[0]))
	if body := get(t, s.Handler(), "/?starred=1"); !strings.Contains(body, `title="/st/0.jpg`) || strings.Contains(body, `title="/st/1.jpg`) {
		t.Fatal("the shared starred page")
	}
}

// Stacked cards carry a star too: a Duplicates card and a stack on the
// Photos page, for the card's photo.
func TestStackCardsHaveStars(t *testing.T) {
	s, cat, _ := newTestServer(t)
	var rs []*store.Result
	for i := 0; i < 3; i++ { // one shot three times: a set of copies and a stack
		p := fmt.Sprintf("/dup/%d.jpg", i)
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: "Z 8",
			TakenAt: 1_700_000_000 + int64(i), PHash: 77, Overall: float64(60 - i), Version: store.SchemaVersion}})
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Store.RebuildGroups(context.Background(), cat.Store.GroupOptions()); err != nil {
		t.Fatal(err)
	}
	top := rs[0].Photo.ID // the best: the copies' and the stack's top
	want := fmt.Sprintf(`data-star="%d"`, top)
	mustContain(t, "duplicates", get(t, s.Handler(), "/duplicates"), `class="card stack"`, want)
	mustContain(t, "photos", get(t, s.Handler(), "/"), `class="card stack"`, want)
}

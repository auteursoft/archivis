package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"archivis/internal/catalog"
	"archivis/internal/imageio"
	"archivis/internal/indexer"
	"archivis/internal/ml"
	"archivis/internal/store"
)

func newTestServer(t *testing.T) (*Server, *catalog.Catalog, int64) {
	t.Helper()
	cat, err := catalog.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	id, err := cat.Store.EnsurePerson("Ada")
	if err != nil {
		t.Fatal(err)
	}
	return New(cat), cat, id
}

func post(h http.Handler, path, body, contentType string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8088"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCrossSiteWritesRefused(t *testing.T) {
	s, cat, id := newTestServer(t)
	h := s.Handler()
	path := "/api/person/" + strconv.FormatInt(id, 10) + "/delete"
	for name, tc := range map[string]struct {
		ct  string
		hdr map[string]string
	}{
		"cross-site fetch metadata": {"application/json", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"same-site but other port":  {"application/json", map[string]string{"Sec-Fetch-Site": "same-site"}},
		"foreign Origin":            {"application/json", map[string]string{"Origin": "https://evil.example"}},
		"opaque Origin":             {"application/json", map[string]string{"Origin": "null"}},
		"HTML form body":            {"application/x-www-form-urlencoded", nil},
		"no content type":           {"", nil},
	} {
		rec := post(h, path, "", tc.ct, tc.hdr)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: status %d, want 403/415", name, rec.Code)
		}
	}
	if p, _ := cat.Store.Person(id); p == nil {
		t.Fatal("person was deleted by a forged request")
	}
	// the real UI: same-origin fetch with a JSON body
	rec := post(h, path, "{}", "application/json", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8088"})
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin delete: status %d %s", rec.Code, rec.Body)
	}
	if p, _ := cat.Store.Person(id); p != nil {
		t.Fatal("legitimate delete did not delete")
	}
}

func TestRenameMissingPersonIs404(t *testing.T) {
	s, _, _ := newTestServer(t)
	rec := post(s.Handler(), "/api/person/424242/rename", `{"name":"Grace"}`, "application/json", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (%s)", rec.Code, rec.Body)
	}
	rec = post(s.Handler(), "/api/person/424242/delete", `{}`, "application/json", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: status %d, want 404", rec.Code)
	}
}

// The face page reads Photo and Person from faceView (not the embedded
// store.Face); render it for real so a template field error can't hide.
func TestFacePageRenders(t *testing.T) {
	s, cat, pid := newTestServer(t)
	emb := make([]byte, 1024)   // 512 float16
	emb[0], emb[1] = 0x00, 0x3C // 1.0
	r := &store.Result{
		Photo: store.Photo{Path: "/pics/ada.jpg", Size: 1, MTime: 1, Fingerprint: "fp123", Version: store.SchemaVersion, FaceCount: 1},
		Faces: []store.Face{{Ordinal: 0, X1: .1, Y1: .1, X2: .3, Y2: .4, Score: .9, Embedding: emb}},
	}
	if err := cat.Store.Save([]*store.Result{r}); err != nil {
		t.Fatal(err)
	}
	fid := r.Faces[0].ID
	if err := cat.Store.AssignFaces(pid, []int64{fid}, "manual", nil); err != nil {
		t.Fatal(err)
	}
	if err := cat.Refresh(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/face/"+strconv.FormatInt(fid, 10), nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, body)
	}
	for _, want := range []string{`src="/f/fp123/0.jpg"`, `<h1>Ada</h1>`, `>ada.jpg</a>`, `</html>`} {
		if !strings.Contains(body, want) {
			t.Errorf("face page missing %q", want)
		}
	}
}

// twoFaces stores two photos whose faces have the same embedding, so naming
// one makes auto-matching label the other.
func twoFaces(t *testing.T, cat *catalog.Catalog) (int64, int64) {
	t.Helper()
	emb := make([]byte, 1024)
	emb[0], emb[1] = 0x00, 0x3C
	var rs []*store.Result
	for _, p := range []string{"/a.jpg", "/b.jpg"} {
		rs = append(rs, &store.Result{
			Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Version: store.SchemaVersion, FaceCount: 1},
			Faces: []store.Face{{X1: .1, Y1: .1, X2: .3, Y2: .4, Score: .9, Embedding: emb}},
		})
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	if err := cat.Refresh(); err != nil {
		t.Fatal(err)
	}
	return rs[0].Faces[0].ID, rs[1].Faces[0].ID
}

func jsonPost(h http.Handler, path, body string) *httptest.ResponseRecorder {
	return post(h, path, body, "application/json", map[string]string{"Sec-Fetch-Site": "same-origin"})
}

// Discover clusters unlabelled faces, so every labelling change must drop
// the cached clusters.
func TestLabelChangesInvalidateDiscover(t *testing.T) {
	s, cat, ada := newTestServer(t)
	h := s.Handler()
	f1, _ := twoFaces(t, cat)
	prime := func() {
		s.dmu.Lock()
		s.discAt = time.Now()
		s.dmu.Unlock()
	}
	stale := func(what string) {
		t.Helper()
		s.dmu.Lock()
		defer s.dmu.Unlock()
		if !s.discAt.IsZero() {
			t.Errorf("%s left the Discover cache in place", what)
		}
	}
	for _, step := range []struct{ name, path, body string }{
		{"assign", "/api/assign", fmt.Sprintf(`{"name":"Grace","faces":[%d]}`, f1)},
		{"unassign", "/api/unassign", fmt.Sprintf(`{"faces":[%d]}`, f1)},
		{"match", "/api/match", `{}`},
		{"delete person", fmt.Sprintf("/api/person/%d/delete", ada), `{}`},
	} {
		if step.name == "match" {
			jsonPost(h, "/api/assign", fmt.Sprintf(`{"name":"Grace","faces":[%d]}`, f1))
		}
		prime()
		if rec := jsonPost(h, step.path, step.body); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d %s", step.name, rec.Code, rec.Body)
		}
		stale(step.name)
	}
}

// The manual labels are saved before auto-matching runs, so a matching
// failure must be reported alongside the success, not swallowed.
func TestAssignReportsAutoMatchFailure(t *testing.T) {
	s, cat, _ := newTestServer(t)
	f1, f2 := twoFaces(t, cat)
	if _, err := cat.Store.DB.Exec(`CREATE TRIGGER no_auto BEFORE UPDATE ON faces
		WHEN NEW.person_source = 'auto' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	rec := jsonPost(s.Handler(), "/api/assign", fmt.Sprintf(`{"name":"Grace","faces":[%d],"match":true}`, f1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Assigned  int    `json:"assigned"`
		Auto      int    `json:"auto"`
		AutoError string `json:"auto_error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Assigned != 1 || !strings.Contains(resp.AutoError, "disk full") {
		t.Fatalf("response %s: want the assignment confirmed and the matching error reported", rec.Body)
	}
	fs, _ := cat.Store.FacesForPhoto(cat.PhotoOfFace(f2))
	if fs[0].PersonID.Valid {
		t.Fatal("trigger did not block auto-labelling; test is not exercising the failure")
	}
}

// samePerson stores the same face (identical embedding) in several photos;
// perPhoto[i] is how many times it appears in photo i.
func samePerson(t *testing.T, cat *catalog.Catalog, perPhoto ...int) [][]int64 {
	t.Helper()
	emb := make([]byte, 1024)
	emb[0], emb[1] = 0x00, 0x3C
	var rs []*store.Result
	for i, n := range perPhoto {
		r := &store.Result{Photo: store.Photo{Path: fmt.Sprintf("/p%d.jpg", i), Size: 1, MTime: 1,
			Fingerprint: fmt.Sprintf("fp%d", i), Version: store.SchemaVersion, FaceCount: n}}
		for j := 0; j < n; j++ {
			x := 0.1 + 0.4*float64(j)
			r.Faces = append(r.Faces, store.Face{X1: x, Y1: .1, X2: x + .2, Y2: .4, Score: .9, WidthPx: 120, Embedding: emb})
		}
		rs = append(rs, r)
	}
	if err := cat.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	if err := cat.Refresh(); err != nil {
		t.Fatal(err)
	}
	var ids [][]int64
	for _, r := range rs {
		var f []int64
		for _, x := range r.Faces {
			f = append(f, x.ID)
		}
		ids = append(ids, f)
	}
	return ids
}

func get(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func mustContain(t *testing.T, page, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("%s: missing %q", page, w)
		}
	}
}

// Counts labelled "photos" must count photos: a person seen twice in one
// photo is in one photo.
func TestPagesCountPhotosNotFaces(t *testing.T) {
	s, cat, _ := newTestServer(t)
	h := s.Handler()

	// Discover: 4 unnamed faces across 3 photos is one person in 3 photos.
	samePerson(t, cat, 2, 1, 1)
	mustContain(t, "discover", get(t, h, "/discover"), "Appears in 3 photos")

	// Name them: 4 faces, 3 photos; the two in photo 0 are confirmed.
	var all []int64
	rows, _ := cat.Store.DB.Query(`SELECT id FROM faces ORDER BY id`)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		all = append(all, id)
	}
	rows.Close()
	gid, _ := cat.Store.EnsurePerson("Grace")
	cat.Store.AssignFaces(gid, all[:2], "manual", nil)
	cat.Store.AssignFaces(gid, all[2:], "auto", nil)
	mustContain(t, "people", get(t, h, "/people"), "3 photos · 1 confirmed")
	mustContain(t, "person", get(t, h, fmt.Sprintf("/person/%d", gid)),
		"In 3 photos · 1 confirmed by you", "Browse their 3 photos", "Faces labelled Grace (4)")

	// Face page: the other face in the same photo is not "another photo".
	mustContain(t, "face", get(t, h, fmt.Sprintf("/face/%d", all[0])), "This face appears in 2 other photos")
	mustContain(t, "face (single)", get(t, h, fmt.Sprintf("/face/%d", all[2])), "This face appears in 2 other photos")
}

func TestPluralOnePhoto(t *testing.T) {
	plural := funcs["plural"].(func(int, string) string)
	for n, want := range map[int]string{0: "0 photos", 1: "1 photo", 2: "2 photos"} {
		if got := plural(n, "photo"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}

// A photo indexed now (full-content, 32-digit fingerprint) and one from an
// older catalogue (24 digits) must both get their thumbnails served.
func TestThumbnailsServedForCurrentAndLegacyFingerprints(t *testing.T) {
	s, cat, _ := newTestServer(t)
	root := t.TempDir()
	jpg, err := os.ReadFile("../imageio/testdata/coffee.jpg")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "a.jpg"), jpg, 0o644)
	ix := indexer.New(indexer.Config{Roots: []string{root}, Workers: 1}, cat.Store, &indexer.Engines{}, cat.Layout)
	if err := ix.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var fp string
	cat.Store.DB.QueryRow(`SELECT fingerprint FROM photos`).Scan(&fp)
	if len(fp) != 32 {
		t.Fatalf("fingerprint %q", fp)
	}
	get := func(path string) int {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK && rec.Header().Get("Content-Type") != "image/jpeg" {
			t.Errorf("%s: content type %q", path, rec.Header().Get("Content-Type"))
		}
		return rec.Code
	}
	if code := get("/t/" + fp + ".jpg"); code != http.StatusOK {
		t.Fatalf("thumbnail of a newly indexed photo: HTTP %d", code)
	}
	// legacy: rename the thumbnail to a 24-digit fingerprint
	legacy := fp[:24]
	os.MkdirAll(filepath.Dir(cat.Layout.Thumb(legacy)), 0o755)
	os.Rename(cat.Layout.Thumb(fp), cat.Layout.Thumb(legacy))
	if code := get("/t/" + legacy + ".jpg"); code != http.StatusOK {
		t.Fatalf("legacy thumbnail: HTTP %d", code)
	}
	for _, bad := range []string{"/t/" + fp[:31] + ".jpg", "/t/" + strings.ToUpper(fp) + ".jpg", "/f/" + fp + "x/0.jpg"} {
		if code := get(bad); code != http.StatusNotFound {
			t.Errorf("%s: HTTP %d, want 404", bad, code)
		}
	}
	// path traversal: the router redirects to the cleaned path; never a file
	if code := get("/t/../../etc/passwd.jpg"); code == http.StatusOK {
		t.Error("path traversal served a file")
	}
}

// Uploads must not grow the heap without limit: the cache keeps at most
// maxQueries entries, evicting the oldest, and drops the decoded image.
func TestQueryCacheIsBounded(t *testing.T) {
	s, _, _ := newTestServer(t)
	var toks []string
	for i := 0; i < maxQueries+10; i++ {
		tok := fmt.Sprintf("tok%03d", i)
		toks = append(toks, tok)
		s.cacheQuery(tok, &catalog.QueryResult{Analysis: &indexer.Analysis{Decoded: &imageio.Decoded{}}})
		time.Sleep(time.Millisecond) // distinct timestamps
	}
	s.qmu.Lock()
	defer s.qmu.Unlock()
	if len(s.queries) != maxQueries {
		t.Fatalf("%d cached uploads, want %d", len(s.queries), maxQueries)
	}
	if s.queries[toks[0]] != nil || s.queries[toks[len(toks)-1]] == nil {
		t.Fatal("eviction should drop the oldest upload and keep the newest")
	}
	for _, e := range s.queries {
		if e.q.Analysis.Decoded != nil {
			t.Fatal("decoded image kept in the cache")
		}
	}
}

func TestAssignUnknownFaceCreatesNobody(t *testing.T) {
	s, cat, _ := newTestServer(t)
	rec := post(s.Handler(), "/api/assign", `{"name":"Ghost","faces":[424242]}`, "application/json", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d (%s), want 404", rec.Code, rec.Body)
	}
	if p, _ := cat.Store.PersonByName("Ghost"); p != nil {
		t.Fatal("assigning a nonexistent face created a person")
	}
}

// Deep or overflowing page numbers must neither crash the server nor
// allocate unbounded result buffers.
func TestHugePageNumbers(t *testing.T) {
	s, _, _ := newTestServer(t)
	for _, pg := range []string{"100000000", "90000000000000000", "-5", "abc"} {
		for _, q := range []string{"", "&sort=newest"} {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?page="+pg+q, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("page=%s%s: HTTP %d", pg, q, rec.Code)
			}
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/?page=90000000000000000", nil)
	if f := parseFilter(r); f.Offset < 0 || f.Offset > maxPage*f.Limit {
		t.Fatalf("offset %d", f.Offset)
	}
}

func TestPreviewCaching(t *testing.T) {
	s, cat, _ := newTestServer(t)
	root := t.TempDir()
	jpg, _ := os.ReadFile("../imageio/testdata/coffee.jpg")
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		os.WriteFile(filepath.Join(root, n), append(bytes.Clone(jpg), n...), 0o644)
	}
	ix := indexer.New(indexer.Config{Roots: []string{root}, Workers: 1}, cat.Store, &indexer.Engines{}, cat.Layout)
	if err := ix.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := func(n string) string {
		var i int64
		cat.Store.DB.QueryRow(`SELECT id FROM photos WHERE path = ?`, filepath.Join(root, n)).Scan(&i)
		return strconv.FormatInt(i, 10)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	// normal: rendered, cached, nothing temporary left behind
	if rec := get("/preview/" + id("a.jpg")); rec.Code != 200 || rec.Header().Get("Cache-Control") == "no-store" {
		t.Fatalf("preview: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	filepath.Walk(filepath.Join(cat.Layout.Dir, "previews"), func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".tmp") {
			t.Errorf("temporary file left: %s", p)
		}
		return nil
	})
	// original offline: thumbnail, not cached as the preview
	os.Remove(filepath.Join(root, "b.jpg"))
	if rec := get("/preview/" + id("b.jpg")); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("offline fallback: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	// cache can't be written (a file where its folder should be -- works
	// even as root): still served, uncached
	var fp string
	cat.Store.DB.QueryRow(`SELECT fingerprint FROM photos WHERE id = ?`, id("c.jpg")).Scan(&fp)
	os.WriteFile(filepath.Join(cat.Layout.Dir, "previews", fp[:2]), []byte("not a folder"), 0o644)
	rec := get("/preview/" + id("c.jpg"))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Body.Len() == 0 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unwritable cache: %d %q %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
}

func TestUploadStreamsToResultPage(t *testing.T) {
	s, _, _ := newTestServer(t)
	jpg, _ := os.ReadFile("../imageio/testdata/coffee.jpg")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("image", "coffee.jpg")
	fw.Write(jpg)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/query", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/query/") {
		t.Fatalf("upload: %d %q %s", rec.Code, loc, rec.Body)
	}
	if code := get(t, s.Handler(), loc); !strings.Contains(code, "Search by photo") {
		t.Fatal("result page missing")
	}
	// no upload left on disk
	m, _ := filepath.Glob(filepath.Join(os.TempDir(), "archivis-upload-*"))
	if len(m) != 0 {
		t.Fatalf("temporary uploads left: %v", m)
	}
}

func TestBestMatchOptionOnlyWhenSearching(t *testing.T) {
	s, _, _ := newTestServer(t)
	if body := get(t, s.Handler(), "/"); strings.Contains(body, `value="relevance"`) {
		t.Error("Best match offered without a search")
	}
	body := get(t, s.Handler(), "/?q=dogs")
	if !strings.Contains(body, `<option value="relevance" selected>Best match</option>`) {
		t.Error("Best match not offered (and preselected) for a search")
	}
}

// A failed lookup is shown as an error, not as "nothing found"; a face whose
// photo is gone is a 404, not a crash.
func TestPagePartFailuresAreShown(t *testing.T) {
	s, cat, _ := newTestServer(t)
	ids := samePerson(t, cat, 1, 1)
	var pid int64
	cat.Store.DB.QueryRow(`SELECT photo_id FROM faces WHERE id = ?`, ids[0][0]).Scan(&pid)
	cat.Store.DB.Exec(`DROP TABLE tags`)
	body := get(t, s.Handler(), fmt.Sprintf("/photo/%d", pid))
	if !strings.Contains(body, "Part of this page could not be loaded") || !strings.Contains(body, "tags:") {
		t.Error("tag lookup failure not shown")
	}
	// orphan a face: its photo row disappears (foreign keys off on one conn)
	ctx := context.Background()
	conn, _ := cat.Store.DB.Conn(ctx)
	conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	conn.ExecContext(ctx, `DELETE FROM photos WHERE id = (SELECT photo_id FROM faces WHERE id = ?)`, ids[1][0])
	conn.Close()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/face/%d", ids[1][0]), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("face of a missing photo: HTTP %d", rec.Code)
	}
}

func TestFvalIgnoresNonFinite(t *testing.T) {
	for q, want := range map[string]float64{"min=0.6": 0.6, "min=NaN": 0, "min=Inf": 0, "min=-inf": 0, "min=abc": 0, "": 0} {
		if got := fval(httptest.NewRequest(http.MethodGet, "/?"+q, nil), "min"); got != want {
			t.Errorf("%q: %v", q, got)
		}
	}
}

// Aesthetic feedback is validated, attributed to the basic-auth user, kept
// with the score shown, and shown back on the photo page.
func TestAestheticFeedback(t *testing.T) {
	s, cat, _ := newTestServer(t)
	s.Auth = "ann:pw"
	h := s.Handler()
	r := &store.Result{Photo: store.Photo{Path: "/pics/x.jpg", Size: 1, MTime: 1, Fingerprint: "fpx", Version: store.SchemaVersion,
		Aesthetic: sql.NullFloat64{Float64: 6.2, Valid: true}}}
	bare := &store.Result{Photo: store.Photo{Path: "/pics/y.jpg", Size: 1, MTime: 1, Fingerprint: "fpy", Version: store.SchemaVersion}}
	if err := cat.Store.Save([]*store.Result{r, bare}); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(r.Photo.ID, 10)
	send := func(path, body string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Host = "127.0.0.1:8088"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.SetBasicAuth("ann", "pw")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, c := range []struct {
		body string
		want int
	}{ // in order: the last valid one is the latest judgement
		{`{}`, 400},
		{`{"rating": 0}`, 400},
		{`{"rating": 11}`, 400},
		{`{"verdict": "meh"}`, 400},
		{`{"rating": "x"}`, 400},
		{`{"verdict": "high"}`, 200},
		{`{"rating": 4}`, 200},
	} {
		if got := send("/api/photo/"+id+"/aesthetic", c.body); got != c.want {
			t.Errorf("%s: status %d, want %d", c.body, got, c.want)
		}
	}
	if got := send("/api/photo/99999/aesthetic", `{"rating": 4}`); got != 404 {
		t.Errorf("unknown photo: %d", got)
	}
	bid := strconv.FormatInt(bare.Photo.ID, 10)
	if got := send("/api/photo/"+bid+"/aesthetic", `{"verdict": "agree"}`); got != 400 {
		t.Errorf("verdict on a photo without a score: %d", got)
	}
	if got := send("/api/photo/"+bid+"/aesthetic", `{"rating": 8}`); got != 200 {
		t.Errorf("rating a photo without a score: %d", got)
	}
	fb, _, err := cat.Store.LatestFeedback("ann")
	if err != nil {
		t.Fatal(err)
	}
	// the photos have no CLIP embedding, so they are not training data
	if len(fb) != 0 {
		t.Fatalf("feedback without embeddings offered for training: %+v", fb)
	}
	f, _ := cat.Store.PhotoFeedback(r.Photo.ID, "ann")
	if f == nil || f.Rating.Float64 != 4 || f.Shown != 6.2 || !strings.Contains(f.Models, ml.BuiltinAestheticID) {
		t.Fatalf("stored feedback %+v", f)
	}
	req := httptest.NewRequest(http.MethodGet, "/photo/"+id, nil)
	req.SetBasicAuth("ann", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, want := range []string{"Is the aesthetic score of 6.2 right?", "You said: 4 / 10.", `data-rating="10"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("photo page missing %q", want)
		}
	}
}

// Thumbnail links are named after the photo they open.
func TestThumbnailsHaveNames(t *testing.T) {
	s, cat, _ := newTestServer(t)
	r := &store.Result{Photo: store.Photo{Path: "/pics/harbour at dusk.jpg", Size: 1, MTime: 1, Fingerprint: "fpz", Version: store.SchemaVersion}}
	if err := cat.Store.Save([]*store.Result{r}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `alt="harbour at dusk.jpg"`) {
		t.Fatal("thumbnail has no name")
	}
}

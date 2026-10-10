package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/auteursoft/archivis/internal/auth"
	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/store"
)

// browser is a minimal cookie-keeping client for a handler.
type browser struct {
	t      *testing.T
	h      http.Handler
	cookie *http.Cookie
	remote string
	hdr    map[string]string
}

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Host = "photos.example"
	if b.remote != "" {
		req.RemoteAddr = b.remote
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", "http://photos.example")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	} else {
		req.Header.Set("Sec-Fetch-Dest", "document")
	}
	for k, v := range b.hdr {
		req.Header.Set(k, v)
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if c.MaxAge < 0 {
				b.cookie = nil
			} else {
				b.cookie = c
			}
		}
	}
	return rec
}

func (b *browser) api(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "photos.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	return rec
}

func (b *browser) login(name, pw string) *httptest.ResponseRecorder {
	return b.do(http.MethodPost, "/login", url.Values{"name": {name}, "password": {pw}, "next": {"/people"}})
}

const testPassword = "correct horse battery"

// accountsServer returns a server with an admin "root" (password
// testPassword).
func accountsServer(t *testing.T) (*Server, *catalog.Catalog, *store.User) {
	t.Helper()
	s, cat, _ := newTestServer(t)
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := cat.Store.CreateUser("root", store.RoleAdmin, hash)
	if err != nil {
		t.Fatal(err)
	}
	return s, cat, admin
}

func TestNoAccountsKeepsOldBehaviour(t *testing.T) {
	s, _, _ := newTestServer(t)
	b := &browser{t: t, h: s.Handler()}
	if rec := b.do("GET", "/people", nil); rec.Code != 200 {
		t.Fatalf("open server: %d", rec.Code)
	}
	if rec := b.do("GET", "/login", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login without accounts: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := b.do("GET", "/admin/users", nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "add-admin") {
		t.Fatalf("users page without accounts: %d", rec.Code)
	}
	s.Auth = "ann:pw"
	b.h = s.Handler()
	if rec := b.do("GET", "/people", nil); rec.Code != 401 {
		t.Fatalf("shared password not required: %d", rec.Code)
	}
}

func TestSignInAndRoles(t *testing.T) {
	s, cat, admin := accountsServer(t)
	s.Auth = "ann:pw" // ignored once accounts exist
	h := s.Handler()
	anon := &browser{t: t, h: h}

	// signed out: pages redirect to sign-in, everything else is refused
	rec := anon.do("GET", "/people", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next=%2Fpeople" {
		t.Fatalf("signed out page: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, p := range []string{"/t/abc.jpg", "/original/1", "/preview/1"} {
		req := httptest.NewRequest("GET", p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("signed out %s: %d", p, rec.Code)
		}
	}
	if rec := anon.api("/api/refresh", "{}"); rec.Code != 401 {
		t.Fatalf("signed out API: %d", rec.Code)
	}
	// the old shared password no longer opens anything
	req := httptest.NewRequest("GET", "/people", nil)
	req.SetBasicAuth("ann", "pw")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatal("basic auth still works with accounts")
	}
	if rec := anon.do("GET", "/static/app.css", nil); rec.Code != 200 {
		t.Fatalf("static: %d", rec.Code)
	}
	if rec := anon.do("GET", "/login", nil); rec.Code != 200 || strings.Contains(rec.Body.String(), "photos ·") {
		t.Fatalf("login page: %d (must not show catalogue figures)", rec.Code)
	}

	// wrong password, unknown user, then the right one
	if rec := anon.login("root", "wrong password!"); rec.Code != 401 || anon.cookie != nil {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	if rec := anon.login("nobody", testPassword); rec.Code != 401 || !strings.Contains(rec.Body.String(), "Wrong name or password") {
		t.Fatalf("unknown user: %d", rec.Code)
	}
	rec = anon.login("ROOT", testPassword)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/people" || anon.cookie == nil {
		t.Fatalf("sign in: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	c := anon.cookie
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure {
		t.Fatalf("cookie flags: %+v", c)
	}
	if strings.Contains(c.Value, auth.HashToken(c.Value)) {
		t.Fatal("cookie holds the stored hash")
	}
	rec = anon.do("GET", "/people", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Sign out") || !strings.Contains(rec.Body.String(), `href="/admin/users"`) {
		t.Fatalf("signed in page: %d", rec.Code)
	}
	if rec := anon.do("GET", "/admin/users", nil); rec.Code != 200 {
		t.Fatalf("admin page: %d", rec.Code)
	}

	// a viewer browses but cannot edit, download originals or administer
	viewer := newUser(t, cat, "vic", store.RoleViewer)
	vb := &browser{t: t, h: h}
	vb.login("vic", testPassword)
	if rec := vb.do("GET", "/people", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `class="readonly"`) || strings.Contains(rec.Body.String(), "/admin/users") {
		t.Fatalf("viewer page: %d", rec.Code)
	}
	if rec := vb.do("GET", "/original/1", nil); rec.Code != 403 {
		t.Fatalf("viewer original: %d", rec.Code)
	}
	if rec := vb.api("/api/refresh", "{}"); rec.Code != 403 {
		t.Fatalf("viewer API: %d", rec.Code)
	}
	if rec := vb.do("GET", "/admin/users", nil); rec.Code != 403 {
		t.Fatalf("viewer admin: %d", rec.Code)
	}
	if rec := vb.do("POST", "/admin/invite", url.Values{"name": {"x"}, "role": {"admin"}}); rec.Code != 403 {
		t.Fatalf("viewer invite: %d", rec.Code)
	}
	// an editor may edit but not administer
	newUser(t, cat, "eve", store.RoleEditor)
	eb := &browser{t: t, h: h}
	eb.login("eve", testPassword)
	if rec := eb.api("/api/refresh", "{}"); rec.Code != 200 {
		t.Fatalf("editor API: %d %s", rec.Code, rec.Body)
	}
	if rec := eb.do("GET", "/admin/users", nil); rec.Code != 403 {
		t.Fatalf("editor admin: %d", rec.Code)
	}

	// disabling signs the viewer out at once
	if rec := anon.do("POST", "/admin/users/"+strconv.FormatInt(viewer.ID, 10), url.Values{"action": {"disable"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if rec := vb.do("GET", "/people", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("disabled user still in: %d", rec.Code)
	}
	if rec := vb.login("vic", testPassword); rec.Code != 401 {
		t.Fatalf("disabled user signed in: %d", rec.Code)
	}
	// the last admin cannot demote themself
	rec = anon.do("POST", "/admin/users/"+strconv.FormatInt(admin.ID, 10), url.Values{"action": {"role"}, "role": {"viewer"}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "at least one active admin") {
		t.Fatalf("demote last admin: %d", rec.Code)
	}

	// sign out ends the session on the server, not just the cookie
	old := anon.cookie
	if rec := anon.do("POST", "/logout", url.Values{}); rec.Code != http.StatusSeeOther || anon.cookie != nil {
		t.Fatalf("logout: %d", rec.Code)
	}
	anon.cookie = old
	if rec := anon.do("GET", "/people", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("session survived sign-out: %d", rec.Code)
	}
}

func newUser(t *testing.T, cat *catalog.Catalog, name, role string) *store.User {
	t.Helper()
	hash, _ := auth.HashPassword(testPassword)
	u, err := cat.Store.CreateUser(name, role, hash)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

var linkRe = regexp.MustCompile(`value="(https?://[^"]+/invite/[A-Za-z0-9_-]+)"`)

func TestInviteFlow(t *testing.T) {
	s, cat, _ := accountsServer(t)
	h := s.Handler()
	admin := &browser{t: t, h: h}
	admin.login("root", testPassword)

	rec := admin.do("POST", "/admin/invite", url.Values{"name": {"Grace"}, "role": {"editor"}})
	m := linkRe.FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	link := m[1]
	if !strings.HasPrefix(link, "http://photos.example/invite/") {
		t.Fatalf("link = %s", link)
	}
	path := strings.TrimPrefix(link, "http://photos.example")
	// the link is shown once; the users page lists the invite without it
	if rec := admin.do("GET", "/admin/users", nil); strings.Contains(rec.Body.String(), path) || !strings.Contains(rec.Body.String(), "Grace") {
		t.Fatal("pending invite not listed, or its link shown again")
	}
	if rec := admin.do("POST", "/admin/invite", url.Values{"name": {"root"}, "role": {"viewer"}}); rec.Code != 400 {
		t.Fatalf("invite a taken name: %d", rec.Code)
	}

	guest := &browser{t: t, h: h}
	rec = guest.do("GET", path, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Grace") || rec.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("invite page: %d %q", rec.Code, rec.Header().Get("Referrer-Policy"))
	}
	if rec := guest.do("POST", path, url.Values{"password": {"short"}, "password2": {"short"}}); rec.Code != 400 {
		t.Fatalf("short password accepted: %d", rec.Code)
	}
	if rec := guest.do("POST", path, url.Values{"password": {testPassword}, "password2": {testPassword + "x"}}); rec.Code != 400 {
		t.Fatalf("mismatched passwords accepted: %d", rec.Code)
	}
	rec = guest.do("POST", path, url.Values{"password": {testPassword}, "password2": {testPassword}})
	if rec.Code != http.StatusSeeOther || guest.cookie == nil {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	if rec := guest.api("/api/refresh", "{}"); rec.Code != 200 {
		t.Fatalf("new editor cannot edit: %d", rec.Code)
	}
	// single use
	other := &browser{t: t, h: h}
	if rec := other.do("POST", path, url.Values{"password": {testPassword}, "password2": {testPassword}}); rec.Code != 404 || other.cookie != nil {
		t.Fatalf("invite reused: %d", rec.Code)
	}

	// password reset: a new link, the old sessions end
	u, _, _ := cat.Store.UserForLogin("grace")
	rec = admin.do("POST", "/admin/users/"+strconv.FormatInt(u.ID, 10), url.Values{"action": {"reset"}})
	m = linkRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body)
	}
	newPw := "another long passphrase"
	fresh := &browser{t: t, h: h}
	if rec := fresh.do("POST", strings.TrimPrefix(m[1], "http://photos.example"), url.Values{"password": {newPw}, "password2": {newPw}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reset accept: %d", rec.Code)
	}
	if rec := guest.do("GET", "/people", nil); rec.Code != http.StatusSeeOther {
		t.Fatal("old session survived a password reset")
	}
	if rec := (&browser{t: t, h: h}).login("grace", testPassword); rec.Code != 401 {
		t.Fatal("old password still works")
	}
	if rec := (&browser{t: t, h: h}).login("grace", newPw); rec.Code != http.StatusSeeOther {
		t.Fatal("new password refused")
	}
}

func TestSignInLimits(t *testing.T) {
	s, _, _ := accountsServer(t)
	s.nameLimit = auth.NewLimiter(3, time.Hour)
	s.ipLimit = auth.NewLimiter(5, time.Hour)
	h := s.Handler()
	b := &browser{t: t, h: h, remote: "198.51.100.7:5000"}
	for i := 0; i < 3; i++ {
		b.login("root", "wrong password!")
	}
	// the name is locked, even with the right password, from anywhere
	if rec := b.login("root", testPassword); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked name: %d", rec.Code)
	}
	b2 := &browser{t: t, h: h, remote: "203.0.113.9:5000"}
	if rec := b2.login("root", testPassword); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked name from another address: %d", rec.Code)
	}
	// the address is limited across names
	b.login("a", "x")
	b.login("b", "x")
	if rec := b.login("c", "x"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked address: %d", rec.Code)
	}
	// behind a local proxy, the forwarded client address is what counts,
	// and a forged earlier entry does not help
	p := &browser{t: t, h: h, remote: "127.0.0.1:4000", hdr: map[string]string{"X-Forwarded-For": "1.2.3.4, 198.51.100.7"}}
	if rec := p.login("d", "x"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("proxied locked address: %d", rec.Code)
	}
	// a remote client cannot claim another address
	f := &browser{t: t, h: h, remote: "198.51.100.7:5001", hdr: map[string]string{"X-Forwarded-For": "10.9.9.9"}}
	if rec := f.login("e", "x"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("forged X-Forwarded-For escaped the limit: %d", rec.Code)
	}
}

func TestSecureCookieBehindHTTPSProxy(t *testing.T) {
	s, _, _ := accountsServer(t)
	h := s.Handler()
	b := &browser{t: t, h: h, remote: "127.0.0.1:4000", hdr: map[string]string{"X-Forwarded-Proto": "https"}}
	b.login("root", testPassword)
	if b.cookie == nil || !b.cookie.Secure {
		t.Fatalf("cookie not Secure behind an HTTPS proxy: %+v", b.cookie)
	}
	// only a local proxy is believed
	r := &browser{t: t, h: h, remote: "198.51.100.7:5000", hdr: map[string]string{"X-Forwarded-Proto": "https"}}
	r.login("root", testPassword)
	if r.cookie == nil || r.cookie.Secure {
		t.Fatalf("remote X-Forwarded-Proto believed: %+v", r.cookie)
	}
	// and the invitation link uses https
	rec := b.do("POST", "/admin/invite", url.Values{"name": {"hal"}, "role": {"viewer"}})
	if m := linkRe.FindStringSubmatch(rec.Body.String()); m == nil || !strings.HasPrefix(m[1], "https://photos.example/invite/") {
		t.Fatalf("invite link over https: %v", m)
	}
}

func TestCrossSiteAccountForms(t *testing.T) {
	s, _, _ := accountsServer(t)
	b := &browser{t: t, h: s.Handler()}
	b.login("root", testPassword)
	b.hdr = map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}
	if rec := b.do("POST", "/admin/invite", url.Values{"name": {"mallory"}, "role": {"admin"}}); rec.Code != 403 {
		t.Fatalf("cross-site invite: %d", rec.Code)
	}
	if rec := b.do("POST", "/logout", url.Values{}); rec.Code != 403 {
		t.Fatalf("cross-site logout: %d", rec.Code)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/people": "/people", "": "/", "//evil.example": "/", "https://evil.example": "/",
		"/\\evil.example": "/", "/a?b=c": "/a?b=c", "people": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRaterUsesAccount(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if rater(req) != "local" {
		t.Fatal("anonymous rater")
	}
	u := &store.User{Name: "Grace"}
	if got := rater(req.WithContext(withUser(req, u))); got != "Grace" {
		t.Fatalf("rater = %q", got)
	}
}

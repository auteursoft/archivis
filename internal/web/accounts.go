package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/auteursoft/archivis/internal/auth"
	"github.com/auteursoft/archivis/internal/store"
)

// Accounts. Once the first account exists (archivis users add-admin), every
// page needs a signed-in user, and the shared --auth password stops being
// used. Sessions are random tokens in an HttpOnly cookie, stored only as
// hashes. Admins invite people with single-use links that expire.

const (
	sessionCookie = "archivis_session"
	sessionTTL    = 30 * 24 * time.Hour
	// InviteTTL is how long an invitation or password-reset link works.
	InviteTTL = 7 * 24 * time.Hour
)

type userKey struct{}

// userFrom returns the signed-in account, or nil when accounts are not in
// use.
func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey{}).(*store.User)
	return u
}

func withUser(r *http.Request, u *store.User) context.Context {
	return context.WithValue(r.Context(), userKey{}, u)
}

// accountsOn reports whether any account exists. Accounts are never
// deleted, so once true it stays true without asking the database again.
func (s *Server) accountsOn() (bool, error) {
	if s.hasUsers.Load() {
		return true, nil
	}
	has, err := s.cat.Store.HasUsers()
	if has {
		s.hasUsers.Store(true)
	}
	return has, err
}

// requiredRole is the least role that may make a request.
func requiredRole(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/admin/"):
		return store.RoleAdmin
	case strings.HasPrefix(p, "/original/"):
		return store.RoleEditor
	case r.Method != http.MethodGet && r.Method != http.MethodHead && strings.HasPrefix(p, "/api/"):
		return store.RoleEditor
	}
	return store.RoleViewer
}

// publicPath is reachable without signing in.
func publicPath(p string) bool {
	return strings.HasPrefix(p, "/static/") || p == "/login" || strings.HasPrefix(p, "/invite/")
}

// authenticate admits requests: with accounts, by session and role;
// without, by the shared password if one is set.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Everything but the stylesheet and script depends on who asks: no
		// shared cache (a proxy) may keep it, or it could hand it to someone
		// else without this check. Handlers may set something stricter, or
		// a longer browser-only lifetime (serveCached).
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "private, no-cache")
		}
		on, err := s.accountsOn()
		if err != nil {
			http.Error(w, "accounts unavailable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Without accounts the shared password covers everything except the
		// sign-in pages (which then only redirect or refuse).
		if publicPath(r.URL.Path) && (on || !strings.HasPrefix(r.URL.Path, "/static/")) {
			if u := s.sessionUser(r); u != nil {
				r = r.WithContext(withUser(r, u))
			}
			next.ServeHTTP(w, r)
			return
		}
		if !on {
			if s.Auth != "" && !s.basicOK(r) {
				w.Header().Set("WWW-Authenticate", `Basic realm="Archivis"`)
				http.Error(w, "unauthorised", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		u := s.sessionUser(r)
		if u == nil {
			if wantsPage(r) {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(withUser(r, u))
		if need := requiredRole(r); !store.RoleAtLeast(u.Role, need) {
			if wantsPage(r) {
				s.renderStatus(w, r, "notfound", &page{Title: "Not allowed",
					Data: "Your account (" + u.Role + ") cannot do this. Ask an admin if you need " + need + " access."}, http.StatusForbidden)
				return
			}
			http.Error(w, "your account ("+u.Role+") cannot do this; it needs "+need+" access", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// wantsPage reports whether a request is a browser navigation (answered
// with a page or redirect) rather than an image or API call.
func wantsPage(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (s *Server) basicOK(r *http.Request) bool {
	user, pass, _ := strings.Cut(s.Auth, ":")
	u, p, ok := r.BasicAuth()
	return ok && subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 && subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
}

func (s *Server) sessionUser(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" || len(c.Value) > 100 {
		return nil
	}
	u, err := s.cat.Store.SessionUser(auth.HashToken(c.Value))
	if err != nil {
		if !errors.Is(err, store.ErrNoUser) {
			log.Printf("session: %v", err)
		}
		return nil
	}
	return u
}

// fromLocalProxy reports whether a request came through a reverse proxy on
// this machine (Caddy), whose X-Forwarded-* headers can be believed.
func fromLocalProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isHTTPS reports whether the browser reached us over HTTPS.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (fromLocalProxy(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

// clientIP is the browser's address: the remote address, or behind a local
// proxy the address it appended last to X-Forwarded-For (earlier entries
// are whatever the client sent, and can be forged).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if fromLocalProxy(r) {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
				return last
			}
		}
	}
	return host
}

// startSession signs u in on this browser.
// pwHash is the password hash the sign-in was checked against.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User, pwHash string) error {
	tok, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	if err := s.cat.Store.CreateSession(hash, u.ID, pwHash, sessionTTL, r.UserAgent()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	return nil
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
}

// safeNext keeps a post-login redirect on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

func waitText(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m <= 1 {
		return "a minute"
	}
	return strconv.Itoa(m) + " minutes"
}

type loginData struct{ Name, Next string }

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	on, err := s.accountsOn()
	if err != nil || !on {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := &loginData{Next: safeNext(r.FormValue("next"))}
	p := &page{Title: "Sign in", Bare: true, Data: d}
	if r.Method == http.MethodGet {
		if userFrom(r) != nil {
			http.Redirect(w, r, d.Next, http.StatusSeeOther)
			return
		}
		s.render(w, r, "login", p)
		return
	}
	d.Name = strings.TrimSpace(r.PostFormValue("name"))
	pw := r.PostFormValue("password")
	ip, nameKey := "ip:"+clientIP(r), "name:"+store.NameKey(d.Name) // the account's own key: equivalent spellings share a limit
	for _, k := range []string{ip, nameKey} {
		lim := s.ipLimit
		if k == nameKey {
			lim = s.nameLimit
		}
		if blocked, wait := lim.Blocked(k); blocked {
			p.Error = "Too many failed attempts. Try again in " + waitText(wait) + "."
			s.renderStatus(w, r, "login", p, http.StatusTooManyRequests)
			return
		}
	}
	u, hash, err := s.cat.Store.UserForLogin(d.Name)
	ok := false
	if err == nil && hash != "" {
		ok = auth.VerifyPassword(hash, pw) && !u.Disabled
	} else {
		auth.BurnTime(pw) // an unknown name takes as long as a wrong password
	}
	if !ok {
		s.ipLimit.Fail(ip)
		s.nameLimit.Fail(nameKey)
		log.Printf("sign-in failed for %q from %s", d.Name, clientIP(r))
		p.Error = "Wrong name or password."
		s.renderStatus(w, r, "login", p, http.StatusUnauthorized)
		return
	}
	s.nameLimit.Reset(nameKey)
	if err := s.startSession(w, r, u, hash); err != nil {
		p.Error = "Could not sign in: " + err.Error()
		s.renderStatus(w, r, "login", p, http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, d.Next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		s.cat.Store.DeleteSession(auth.HashToken(c.Value))
	}
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type inviteData struct {
	Invite *store.Invite
	Reset  bool
}

// handleInvite lets an invited person choose a password (or someone sent a
// reset link choose a new one), then signs them in.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	// "origin": the page's own stylesheet and script requests carry only
	// the site in Referer, not this URL with its token (a proxy's access log
	// could keep it). Not no-referrer: with it, browsers send "Origin: null"
	// on the form, which sameOrigin refuses.
	w.Header().Set("Referrer-Policy", "origin")
	w.Header().Set("Cache-Control", "no-store")
	ip := "ip:" + clientIP(r)
	p := &page{Title: "Welcome", Bare: true}
	if blocked, wait := s.ipLimit.Blocked(ip); blocked {
		p.Error = "Too many failed attempts. Try again in " + waitText(wait) + "."
		s.renderStatus(w, r, "invite", p, http.StatusTooManyRequests)
		return
	}
	hash := auth.HashToken(r.PathValue("tok"))
	inv, err := s.cat.Store.InviteByToken(hash)
	if err != nil {
		s.ipLimit.Fail(ip)
		p.Error = store.ErrInviteInvalid.Error() + ". Ask whoever sent it for a new one."
		s.renderStatus(w, r, "invite", p, http.StatusNotFound)
		return
	}
	d := &inviteData{Invite: inv, Reset: inv.UserID != 0}
	p.Data = d
	if r.Method == http.MethodGet {
		s.render(w, r, "invite", p)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	pw := r.PostFormValue("password")
	if pw != r.PostFormValue("password2") {
		p.Error = "The two passwords differ."
		s.renderStatus(w, r, "invite", p, http.StatusBadRequest)
		return
	}
	if err := auth.CheckPassword(inv.Name, pw); err != nil {
		p.Error = "Choose another password: " + strings.TrimPrefix(err.Error(), auth.ErrWeakPassword.Error()+": ") + "."
		s.renderStatus(w, r, "invite", p, http.StatusBadRequest)
		return
	}
	pwHash, err := auth.HashPassword(pw)
	if err == nil {
		var u *store.User
		if u, err = s.cat.Store.AcceptInvite(hash, pwHash); err == nil {
			s.hasUsers.Store(true)
			if err = s.startSession(w, r, u, pwHash); err == nil {
				log.Printf("%s accepted an invitation (%s)", u.Name, u.Role)
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
	}
	p.Error = err.Error()
	s.renderStatus(w, r, "invite", p, http.StatusBadRequest)
}

type usersData struct {
	Me      *store.User
	Users   []store.User
	Invites []store.Invite
	Link    string // a new invitation or reset link, shown once
	LinkFor string
	Roles   []string
}

// inviteLink is the full address of an invitation, as this browser reached
// the site.
func inviteLink(r *http.Request, token string) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/invite/" + token
}

func (s *Server) renderUsers(w http.ResponseWriter, r *http.Request, d *usersData, errMsg string) {
	st := s.cat.Store
	var err error
	if d.Users, err = st.Users(); err == nil {
		d.Invites, err = st.PendingInvites()
	}
	if err != nil {
		errMsg = err.Error()
	}
	d.Me = userFrom(r)
	d.Roles = []string{store.RoleViewer, store.RoleEditor, store.RoleAdmin}
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusBadRequest
	}
	s.renderStatus(w, r, "users", &page{Title: "Users", Nav: "users", Error: errMsg, Data: d}, status)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) == nil {
		s.renderStatus(w, r, "notfound", &page{Title: "No accounts yet",
			Data: "Create the first admin account on the server with: archivis users add-admin NAME"}, http.StatusNotFound)
		return
	}
	s.renderUsers(w, r, &usersData{}, "")
}

// adminInvite creates an invitation and shows its link once.
func (s *Server) adminInvite(w http.ResponseWriter, r *http.Request) {
	me := userFrom(r)
	if me == nil {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	tok, hash, err := auth.NewToken()
	if err != nil {
		s.renderUsers(w, r, &usersData{}, err.Error())
		return
	}
	inv, err := s.cat.Store.CreateInvite(hash, r.PostFormValue("name"), r.PostFormValue("role"), 0, me.Name, InviteTTL)
	if err != nil {
		s.renderUsers(w, r, &usersData{}, err.Error())
		return
	}
	log.Printf("%s invited %s (%s)", me.Name, inv.Name, inv.Role)
	s.renderUsers(w, r, &usersData{Link: inviteLink(r, tok), LinkFor: inv.Name}, "")
}

func (s *Server) adminRevokeInvite(w http.ResponseWriter, r *http.Request) {
	if err := s.cat.Store.RevokeInvite(r.PostFormValue("invite")); err != nil {
		s.renderUsers(w, r, &usersData{}, err.Error())
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// adminUser changes one account: action = role|disable|enable|signout|reset.
func (s *Server) adminUser(w http.ResponseWriter, r *http.Request) {
	me := userFrom(r)
	id, ok := idParam(r, "id")
	if !ok || me == nil {
		http.NotFound(w, r)
		return
	}
	st := s.cat.Store
	u, err := st.UserByID(id)
	if err != nil {
		s.renderUsers(w, r, &usersData{}, err.Error())
		return
	}
	switch action := r.PostFormValue("action"); action {
	case "role":
		err = st.SetRole(id, r.PostFormValue("role"))
	case "disable":
		err = st.SetDisabled(id, true)
	case "enable":
		err = st.SetDisabled(id, false)
	case "signout":
		_, err = st.DeleteSessions(id)
	case "reset":
		tok, hash, terr := auth.NewToken()
		if terr != nil {
			err = terr
			break
		}
		if _, err = st.CreateInvite(hash, "", "", id, me.Name, InviteTTL); err == nil {
			log.Printf("%s made a password-reset link for %s", me.Name, u.Name)
			s.renderUsers(w, r, &usersData{Link: inviteLink(r, tok), LinkFor: u.Name}, "")
			return
		}
	default:
		err = errors.New("unknown action " + strconv.Quote(action))
	}
	if err != nil {
		s.renderUsers(w, r, &usersData{}, err.Error())
		return
	}
	log.Printf("%s: %s %s", me.Name, r.PostFormValue("action"), u.Name)
	if id == me.ID { // signed out, disabled or no longer an admin
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

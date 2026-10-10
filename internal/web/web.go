// Package web serves the archivis browser UI.
package web

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/auteursoft/archivis/internal/auth"
	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/quality"
	"github.com/auteursoft/archivis/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server is the HTTP UI.
type Server struct {
	cat  *catalog.Catalog
	Auth string // "user:password" for basic auth until the first account exists; empty = none

	hasUsers           atomic.Bool
	ipLimit, nameLimit *auth.Limiter

	tmpl map[string]*template.Template

	qmu     sync.Mutex
	queries map[string]*queryEntry

	dmu        sync.Mutex
	discovered []catalog.Cluster
	discAt     time.Time
}

type queryEntry struct {
	q  *catalog.QueryResult
	at time.Time
}

func New(c *catalog.Catalog) *Server {
	s := &Server{cat: c, queries: map[string]*queryEntry{}, tmpl: map[string]*template.Template{},
		// failed sign-ins allowed per 15 minutes, per address and per name
		ipLimit: auth.NewLimiter(20, 15*time.Minute), nameLimit: auth.NewLimiter(10, 15*time.Minute)}
	base := template.Must(template.New("base").Funcs(funcs).ParseFS(templateFS, "templates/layout.html"))
	pages, _ := fs.Glob(templateFS, "templates/*.html")
	for _, p := range pages {
		name := strings.TrimSuffix(filepath.Base(p), ".html")
		if name == "layout" {
			continue
		}
		s.tmpl[name] = template.Must(template.Must(base.Clone()).ParseFS(templateFS, p))
	}
	return s
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	st, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(st))))
	mux.HandleFunc("GET /{$}", s.handleBrowse)
	mux.HandleFunc("GET /photo/{id}", s.handlePhoto)
	mux.HandleFunc("GET /face/{id}", s.handleFace)
	mux.HandleFunc("POST /query", s.handleQueryUpload)
	mux.HandleFunc("GET /query/{tok}", s.handleQuery)
	mux.HandleFunc("GET /query/{tok}/img", s.handleQueryImage)
	mux.HandleFunc("GET /query/{tok}/face/{i}", s.handleQueryImage)
	mux.HandleFunc("GET /people", s.handlePeople)
	mux.HandleFunc("GET /person/{id}", s.handlePerson)
	mux.HandleFunc("GET /discover", s.handleDiscover)
	mux.HandleFunc("GET /bursts", s.handleBursts)
	mux.HandleFunc("GET /duplicates", s.handleDuplicates)
	mux.HandleFunc("GET /guide", s.handleGuide)
	mux.HandleFunc("GET /t/{fp}", s.handleThumb)
	mux.HandleFunc("GET /f/{fp}/{ord}", s.handleFaceThumb)
	mux.HandleFunc("GET /preview/{id}", s.handlePreview)
	mux.HandleFunc("GET /original/{id}", s.handleOriginal)
	mux.HandleFunc("POST /api/assign", s.apiAssign)
	mux.HandleFunc("POST /api/unassign", s.apiUnassign)
	mux.HandleFunc("POST /api/person/{id}/rename", s.apiRename)
	mux.HandleFunc("POST /api/person/{id}/delete", s.apiDeletePerson)
	mux.HandleFunc("POST /api/match", s.apiMatch)
	mux.HandleFunc("POST /api/refresh", s.apiRefresh)
	mux.HandleFunc("POST /api/photo/{id}/aesthetic", s.apiAestheticFeedback)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /invite/{tok}", s.handleInvite)
	mux.HandleFunc("POST /invite/{tok}", s.handleInvite)
	mux.HandleFunc("GET /admin/users", s.handleUsers)
	mux.HandleFunc("POST /admin/invite", s.adminInvite)
	mux.HandleFunc("POST /admin/invite/revoke", s.adminRevokeInvite)
	mux.HandleFunc("POST /admin/users/{id}", s.adminUser)
	var h http.Handler = mux
	h = s.authenticate(h)
	h = sameOrigin(h)
	return logRequests(securityHeaders(h))
}

// securityHeaders forbids framing (clickjacking the admin page) and MIME
// sniffing, and keeps full URLs out of Referer headers sent elsewhere.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// sameOrigin blocks cross-site request forgery: a state-changing request
// must come from archivis's own pages. Browsers send Sec-Fetch-Site (and
// Origin) on such requests, so a page on another site cannot delete people
// or relabel faces through a visitor's browser, even with Basic Auth
// credentials cached. API calls must also be JSON, which a cross-site HTML
// form cannot send. Requests without these headers (curl, scripts) are
// allowed: they do not carry a victim's browser credentials.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		} else if origin == "null" {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			ct := r.Header.Get("Content-Type")
			if mt, _, _ := mime.ParseMediaType(ct); mt != "application/json" {
				http.Error(w, "API requests must be JSON", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe serves until the process exits, refreshing the in-memory
// indexes periodically so photos indexed meanwhile become searchable.
func (s *Server) ListenAndServe(addr string) error {
	go func() {
		for range time.Tick(time.Minute) {
			if err := s.cat.Refresh(); err != nil {
				log.Printf("refresh: %v", err)
			}
			s.expireQueries()
		}
	}()
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/t/") && !strings.HasPrefix(r.URL.Path, "/f/") && !strings.HasPrefix(r.URL.Path, "/static/") {
			uri := r.URL.RequestURI()
			if strings.HasPrefix(r.URL.Path, "/invite/") {
				uri = "/invite/…" // the token signs someone up or resets a password
			}
			log.Printf("%s %s %s", r.Method, uri, time.Since(start).Round(time.Millisecond))
		}
	})
}

// ---- template helpers ---------------------------------------------------

var funcs = template.FuncMap{
	// plural: plural 1 "photo" -> "1 photo", plural 3 "photo" -> "3 photos"
	"plural": func(n int, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return fmt.Sprintf("%d %ss", n, word)
	},
	// seq: seq 1 3 -> [1 2 3]
	"seq": func(a, b int) []int {
		var out []int
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
		return out
	},
	"f0":  func(v float64) string { return fmt.Sprintf("%.0f", v) },
	"f1":  func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"f2":  func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"f3":  func(v float32) string { return fmt.Sprintf("%.3f", v) },
	"pct": func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) },
	"pct1": func(v float64) string {
		return fmt.Sprintf("%.1f%%", v*100)
	},
	"date": func(t int64) string {
		if t <= 0 {
			return ""
		}
		return time.Unix(t, 0).UTC().Format("2 Jan 2006 15:04")
	},
	"scoreClass": func(v float64) string {
		switch {
		case v >= 75:
			return "good"
		case v >= 50:
			return "ok"
		default:
			return "bad"
		}
	},
	"unitClass": func(v float64) string {
		switch {
		case v >= 0.75:
			return "good"
		case v >= 0.45:
			return "ok"
		default:
			return "bad"
		}
	},
	"castLabel": quality.CastLabel,
	"base":      filepath.Base,
	"dir":       filepath.Dir,
	"shutter": func(t float64) string {
		if t <= 0 {
			return ""
		}
		if t >= 1 {
			return fmt.Sprintf("%gs", math.Round(t*10)/10)
		}
		return fmt.Sprintf("1/%.0fs", 1/t)
	},
	"pos":   func(v float64) string { return fmt.Sprintf("%.3f%%", v*100) },
	"add":   func(a, b int) int { return a + b },
	"sub":   func(a, b int) int { return a - b },
	"q":     func(v any) string { return template.URLQueryEscaper(v) },
	"json":  func(v any) template.JS { b, _ := json.Marshal(v); return template.JS(b) },
	"faceW": func(f store.Face) float64 { return f.X2 - f.X1 },
	"sub1":  func(a, b float64) float64 { return a - b },
	"pcount": func(p store.Photo) string {
		return fmt.Sprintf("%d×%d", p.Width, p.Height)
	},
}

type page struct {
	Title  string
	Nav    string
	Stats  store.Stats
	Filter store.Filter
	Query  string
	Error  string
	Data   any
	People []store.Person
	Tags   []store.Tag
	Params map[string]string

	User    *store.User // signed in; nil without accounts
	CanEdit bool        // may change labels, rate and download originals
	Admin   bool        // may manage accounts
	Bare    bool        // sign-in pages: no navigation or catalogue figures
}

// who fills in the signed-in account and what it may do.
func (p *page) who(r *http.Request) {
	p.User = userFrom(r)
	p.CanEdit = p.User == nil || store.RoleAtLeast(p.User.Role, store.RoleEditor)
	p.Admin = p.User != nil && p.User.Role == store.RoleAdmin
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *page) {
	s.renderStatus(w, r, name, p, http.StatusOK)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, name string, p *page, status int) {
	p.who(r)
	if !p.Bare {
		p.Stats, _ = s.cat.Store.Stats()
	}
	t, ok := s.tmpl[name]
	if !ok {
		http.Error(w, "no template "+name, 500)
		return
	}
	// Render to a buffer so a template error is a 500, not a truncated 200.
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "could not render this page: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// notFound renders a friendly 404 page.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request, msg string) {
	s.renderStatus(w, r, "notfound", &page{Title: "Not found", Data: msg}, http.StatusNotFound)
}

func idParam(r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return v, err == nil && v > 0
}

// fval reads a numeric query parameter; anything non-finite ("NaN",
// "Inf") counts as absent, since NaN would make every comparison false.
func fval(r *http.Request, k string) float64 {
	v, err := strconv.ParseFloat(r.URL.Query().Get(k), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func parseFilter(r *http.Request) store.Filter {
	q := r.URL.Query()
	f := store.Filter{
		MinOverall:    fval(r, "min_overall"),
		MinFocus:      fval(r, "min_focus"),
		MinExposure:   fval(r, "min_exposure"),
		MinAesthetic:  fval(r, "min_aesthetic"),
		MaxCast:       fval(r, "max_cast"),
		MaxBlockiness: fval(r, "max_jpeg"),
		Camera:        q.Get("camera"),
		Lens:          q.Get("lens"),
		PathContains:  q.Get("path"),
		Tag:           q.Get("tag"),
		Faces:         q.Get("faces"),
		Mono:          q.Get("mono"),
		Sort:          q.Get("sort"),
		Limit:         120,
	}
	f.Year, _ = strconv.Atoi(q.Get("year"))
	f.PersonID, _ = strconv.ParseInt(q.Get("person"), 10, 64)
	pg, _ := strconv.Atoi(q.Get("page"))
	if pg > 0 {
		f.Offset = min(pg, maxPage) * f.Limit // bounded: no overflow, no huge result buffers
	}
	return f
}

// maxPage bounds paging (120 photos a page): far beyond what anyone pages
// through, and it keeps a text search's candidate list bounded.
const maxPage = 1000

// ---- pages --------------------------------------------------------------

// pageErrs collects failures of parts of a page, so a failed lookup is
// shown as an error rather than looking like "nothing found".
type pageErrs []string

func (e *pageErrs) note(what string, err error) {
	if err != nil {
		*e = append(*e, what+": "+err.Error())
	}
}

func (e pageErrs) String() string {
	if len(e) == 0 {
		return ""
	}
	return "Part of this page could not be loaded — " + strings.Join(e, "; ")
}

type browseData struct {
	Photos   []catalog.PhotoHit
	Total    int
	Page     int
	HasMore  bool
	Searched bool
	QS       string // query string without page
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	text := strings.TrimSpace(r.URL.Query().Get("q"))
	p := &page{Title: "Photos", Nav: "browse", Filter: f, Query: text, Params: map[string]string{}}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			p.Params[k] = v[0]
		}
	}
	p.People, _ = s.cat.Store.People()
	p.Tags, _ = s.cat.Store.TagCounts()
	d := &browseData{Page: f.Offset / f.Limit}
	qs := r.URL.Query()
	qs.Del("page")
	d.QS = qs.Encode()
	if text != "" {
		d.Searched = true
		hits, err := s.cat.SearchText(text, f, f.Offset+f.Limit)
		if err != nil {
			p.Error = err.Error()
		} else {
			catalog.SortHits(hits, f.Sort) // "Best match" (relevance) unless another order is chosen
			if f.Offset < len(hits) {
				hits = hits[f.Offset:]
			} else {
				hits = nil
			}
			d.Photos = hits
			d.Total = len(hits)
			d.HasMore = len(hits) == f.Limit
		}
	} else {
		ps, total, err := s.cat.Store.Browse(f)
		if err != nil {
			p.Error = err.Error()
		}
		for _, ph := range ps {
			d.Photos = append(d.Photos, catalog.PhotoHit{Photo: ph})
		}
		d.Total = total
		d.HasMore = f.Offset+len(ps) < total
	}
	p.Data = d
	s.render(w, r, "browse", p)
}

type faceView struct {
	store.Face
	Photo  store.Photo
	Person string
	Score  float32
}

type photoData struct {
	Photo   store.Photo
	Faces   []faceView
	Tags    []store.Tag
	Similar []catalog.PhotoHit
	Metrics []metricRow
	Cast    string
	// Feedback is this viewer's latest judgement of the aesthetic score.
	Feedback *store.Feedback
}

type metricRow struct {
	Name, Value, Note string
	Score             float64 // 0..1 or -1 for no bar
}

func (s *Server) handlePhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		s.notFound(w, r, "That photo is not in the catalogue (it may have been removed or re-indexed).")
		return
	}
	ph, err := s.cat.Store.Photo(id)
	if err != nil || ph == nil {
		s.notFound(w, r, "That photo is not in the catalogue (it may have been removed or re-indexed).")
		return
	}
	d := &photoData{Photo: *ph}
	var errs pageErrs
	faces, err := s.cat.Store.FacesForPhoto(id)
	errs.note("faces", err)
	names := s.personNames()
	for _, f := range faces {
		d.Faces = append(d.Faces, faceView{Face: f, Photo: *ph, Person: names[f.PersonID.Int64]})
	}
	d.Tags, err = s.cat.Store.TagsForPhoto(id)
	errs.note("tags", err)
	d.Similar, err = s.cat.SimilarPhotos(id, store.Filter{}, 12)
	if _, has := s.cat.PhotoIndex().Vector(id); has { // no embedding is not a failure
		errs.note("similar photos", err)
	}
	d.Cast = quality.CastLabel(ph.CastA, ph.CastB, ph.CastStrength)
	d.Metrics = metricsFor(ph, d.Cast)
	if ph.Aesthetic.Valid {
		sc, err := s.cat.Store.AestheticScores(id)
		errs.note("aesthetic scores", err)
		if note := s.blendNote(sc); note != "" {
			for i := range d.Metrics {
				if d.Metrics[i].Name == "Aesthetic" {
					d.Metrics[i].Note += "; " + note
				}
			}
		}
	}
	d.Feedback, err = s.cat.Store.PhotoFeedback(id, rater(r))
	errs.note("your rating", err)
	p := &page{Title: filepath.Base(ph.Path), Nav: "browse", Data: d, Error: errs.String()}
	p.People, _ = s.cat.Store.People()
	s.render(w, r, "photo", p)
}

// blendNote describes a photo's aesthetic score when several models
// contribute to it, e.g. "blend of eva-ridge 6.1, mine 6.8".
func (s *Server) blendNote(scores map[string]float64) string {
	active, err := indexer.ActiveAesthetics(s.cat.Store)
	if err != nil || len(active) < 2 {
		return ""
	}
	var parts []string
	for _, m := range active {
		if v, ok := scores[m.ID]; ok {
			parts = append(parts, fmt.Sprintf("%s %.1f", m.Name, v))
		}
	}
	if len(parts) < 2 {
		return ""
	}
	return "blend of " + strings.Join(parts, ", ")
}

// rater identifies who gave feedback: the account, the basic-auth user,
// or "local".
func rater(r *http.Request) string {
	if u := userFrom(r); u != nil {
		return u.Name
	}
	if u, _, ok := r.BasicAuth(); ok && u != "" {
		return u
	}
	return "local"
}

// apiAestheticFeedback records a viewer's judgement of a photo's aesthetic
// score: {"rating": 1-10} and/or {"verdict": "agree"|"high"|"low"}. It is
// kept, with the score shown and the models behind it, to train future
// aesthetic models (archivis aesthetic train).
func (s *Server) apiAestheticFeedback(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	var req struct {
		Rating  *float64 `json:"rating"`
		Verdict string   `json:"verdict"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErr(w, err)
		return
	}
	ph, err := s.cat.Store.Photo(id)
	if err != nil || ph == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": store.ErrNoPhoto.Error()})
		return
	}
	f := store.Feedback{PhotoID: id, Rater: rater(r), Verdict: req.Verdict}
	if req.Rating != nil {
		if *req.Rating < 1 || *req.Rating > 10 {
			jsonErr(w, fmt.Errorf("rating must be between 1 and 10"))
			return
		}
		f.Rating = sql.NullFloat64{Float64: *req.Rating, Valid: true}
	}
	if f.Verdict != "" && !ph.Aesthetic.Valid {
		jsonErr(w, fmt.Errorf("this photo has no aesthetic score to judge; rate it instead"))
		return
	}
	f.Shown = ph.Aesthetic.Float64
	if active, err := indexer.ActiveAesthetics(s.cat.Store); err == nil {
		f.Models = indexer.DescribeBlend(active)
	}
	fid, err := s.cat.Store.AddFeedback(f)
	if err != nil {
		jsonErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": fid})
}

func metricsFor(p *store.Photo, cast string) []metricRow {
	rows := []metricRow{
		{"Overall", fmt.Sprintf("%.0f / 100", p.Overall), "technical quality blended with aesthetics", p.Overall / 100},
		{"Technical", fmt.Sprintf("%.0f / 100", p.Technical), "predicted human quality rating (model trained on 10,000 rated photos)", p.Technical / 100},
	}
	if p.Aesthetic.Valid {
		rows = append(rows, metricRow{"Aesthetic", fmt.Sprintf("%.1f / 10", p.Aesthetic.Float64), "predicted average rating by people judging appeal (most photos score 4–7.5)", ml.AestheticPercent(p.Aesthetic.Float64) / 100})
	}
	focusNote := "contrast-normalised edge energy of the sharpest regions"
	if p.FaceSharpness.Valid {
		focusNote += fmt.Sprintf("; main face %.0f", p.FaceSharpness.Float64)
	}
	rows = append(rows,
		metricRow{"Focus", fmt.Sprintf("%.0f%%", p.FocusScore*100), focusNote, p.FocusScore},
		metricRow{"Exposure", fmt.Sprintf("%.0f%%", p.ExposureScore*100), fmt.Sprintf("mean %.0f%%, clipped highlights %.1f%%, crushed shadows %.1f%%, range %.0f%%", p.Brightness*100, p.Highlights*100, p.Shadows*100, p.DynamicRange*100), p.ExposureScore},
	)
	if p.Monochrome {
		rows = append(rows, metricRow{"White balance", "monochrome", "black & white image", 1})
	} else {
		rows = append(rows, metricRow{"White balance", fmt.Sprintf("%.0f%%", p.ColorScore*100), fmt.Sprintf("%s cast, %.1f° from neutral (grey-edge estimate)", cast, p.CastStrength), p.ColorScore})
	}
	rows = append(rows,
		metricRow{"Noise", fmt.Sprintf("%.0f%%", p.NoiseScore*100), fmt.Sprintf("estimated σ %.2f levels", p.Noise), p.NoiseScore},
		metricRow{"Compression", fmt.Sprintf("%.2f", p.Blockiness), blockNote(p.Blockiness), -1},
		metricRow{"Contrast", fmt.Sprintf("%.2f", p.Contrast), "RMS contrast of luminance", -1},
		metricRow{"Colourfulness", fmt.Sprintf("%.0f", p.Colorfulness), "Hasler–Süsstrunk (≈0 grey, 33 moderate, 60+ vivid)", -1},
	)
	return rows
}

func blockNote(b float64) string {
	switch {
	case b < 1.15:
		return "no visible JPEG block artefacts"
	case b < 1.5:
		return "mild JPEG compression artefacts"
	default:
		return "heavy JPEG compression: visible 8×8 blocks"
	}
}

func (s *Server) personNames() map[int64]string {
	ps, _ := s.cat.Store.People()
	m := map[int64]string{}
	for _, p := range ps {
		m[p.ID] = p.Name
	}
	return m
}

type faceData struct {
	Face    faceView
	Matches []faceView
	Min     float64
	Photos  int  // distinct photos among Matches
	AtLeast bool // Matches hit the search limit, so there may be more
}

// faceLimit and queryLimit cap how many similar faces a page lists.
const faceLimit, queryLimit = 400, 200

// matchPhotos counts the distinct photos among matched faces, leaving out
// photo `except` (0 = none), and whether the list was cut off at limit (so
// the true number may be higher).
func matchPhotos(fs []faceView, limit int, except int64) (int, bool) {
	seen := map[int64]bool{}
	for _, f := range fs {
		if f.PhotoID != except {
			seen[f.PhotoID] = true
		}
	}
	return len(seen), len(fs) >= limit
}

func (s *Server) handleFace(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		s.notFound(w, r, "That face is not in the catalogue. Faces are re-detected when a photo is re-indexed, so old links can go stale.")
		return
	}
	fs, err := s.cat.Store.FacesByID([]int64{id})
	if err != nil || len(fs) == 0 {
		s.notFound(w, r, "That face is not in the catalogue. Faces are re-detected when a photo is re-indexed, so old links can go stale.")
		return
	}
	ph, err := s.cat.Store.Photo(fs[0].PhotoID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if ph == nil {
		s.notFound(w, r, "That face's photo is no longer in the catalogue.")
		return
	}
	names := s.personNames()
	min := fval(r, "min")
	if min <= 0 {
		min = 0.35
	}
	d := &faceData{Face: faceView{Face: fs[0], Photo: *ph, Person: names[fs[0].PersonID.Int64]}, Min: min}
	hits, err := s.cat.SearchFaces(s.cat.FaceVectors([]int64{id}), faceLimit, float32(min), func(fid int64) bool { return fid == id })
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for _, h := range hits {
		d.Matches = append(d.Matches, faceView{Face: h.Face, Photo: h.Photo, Person: names[h.PersonID.Int64], Score: h.Score})
	}
	d.Photos, d.AtLeast = matchPhotos(d.Matches, faceLimit, d.Face.PhotoID)
	p := &page{Title: "Similar faces", Nav: "people", Data: d}
	p.People, _ = s.cat.Store.People()
	s.render(w, r, "face", p)
}

// ---- query by uploaded photo ------------------------------------------

// analyzeSlots bounds concurrent upload analyses (each decodes a full image
// and runs the models), so a burst of uploads queues instead of exhausting
// memory.
var analyzeSlots = make(chan struct{}, 2)

func (s *Server) handleQueryUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 200<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "no image uploaded", 400)
		return
	}
	// Stream the file to disk rather than holding it (up to 200 MB) in memory.
	var tmpPath string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "image" {
			part.Close()
			continue
		}
		tmp, err := os.CreateTemp("", "archivis-upload-*")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		tmpPath = tmp.Name()
		_, err = io.Copy(tmp, part)
		tmp.Close()
		part.Close()
		if err != nil {
			os.Remove(tmpPath)
			http.Error(w, "upload failed: "+err.Error(), 400)
			return
		}
		break
	}
	if tmpPath == "" {
		http.Error(w, "no image uploaded", 400)
		return
	}
	defer os.Remove(tmpPath)
	select {
	case analyzeSlots <- struct{}{}:
		defer func() { <-analyzeSlots }()
	case <-r.Context().Done():
		return
	}
	q, err := s.cat.AnalyzeQueryFile(tmpPath)
	if err != nil {
		s.render(w, r, "browse", &page{Title: "Search", Nav: "browse", Error: err.Error(), Data: &browseData{}})
		return
	}
	b := make([]byte, 8)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	s.cacheQuery(tok, q)
	http.Redirect(w, r, "/query/"+tok, http.StatusSeeOther)
}

// maxQueries bounds the upload cache: entries are small once the decoded
// image is dropped, but uploads must not grow the heap without limit.
const maxQueries = 50

// cacheQuery keeps an analysed upload for its result page, dropping the
// decoded image (the page needs only faces, embedding, metrics and the small
// JPEGs) and evicting the oldest entry when the cache is full.
func (s *Server) cacheQuery(tok string, q *catalog.QueryResult) {
	if q.Analysis != nil {
		q.Analysis.Decoded = nil
		for i := range q.Analysis.Faces {
			q.Analysis.Faces[i].Aligned = nil // the page uses the small JPEG crops
		}
	}
	s.qmu.Lock()
	defer s.qmu.Unlock()
	for len(s.queries) >= maxQueries {
		oldest := ""
		for k, v := range s.queries {
			if oldest == "" || v.at.Before(s.queries[oldest].at) {
				oldest = k
			}
		}
		delete(s.queries, oldest)
	}
	s.queries[tok] = &queryEntry{q, time.Now()}
}

func (s *Server) expireQueries() {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	for k, v := range s.queries {
		if time.Since(v.at) > 2*time.Hour {
			delete(s.queries, k)
		}
	}
}

type queryFace struct {
	Index   int
	Matches []faceView
	Photos  int  // distinct photos among Matches
	AtLeast bool // Matches hit the search limit
}

type queryData struct {
	Token   string
	Faces   []queryFace
	Similar []catalog.PhotoHit
	Metrics []metricRow
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("tok")
	s.qmu.Lock()
	e := s.queries[tok]
	s.qmu.Unlock()
	if e == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a := e.q.Analysis
	d := &queryData{Token: tok}
	names := s.personNames()
	min := fval(r, "min")
	if min <= 0 {
		min = 0.35
	}
	var errs pageErrs
	for i, f := range a.Faces {
		hits, err := s.cat.SearchFaces([][]float32{f.Embedding}, queryLimit, float32(min), nil)
		errs.note(fmt.Sprintf("face %d search", i+1), err)
		qf := queryFace{Index: i}
		for _, h := range hits {
			qf.Matches = append(qf.Matches, faceView{Face: h.Face, Photo: h.Photo, Person: names[h.PersonID.Int64], Score: h.Score})
		}
		qf.Photos, qf.AtLeast = matchPhotos(qf.Matches, queryLimit, 0)
		d.Faces = append(d.Faces, qf)
	}
	if a.CLIP != nil {
		var err error
		d.Similar, err = s.cat.SearchVector(a.CLIP, store.Filter{}, 48)
		errs.note("similar scenes", err)
	}
	ph := a.Result.Photo
	d.Metrics = metricsFor(&ph, quality.CastLabel(ph.CastA, ph.CastB, ph.CastStrength))
	s.render(w, r, "query", &page{Title: "Search by photo", Nav: "browse", Data: d, Error: errs.String()})
}

func (s *Server) handleQueryImage(w http.ResponseWriter, r *http.Request) {
	s.qmu.Lock()
	e := s.queries[r.PathValue("tok")]
	s.qmu.Unlock()
	if e == nil {
		http.NotFound(w, r)
		return
	}
	b := e.q.Thumb
	if is := r.PathValue("i"); is != "" {
		i, err := strconv.Atoi(is)
		if err != nil || i < 0 || i >= len(e.q.FaceJPG) {
			http.NotFound(w, r)
			return
		}
		b = e.q.FaceJPG[i]
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(b)
}

// ---- people -------------------------------------------------------------

type personData struct {
	Person      store.Person
	Faces       []faceView
	Suggestions []faceView
	Min         float64
}

func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	p := &page{Title: "People", Nav: "people"}
	ps, err := s.cat.Store.People()
	if err != nil {
		p.Error = err.Error()
	}
	type card struct {
		store.Person
		Cover *faceView
	}
	var cards []card
	for _, x := range ps {
		c := card{Person: x}
		if x.CoverFace > 0 {
			if fs, _ := s.cat.Store.FacesByID([]int64{x.CoverFace}); len(fs) == 1 {
				if ph, _ := s.cat.Store.Photo(fs[0].PhotoID); ph != nil {
					c.Cover = &faceView{Face: fs[0], Photo: *ph}
				}
			}
		}
		cards = append(cards, c)
	}
	p.Data = cards
	s.render(w, r, "people", p)
}

func (s *Server) facesWithPhotos(fs []store.Face, names map[int64]string) []faceView {
	var ids []int64
	for _, f := range fs {
		ids = append(ids, f.PhotoID)
	}
	photos, _ := s.cat.Store.Photos(ids)
	byID := map[int64]store.Photo{}
	for _, p := range photos {
		byID[p.ID] = p
	}
	out := make([]faceView, 0, len(fs))
	for _, f := range fs {
		out = append(out, faceView{Face: f, Photo: byID[f.PhotoID], Person: names[f.PersonID.Int64], Score: float32(f.PersonSim.Float64)})
	}
	return out
}

func (s *Server) handlePerson(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		s.notFound(w, r, "That person does not exist (they may have been merged or deleted).")
		return
	}
	per, err := s.cat.Store.Person(id)
	if err != nil || per == nil {
		s.notFound(w, r, "That person does not exist (they may have been merged or deleted).")
		return
	}
	names := s.personNames()
	d := &personData{Person: *per}
	var errs pageErrs
	fs, err := s.cat.Store.LabeledFaces(id, false)
	errs.note("labelled faces", err)
	// every face already labelled as them, before the display list is cut
	labelled := make(map[int64]bool, len(fs))
	for _, f := range fs {
		labelled[f.ID] = true
	}
	// confirmed first, then auto by similarity
	sort.SliceStable(fs, func(i, j int) bool {
		mi, mj := fs[i].PersonSource.String == "manual", fs[j].PersonSource.String == "manual"
		if mi != mj {
			return mi
		}
		return fs[i].PersonSim.Float64 > fs[j].PersonSim.Float64
	})
	if len(fs) > 600 {
		fs = fs[:600]
	}
	d.Faces = s.facesWithPhotos(fs, names)
	d.Min = fval(r, "min")
	if d.Min <= 0 {
		d.Min = 0.35
	}
	// Suggestions: faces similar to confirmed ones, not yet labelled as them.
	manual, err := s.cat.Store.LabeledFaces(id, true)
	errs.note("confirmed faces", err)
	var mids []int64
	for _, f := range manual {
		mids = append(mids, f.ID)
	}
	if len(mids) > 0 {
		// Each exemplar costs a full scan, so use a few mutually
		// dissimilar ones (covering ages, angles, lighting).
		vecs := catalog.Diverse(s.cat.FaceVectors(mids), 8)
		hits, err := s.cat.SearchFaces(vecs, 120, float32(d.Min), func(fid int64) bool { return labelled[fid] })
		errs.note("suggestions", err)
		for _, h := range hits {
			if h.PersonSource.String == "manual" {
				continue // confirmed as someone else
			}
			d.Suggestions = append(d.Suggestions, faceView{Face: h.Face, Photo: h.Photo, Person: names[h.PersonID.Int64], Score: h.Score})
		}
	}
	p := &page{Title: per.Name, Nav: "people", Data: d, Error: errs.String()}
	p.People, _ = s.cat.Store.People()
	s.render(w, r, "person", p)
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	s.dmu.Lock()
	if r.URL.Query().Get("refresh") != "" || s.discAt.IsZero() || time.Since(s.discAt) > 10*time.Minute {
		cl, err := s.cat.Discover(5000, 3, 0.5)
		if err != nil {
			s.dmu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
		s.discovered, s.discAt = cl, time.Now()
	}
	clusters := s.discovered
	s.dmu.Unlock()
	type cv struct {
		Photos int // distinct photos the cluster's faces are in
		Faces  []faceView
	}
	var out []cv
	names := s.personNames()
	for _, c := range clusters {
		out = append(out, cv{c.Photos, s.facesWithPhotos(c.Faces, names)})
	}
	p := &page{Title: "Discover people", Nav: "discover", Data: out}
	p.People, _ = s.cat.Store.People()
	s.render(w, r, "discover", p)
}

func (s *Server) handleBursts(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	f.Limit = 60
	gap, _ := strconv.ParseInt(r.URL.Query().Get("gap"), 10, 64)
	if gap <= 0 {
		gap = 3
	}
	bs, err := s.cat.Store.Bursts(f, gap, 3)
	p := &page{Title: "Bursts", Nav: "bursts", Data: bs, Params: map[string]string{"gap": fmt.Sprint(gap)}}
	if err != nil {
		p.Error = err.Error()
	}
	s.render(w, r, "bursts", p)
}

func (s *Server) handleDuplicates(w http.ResponseWriter, r *http.Request) {
	gs, err := s.cat.Store.DuplicateGroups(200)
	p := &page{Title: "Duplicates", Nav: "duplicates", Data: gs}
	if err != nil {
		p.Error = err.Error()
	}
	s.render(w, r, "duplicates", p)
}

func (s *Server) handleGuide(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "guide", &page{Title: "How scores work", Nav: "guide"})
}

// ---- images -------------------------------------------------------------

// fpRe matches content fingerprints: 32 hex digits (full-content SHA-256,
// indexer.Fingerprint) or 24 for photos catalogued before that.
var fpRe = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{24})$`)

// serveCached serves an image the browser may keep. "private": a shared
// cache (a proxy) must not hand one person's photos to another request,
// which would skip the sign-in check.
func serveCached(w http.ResponseWriter, r *http.Request, path string) {
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, path)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	fp := strings.TrimSuffix(r.PathValue("fp"), ".jpg")
	if !fpRe.MatchString(fp) {
		http.NotFound(w, r)
		return
	}
	serveCached(w, r, s.cat.Layout.Thumb(fp))
}

func (s *Server) handleFaceThumb(w http.ResponseWriter, r *http.Request) {
	fp := r.PathValue("fp")
	ord, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("ord"), ".jpg"))
	if !fpRe.MatchString(fp) || err != nil || ord < 0 {
		http.NotFound(w, r)
		return
	}
	serveCached(w, r, s.cat.Layout.FaceThumb(fp, ord))
}

// handlePreview renders a large JPEG (cached on disk) of any supported format.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ph, err := s.cat.Store.Photo(id)
	if err != nil || ph == nil {
		http.NotFound(w, r)
		return
	}
	cache := filepath.Join(s.cat.Layout.Dir, "previews", ph.Fingerprint[:2], ph.Fingerprint+".jpg")
	if _, err := os.Stat(cache); err != nil {
		d, err := imageio.Load(ph.Path, 1800)
		if err != nil {
			// The original is offline: show the thumbnail, but don't let the
			// browser keep it as the preview once the drive is back.
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, s.cat.Layout.Thumb(ph.Fingerprint))
			return
		}
		b, err := imageio.EncodeJPEG(d.Img, 88)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// Written atomically: a concurrent request or a crash never sees
		// (or leaves) a half-written preview.
		err = os.MkdirAll(filepath.Dir(cache), 0o755)
		if err == nil {
			err = indexer.WriteAtomic(cache, b)
		}
		if err != nil {
			log.Printf("preview cache %s: %v (serving uncached)", cache, err)
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(b)
			return
		}
	}
	serveCached(w, r, cache)
}

func (s *Server) handleOriginal(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ph, err := s.cat.Store.Photo(id)
	if err != nil || ph == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", filepath.Base(ph.Path)))
	http.ServeFile(w, r, ph.Path)
}

// ---- JSON API -----------------------------------------------------------

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(400)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) apiAssign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string  `json:"name"`
		Faces []int64 `json:"faces"`
		Match bool    `json:"match"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Name) == "" || len(req.Faces) == 0 {
		jsonErr(w, fmt.Errorf("need a name and at least one face"))
		return
	}
	// one transaction: unknown face ids change nothing and create no person
	id, err := s.cat.Store.LabelFaces(req.Name, req.Faces)
	if err != nil {
		if errors.Is(err, store.ErrNoFace) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		jsonErr(w, err)
		return
	}
	s.labelsChanged()
	resp := map[string]any{"person": id, "assigned": len(req.Faces), "auto": 0}
	if req.Match {
		// The manual labels are already saved, so a matching failure is a
		// partial success: report it rather than fail the whole request.
		n, err := s.cat.AutoMatch(catalog.MatchOptions{Threshold: catalog.DefaultMatch.Threshold, Margin: catalog.DefaultMatch.Margin, PersonID: id})
		resp["auto"] = n
		if err != nil {
			log.Printf("auto-match for person %d: %v", id, err)
			resp["auto_error"] = err.Error()
		}
		if n > 0 {
			s.labelsChanged()
		}
	}
	writeJSON(w, resp)
}

// labelsChanged drops the cached Discover clusters: they are built from
// unlabelled faces, so any labelling change makes them stale.
func (s *Server) labelsChanged() {
	s.dmu.Lock()
	s.discAt = time.Time{}
	s.dmu.Unlock()
}

func (s *Server) apiUnassign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Faces []int64 `json:"faces"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErr(w, err)
		return
	}
	if err := s.cat.Store.UnassignFaces(req.Faces); err != nil {
		jsonErr(w, err)
		return
	}
	s.labelsChanged()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiRename(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErr(w, err)
		return
	}
	if err := s.cat.Store.RenamePerson(id, req.Name); err != nil {
		if errors.Is(err, store.ErrNoPerson) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		jsonErr(w, err)
		return
	}
	p, err := s.cat.Store.PersonByName(req.Name)
	if err != nil || p == nil {
		jsonErr(w, fmt.Errorf("renamed person not found"))
		return
	}
	writeJSON(w, map[string]any{"person": p.ID})
}

func (s *Server) apiDeletePerson(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	if err := s.cat.Store.DeletePerson(id); err != nil {
		if errors.Is(err, store.ErrNoPerson) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		jsonErr(w, err)
		return
	}
	s.labelsChanged()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiMatch(w http.ResponseWriter, r *http.Request) {
	n, err := s.cat.AutoMatch(catalog.DefaultMatch)
	if n > 0 {
		s.labelsChanged()
	}
	if err != nil {
		jsonErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"auto": n})
}

func (s *Server) apiRefresh(w http.ResponseWriter, r *http.Request) {
	if err := s.cat.Refresh(); err != nil {
		jsonErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"photos": s.cat.PhotoIndex().Len(), "faces": s.cat.FaceIndex().Len()})
}

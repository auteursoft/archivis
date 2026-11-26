package web

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/store"
)

// regroupEvery is the least time between regroups while photos keep
// arriving (an index run regroups when it finishes anyway).
const regroupEvery = 30 * time.Minute

// regroup brings the bursts, stacks, sets of duplicates and folders
// up to date in the background, if they are stale; now skips the wait (the rules
// changed). One runs at a time.
func (s *Server) regroup(now bool) {
	st := s.cat.Store
	o := st.GroupOptions()
	now = now || st.GroupRulesChanged(o) // new rules (or never grouped): don't wait
	if !now && time.Since(time.Unix(s.lastRegroup.Load(), 0)) < regroupEvery {
		return
	}
	if (!st.GroupsStale(o) && !st.FoldersStale()) || !s.regrouping.CompareAndSwap(false, true) {
		return
	}
	defer s.regrouping.Store(false)
	s.lastRegroup.Store(time.Now().Unix())
	ctx := context.Background()
	if c := s.ctx.Load(); c != nil {
		ctx = *c
	}
	if ctx.Err() != nil {
		return // shutting down
	}
	stats, ran, err := indexer.Regroup(ctx, s.cat.Layout.Dir, st, o, false)
	switch {
	case err != nil && ctx.Err() == nil: // stopping at shutdown is not a failure
		log.Printf("grouping bursts and duplicates: %v", err)
	case ran:
		log.Printf("grouped: %s", stats)
	}
}

// regroupInBackground starts regroup(now) without waiting for it.
func (s *Server) regroupInBackground(now bool) {
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		s.regroup(now)
	}()
}

// groupPage describes a page of groups: Bursts or Duplicates.
type groupPage struct {
	kind    int
	path    string
	title   string
	minSize int // members a group needs to be listed
	perPage int
	summary bool // a card per group: load only its best shown member
}

var (
	burstsPage     = groupPage{store.GroupBurst, "/bursts", "Bursts", 3, 30, false}
	duplicatesPage = groupPage{store.GroupDuplicate, "/duplicates", "Duplicates", 2, 48, true}
)

type groupView struct {
	ID       int64
	Size     int // members in all
	Matching int // members matching the filters
	Taken    int64
	Camera   string
	Photos   []catalog.PhotoHit // the members shown (matching the filters), best first
}

// Top is the group's best shown member.
func (g groupView) Top() catalog.PhotoHit { return g.Photos[0] }

type groupsData struct {
	Groups     []groupView
	Page       int
	Prev, Next template.URL
	Filtered   bool   // filters are set: groups show only matching members
	Regrouping bool   // groups are being worked out now
	Stale      bool   // groups were worked out by other rules, or never
	Partial    bool   // the search for matching groups stopped early; more may match
	NoMemes    bool   // memes and screenshots are left out (the default)
	Album      *album // the album the page is limited to (nil: none)
	Tabs       []tab  // its photos, bursts and duplicates
}

// handleGroups serves a page of bursts or sets of duplicates, with the
// Photos page's filters and sorts: a group is listed if any member matches,
// with just the matching members, ranked by its best (worst, for "worst
// first") member. With dir (or iphone), as an album's page, only the
// groups with a member in that album, with just those members.
func (s *Server) handleGroups(gp groupPage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := parseFilter(r)
		var errs []string
		fail := func(err error) {
			if err != nil {
				errs = append(errs, err.Error())
			}
		}
		al := s.albumOf(r.URL.Query(), fail)
		if al != nil {
			f.Dirs, f.DirsByPath = al.dirs, al.total < byPathMax
		}
		pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pg = min(max(pg, 0), maxPage) // bounded, as on the Photos page: no overflow, no huge offsets
		f.Offset, f.Limit = pg*gp.perPage, gp.perPage
		st := s.cat.Store
		gs, info, err := st.GroupPage(gp.kind, f, gp.minSize, gp.summary)
		fail(err)
		plain := f
		plain.Dirs = nil // the album alone is not a filter
		d := &groupsData{Page: pg, Filtered: plain.Narrows(), Regrouping: st.GroupsRebuilding(), Partial: info.Partial, NoMemes: f.Source == "photos", Album: al}
		if !d.Regrouping {
			// never grouped, or the rules changed: say so, and group at once
			// (new photos only wait for the next regroup)
			d.Stale = st.GroupRulesChanged(st.GroupOptions())
			s.regroupInBackground(false)
		}
		if info.More && pg < maxPage {
			d.Next = pageLink(r, gp.path, pg+1)
		}
		if pg > 0 {
			d.Prev = pageLink(r, gp.path, pg-1)
		}
		for _, g := range gs {
			v := groupView{ID: g.ID, Size: g.Size, Matching: g.Matching, Taken: g.StartAt, Camera: g.Camera}
			for _, ph := range g.Members {
				v.Photos = append(v.Photos, catalog.PhotoHit{Photo: ph})
			}
			d.Groups = append(d.Groups, v)
		}
		p := &page{Title: gp.title, Nav: gp.path[1:], Filter: f, Data: d}
		s.filterForm(r, p, gp.path)
		if al != nil {
			// inside the album: its tabs, and a reset that stays in it
			d.Tabs = al.tabs(r.URL.Query(), gp.path[1:])
			p.Title = gp.title + " · " + al.Name + " · Albums"
			reset := url.Values{}
			for _, k := range []string{"dir", "iphone", "asort", "lf", "q"} {
				if v := r.URL.Query().Get(k); v != "" {
					reset.Set(k, v)
				}
			}
			p.Params["reset"] = gp.path + "?" + reset.Encode()
			p.Params["keepq"] = strings.TrimSpace(r.URL.Query().Get("q")) // the album's search, for the way back
		}
		if len(errs) > 0 {
			p.Error = errs[0]
		}
		s.render(w, r, gp.path[1:], p)
	}
}

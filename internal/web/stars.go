package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/auteursoft/archivis/internal/store"
)

// Stars: each account stars the photos it likes and lists them (Starred,
// or the "Starred only" filter); its stars are its own. Without accounts
// there is one shared list (user 0). Anyone signed in may star, viewers
// too (requiredRole): it changes nothing anyone else sees.

// starUser is whose stars a request reads and writes.
func starUser(r *http.Request) int64 {
	if u := userFrom(r); u != nil {
		return u.ID
	}
	return 0
}

// apiStar stars ({"on": true}) or unstars a photo for the requester.
func (s *Server) apiStar(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		jsonErr(w, errors.New("no such photo"))
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErr(w, err)
		return
	}
	if err := s.cat.Store.SetStar(starUser(r), id, req.On); err != nil {
		if errors.Is(err, store.ErrNoPhoto) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		jsonErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "starred": req.On})
}

// apiStars says which of the photos ?ids=1,2,3 (at most 500) the requester
// has starred, for the star buttons on a page of cards.
func (s *Server) apiStars(w http.ResponseWriter, r *http.Request) {
	var ids []int64
	for _, f := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
		if len(ids) == 500 {
			break
		}
	}
	got, err := s.cat.Store.Starred(starUser(r), ids)
	if err != nil {
		jsonErr(w, err)
		return
	}
	out := []int64{}
	for _, id := range ids {
		if got[id] {
			out = append(out, id)
		}
	}
	writeJSON(w, map[string]any{"starred": out})
}

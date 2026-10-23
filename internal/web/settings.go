package web

import (
	"log"
	"net/http"

	"github.com/auteursoft/archivis/internal/store"
)

// Settings: catalogue-wide options for admins. With accounts, /admin/ is
// admin-only (requiredRole). Without accounts there is no admin, and anyone
// the server admits (past --auth, if set) may change them, as they may
// edit everything else.

type settingsData struct {
	Settings       store.Settings
	MinGap, MaxGap int
	Saved          bool
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, v store.Settings, saved bool, errMsg string) {
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusBadRequest
	}
	s.renderStatus(w, r, "settings", &page{Title: "Settings", Nav: "settings", Error: errMsg,
		Data: &settingsData{Settings: v, MinGap: store.MinBurstGap, MaxGap: store.MaxBurstGap, Saved: saved}}, status)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, s.cat.Store.GetSettings(), r.URL.Query().Get("saved") == "1", "")
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	v := s.cat.Store.GetSettings()
	if err := r.ParseForm(); err != nil {
		s.renderSettings(w, r, v, false, "could not read the form: "+err.Error())
		return
	}
	for _, name := range store.SettingNames() {
		if _, ok := r.PostForm[name]; ok {
			if err := v.Set(name, r.PostFormValue(name)); err != nil {
				s.renderSettings(w, r, v, false, err.Error())
				return
			}
		}
	}
	if err := s.cat.Store.SetSettings(v); err != nil {
		s.renderSettings(w, r, v, false, err.Error())
		return
	}
	log.Printf("%s changed settings: burst gap %ds", rater(r), v.BurstGap) // account, --auth user or "local"
	http.Redirect(w, r, "/admin/settings?saved=1", http.StatusSeeOther)
}

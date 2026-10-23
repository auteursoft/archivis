package web

import (
	"context"
	"log"
	"time"

	"github.com/auteursoft/archivis/internal/indexer"
)

// regroupEvery is the least time between regroups while photos keep
// arriving (an index run regroups when it finishes anyway).
const regroupEvery = 30 * time.Minute

// regroup brings the bursts, stacks and sets of identical images up to date
// in the background, if they are stale; now skips the wait (the rules
// changed). One runs at a time.
func (s *Server) regroup(now bool) {
	if !now && time.Since(time.Unix(s.lastRegroup.Load(), 0)) < regroupEvery {
		return
	}
	st := s.cat.Store
	o := st.GroupOptions()
	if !st.GroupsStale(o) || !s.regrouping.CompareAndSwap(false, true) {
		return
	}
	defer s.regrouping.Store(false)
	s.lastRegroup.Store(time.Now().Unix())
	stats, ran, err := indexer.Regroup(context.Background(), s.cat.Layout.Dir, st, o, false)
	switch {
	case err != nil:
		log.Printf("grouping bursts and identical images: %v", err)
	case ran:
		log.Printf("grouped: %s", stats)
	}
}

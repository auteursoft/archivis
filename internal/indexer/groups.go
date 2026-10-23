package indexer

import (
	"context"
	"errors"

	"github.com/auteursoft/archivis/internal/store"
)

// Regroup rebuilds the catalogue's bursts, stacks and sets of identical
// images if they are out of date (or force is set). It returns ran=false
// if they were up to date or another process is regrouping.
func Regroup(ctx context.Context, dir string, st *store.Store, o store.GroupOptions, force bool) (stats store.RebuildStats, ran bool, err error) {
	if !force && !st.GroupsStale(o) {
		return stats, false, nil
	}
	unlock, err := LockGroups(dir)
	if errors.Is(err, ErrGroupsRunning) {
		return stats, false, nil
	}
	if err != nil {
		return stats, false, err
	}
	defer unlock()
	if !force && !st.GroupsStale(o) { // the other process just did it
		return stats, false, nil
	}
	stats, err = st.RebuildGroups(ctx, o)
	return stats, err == nil, err
}

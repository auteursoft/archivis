package indexer

import (
	"context"
	"errors"
	"time"

	"github.com/auteursoft/archivis/internal/store"
)

// Regroup rebuilds the catalogue's bursts, stacks and sets of identical
// images, and the folders Albums shows, if they are out of date (or force
// is set). It returns ran=false if they were up to date or another process
// is regrouping.
func Regroup(ctx context.Context, dir string, st *store.Store, o store.GroupOptions, force bool) (stats store.RebuildStats, ran bool, err error) {
	stale := func() (groups, folders bool) { return force || st.GroupsStale(o), force || st.FoldersStale() }
	if g, f := stale(); !g && !f {
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
	groups, folders := stale() // the other process may just have done it
	if !groups && !folders {
		return stats, false, nil
	}
	start := time.Now()
	if groups {
		if stats, err = st.RebuildGroups(ctx, o); err != nil {
			return stats, false, err
		}
	} else {
		stats.FoldersOnly = true
	}
	if folders {
		fst, err := st.RebuildFolders(ctx)
		stats.Folders, stats.FolderWrites = fst.Folders, fst.Writes
		if err != nil {
			return stats, false, err
		}
	}
	stats.Took = time.Since(start)
	return stats, true, nil
}

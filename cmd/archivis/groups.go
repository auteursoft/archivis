package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/store"
)

func runGroups(g *globals, args []string) error {
	fs := newFlagSet("groups", `Bursts, stacks of near-identical frames, and identical images:
  groups status     whether they are up to date
  groups rebuild    work them out again now (index runs and the web
                    interface do this by themselves when photos change)`)
	g.register(fs)
	pos := parseInterleaved(fs, args)
	if len(pos) == 0 {
		pos = []string{"status"}
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: groups status | rebuild")
	}
	st, err := store.Open(indexer.Layout{Dir: g.data}.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	o := st.GroupOptions()
	switch pos[0] {
	case "status":
		switch {
		case st.GroupsRebuilding():
			fmt.Println("being worked out now")
		case st.GroupsStale(o):
			fmt.Println("out of date (the next index run or the web interface will update them; or run `archivis groups rebuild`)")
		default:
			fmt.Println("up to date")
		}
	case "rebuild":
		ctx, cancel := signalContext()
		defer cancel()
		stats, ran, err := indexer.Regroup(ctx, g.data, st, o, true)
		if err != nil {
			return err
		}
		if !ran {
			return indexer.ErrGroupsRunning
		}
		fmt.Println(stats)
	default:
		return fmt.Errorf("unknown groups command %q (status | rebuild)", pos[0])
	}
	return nil
}

// regroup brings the groups up to date after an index run, logging what it
// did; a failure only means the groups wait for the next try.
func regroup(ctx context.Context, dir string, st *store.Store) {
	start := time.Now()
	stats, ran, err := indexer.Regroup(ctx, dir, st, st.GroupOptions(), false)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "warning: working out bursts and identical images failed after %s: %v (the web interface retries; or run `archivis groups rebuild`)\n",
			time.Since(start).Round(time.Second), err)
	case ran:
		fmt.Fprintf(os.Stderr, "grouped: %s\n", stats)
	}
}

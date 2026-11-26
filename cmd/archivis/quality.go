package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/store"
)

func runQuality(g *globals, args []string) error {
	fs := newFlagSet("quality", `MUSIQ's part of the technical score, for photos indexed before it was
installed (new photos get it as they are indexed):
  quality status              how many photos MUSIQ has scored, and how many not
  quality backfill [--for D]  score them now: each file is read again (a few
                              photos a second); stops after D (e.g. 2h) if
                              given, else when all are done; resumes where it
                              left off. index --backfill D does the same
                              after a nightly index run`)
	limit := fs.Duration("for", 0, "stop after this long (e.g. 2h); default: when every photo is scored")
	g.register(fs)
	pos := parseInterleaved(fs, args)
	if len(pos) == 0 {
		pos = []string{"status"}
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: quality status | backfill [--for DURATION]")
	}
	switch pos[0] {
	case "status":
		st, err := store.Open(indexer.Layout{Dir: g.data}.DB())
		if err != nil {
			return err
		}
		defer st.Close()
		scored, left, err := st.MUSIQCounts()
		if err != nil {
			return err
		}
		fmt.Printf("%d photos scored by MUSIQ, %d not yet", scored, left)
		if left > 0 {
			fmt.Print(" (archivis quality backfill)")
		}
		fmt.Println()
		return nil
	case "backfill":
		unlock, err := indexer.LockIndex(g.data) // not alongside an index run, which writes the same photos
		if err != nil {
			return err
		}
		defer unlock()
		eng, err := g.engines(need{musiq: true}, 1)
		if err != nil {
			return err
		}
		if eng.MUSIQ == nil {
			return fmt.Errorf("MUSIQ is not installed: run `archivis setup` first")
		}
		cat, err := catalog.Open(g.data, eng)
		if err != nil {
			return err
		}
		defer cat.Close()
		ctx, cancel := signalContext()
		defer cancel()
		var deadline time.Time
		if *limit > 0 {
			deadline = time.Now().Add(*limit)
		}
		if backfillMUSIQ(ctx, cat.Store, eng.MUSIQ, deadline, os.Stderr) && ctx.Err() == nil {
			regroupAndMark(ctx, g.data, cat.Store) // stacks, groups and album covers rank by the new scores
		}
		return nil
	}
	return fmt.Errorf("unknown quality command %q (status | backfill)", pos[0])
}

// backfillMUSIQ scores photos indexed before MUSIQ was installed until
// deadline (zero: all of them), logging what it did; it reports whether it
// changed any scores. Without MUSIQ (m nil) it does nothing.
func backfillMUSIQ(ctx context.Context, st *store.Store, m indexer.MUSIQScorer, deadline time.Time, w io.Writer) bool {
	if m == nil {
		return false
	}
	b, err := indexer.BackfillMUSIQ(ctx, st, m, deadline, w)
	if err != nil {
		fmt.Fprintf(w, "warning: scoring older photos with MUSIQ: %v (retried on the next run)\n", err)
		return b.Scored > 0
	}
	if b.Scored == 0 && b.Left == 0 {
		return false // nothing to do: say nothing
	}
	fmt.Fprintf(w, "quality: %s\n", b)
	return b.Scored > 0
}

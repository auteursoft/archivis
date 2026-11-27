package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/quality"
	"github.com/auteursoft/archivis/internal/store"
)

// BackfillStats reports a MUSIQ backfill.
type BackfillStats struct {
	Scored  int // photos scored and saved
	Changed int // photos that changed while being scored (the indexer scores those itself)
	Missing int // files not there (an offline drive, or deleted)
	Failed  int // files that could not be read or scored
	Stale   int // files changed on disk since they were indexed (the next index reads them again)
	Left    int // photos still unscored afterwards
	Took    time.Duration
	Stopped bool // the time ran out (or ctx was cancelled) before every photo was tried
}

func (b BackfillStats) String() string {
	s := fmt.Sprintf("scored %d photos with MUSIQ in %s", b.Scored, b.Took.Round(time.Second))
	if b.Scored > 0 {
		s += fmt.Sprintf(" (%.2f s each)", b.Took.Seconds()/float64(b.Scored))
	}
	if b.Missing > 0 {
		s += fmt.Sprintf("; %d files not found", b.Missing)
	}
	if b.Failed > 0 {
		s += fmt.Sprintf("; %d could not be read", b.Failed)
	}
	if b.Changed > 0 {
		s += fmt.Sprintf("; %d changed meanwhile (indexing scores them)", b.Changed)
	}
	if b.Stale > 0 {
		s += fmt.Sprintf("; %d files changed since they were indexed (the next index reads them again)", b.Stale)
	}
	return s + fmt.Sprintf("; %d left", b.Left)
}

// backfillBatch is how many photos are read, and saved, at a time.
var backfillBatch = 50

// backfillCursor is the meta key holding the last photo id a backfill
// tried, so a run stopped by its time limit is continued by the next one
// rather than started over (0: start from the beginning).
const backfillCursor = "musiq_backfill_after"

// errFileChanged means a photo's file is no longer the one indexed.
var errFileChanged = errors.New("file changed since it was indexed")

// BackfillMUSIQ scores the catalogued photos MUSIQ has not scored yet
// (photos indexed before it was installed), in id order, and with each score
// works out their technical and overall scores again: as indexing would,
// from the file decoded afresh and measured at quality.RatingSize, with the
// stored exposure and colour scores, blockiness and aesthetic score. It
// stops at deadline (zero: when every photo has been tried) or when ctx is
// cancelled, saving what it scored, and the next run carries on from there,
// wrapping round to the photos before it: so files that are missing,
// unreadable or slow to fail (an offline network drive) are tried again
// once a pass gets back to them, and cannot use up every run's time. A file
// changed on disk since it was indexed is left for the next index.
// workers photos are read and scored at once. Progress lines go to w (may
// be nil).
func BackfillMUSIQ(ctx context.Context, st *store.Store, m MUSIQScorer, deadline time.Time, workers int, w io.Writer) (BackfillStats, error) {
	start := time.Now()
	var b BackfillStats
	if m == nil {
		return b, errors.New("MUSIQ is not installed (run archivis setup)")
	}
	if w == nil {
		w = io.Discard
	}
	done := func() bool {
		return ctx.Err() != nil || (!deadline.IsZero() && time.Now().After(deadline))
	}
	workers = max(1, workers)
	lastReport := time.Now()
	from, _ := strconv.ParseInt(st.GetMeta(backfillCursor), 10, 64)
	after, wrapped := from, from <= 0
	for !done() {
		batch, err := st.UnscoredMUSIQ(after, max(backfillBatch, 4*workers))
		if err != nil {
			return b, err
		}
		if wrapped && from > 0 { // second leg: only the photos before where this run began
			for i, p := range batch {
				if p.ID > from {
					batch = batch[:i]
					break
				}
			}
		}
		if len(batch) == 0 {
			if wrapped {
				after = 0 // a whole pass: the next run starts from the beginning
				break
			}
			wrapped, after = true, 0
			continue
		}
		// score the batch workers at a time; photos not started when the
		// time is up stay for the next run
		type result struct {
			u   store.MUSIQUpdate
			err error
			ran bool
		}
		res := make([]result, len(batch))
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for i, p := range batch {
			sem <- struct{}{}
			if done() {
				<-sem
				break
			}
			after = p.ID
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				u, err := scoreFile(p, m)
				res[i] = result{u, err, true}
			}()
		}
		wg.Wait()
		var ups []store.MUSIQUpdate
		for i, r := range res {
			switch {
			case !r.ran:
			case errors.Is(r.err, fs.ErrNotExist):
				b.Missing++
			case errors.Is(r.err, errFileChanged):
				b.Stale++
			case r.err != nil:
				b.Failed++
				fmt.Fprintf(w, "warning: MUSIQ: %s: %v\n", batch[i].Path, r.err)
			default:
				ups = append(ups, r.u)
			}
		}
		n, err := st.SetMUSIQ(ups, Overall)
		if err != nil {
			return b, err
		}
		b.Scored += n
		b.Changed += len(ups) - n
		if time.Since(lastReport) > time.Minute {
			lastReport = time.Now()
			fmt.Fprintf(w, "MUSIQ: %d photos scored so far\n", b.Scored)
		}
	}
	b.Stopped = done()
	if err := st.SetMeta(backfillCursor, strconv.FormatInt(after, 10)); err != nil {
		return b, err
	}
	_, left, err := st.MUSIQCounts()
	b.Left, b.Took = left, time.Since(start)
	return b, err
}

// scoreFile decodes a photo's file and scores it, as AnalyzeImage would,
// if it is still the file indexed (its size and modification time, as the
// indexer judges, before and after reading it): another file's pixels must
// not be rated with this photo's stored scores.
func scoreFile(p store.MUSIQInput, m MUSIQScorer) (store.MUSIQUpdate, error) {
	same := func() error {
		fi, err := os.Stat(p.Path)
		switch {
		case err != nil:
			return err
		case fi.Size() != p.Size || fi.ModTime().Unix() != p.MTime:
			return errFileChanged
		}
		return nil
	}
	if err := same(); err != nil {
		return store.MUSIQUpdate{}, err
	}
	d, err := imageio.Load(p.Path, WorkSize)
	if err != nil {
		return store.MUSIQUpdate{}, err
	}
	if err := same(); err != nil {
		return store.MUSIQUpdate{}, err
	}
	rating, s, err := Rating(d.Img, m)
	if err != nil {
		return store.MUSIQUpdate{}, err
	}
	tech := round1(technical(rating, s.Valid, quality.Metrics{ExposureScore: p.ExposureScore, ColorScore: p.ColorScore}, p.Blockiness))
	// Overall is worked out again from the aesthetic score as it is when
	// saved (SetMUSIQ): this one is a fallback
	return store.MUSIQUpdate{ID: p.ID, Seq: p.Seq, MUSIQ: s.Float64, Technical: tech, Overall: Overall(tech, p.Aesthetic)}, nil
}

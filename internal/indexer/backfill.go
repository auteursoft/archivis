package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	return s + fmt.Sprintf("; %d left", b.Left)
}

// backfillBatch is how many photos are read, and saved, at a time.
var backfillBatch = 50

// BackfillMUSIQ scores the catalogued photos MUSIQ has not scored yet
// (photos indexed before it was installed), in id order, and with each score
// works out their technical and overall scores again: as indexing would,
// from the file decoded afresh and measured at quality.RatingSize, with the
// stored exposure and colour scores, blockiness and aesthetic score. It
// stops at deadline (zero: when every photo has been tried) or when ctx is
// cancelled, saving what it scored; a later run picks up the rest. Files
// that are missing or unreadable are skipped and tried again next time.
// Progress lines go to w (may be nil).
func BackfillMUSIQ(ctx context.Context, st *store.Store, m MUSIQScorer, deadline time.Time, w io.Writer) (BackfillStats, error) {
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
	lastReport := time.Now()
	after := int64(0)
	for !done() {
		batch, err := st.UnscoredMUSIQ(after, backfillBatch)
		if err != nil {
			return b, err
		}
		if len(batch) == 0 {
			break
		}
		var ups []store.MUSIQUpdate
		for _, p := range batch {
			if done() {
				break
			}
			after = p.ID
			u, err := scoreFile(p, m)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				b.Missing++
				continue
			case err != nil:
				b.Failed++
				fmt.Fprintf(w, "warning: MUSIQ: %s: %v\n", p.Path, err)
				continue
			}
			ups = append(ups, u)
		}
		n, err := st.SetMUSIQ(ups)
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
	_, left, err := st.MUSIQCounts()
	b.Left, b.Took = left, time.Since(start)
	return b, err
}

// scoreFile decodes a photo's file and scores it, as AnalyzeImage would.
func scoreFile(p store.MUSIQInput, m MUSIQScorer) (store.MUSIQUpdate, error) {
	if _, err := os.Stat(p.Path); err != nil {
		return store.MUSIQUpdate{}, err
	}
	d, err := imageio.Load(p.Path, WorkSize)
	if err != nil {
		return store.MUSIQUpdate{}, err
	}
	rating, s, err := Rating(d.Img, m)
	if err != nil {
		return store.MUSIQUpdate{}, err
	}
	tech := round1(quality.Technical(rating, quality.Metrics{ExposureScore: p.ExposureScore, ColorScore: p.ColorScore}, p.Blockiness))
	return store.MUSIQUpdate{ID: p.ID, Seq: p.Seq, MUSIQ: s.Float64, Technical: tech, Overall: Overall(tech, p.Aesthetic)}, nil
}

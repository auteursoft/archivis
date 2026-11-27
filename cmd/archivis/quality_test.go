package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/store"
)

type fixedMUSIQ float64

func (f fixedMUSIQ) Score(*imageio.RGB) (float64, error) { return float64(f), nil }

// Without MUSIQ, index --backfill does nothing and says nothing; with it,
// it reports what it scored and what is left.
func TestBackfillMUSIQReport(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var log strings.Builder
	if backfillMUSIQ(context.Background(), st, nil, time.Time{}, 1, &log) || log.Len() != 0 {
		t.Fatalf("without MUSIQ: %q", log.String())
	}
	if backfillMUSIQ(context.Background(), st, fixedMUSIQ(50), time.Time{}, 1, &log) || log.Len() != 0 {
		t.Fatalf("nothing to score: %q", log.String())
	}
	if err := st.Save([]*store.Result{{Photo: store.Photo{Path: "/gone.jpg", Size: 1, MTime: 1, Fingerprint: "g", Version: store.SchemaVersion}}}); err != nil {
		t.Fatal(err)
	}
	if backfillMUSIQ(context.Background(), st, fixedMUSIQ(50), time.Time{}, 2, &log) {
		t.Fatal("reported a change with nothing scored")
	}
	if got := log.String(); !strings.Contains(got, "quality: scored 0 photos") || !strings.Contains(got, "1 files not found; 1 left") {
		t.Fatalf("report: %q", got)
	}
}

// A backfill with no workers would score nothing: it is refused.
func TestQualityBackfillNeedsWorkers(t *testing.T) {
	g := &globals{data: t.TempDir()}
	for _, n := range []string{"0", "-2"} {
		if err := runQuality(g, []string{"backfill", "--workers", n}); err == nil || !strings.Contains(err.Error(), "at least 1") {
			t.Errorf("--workers %s: %v", n, err)
		}
	}
}

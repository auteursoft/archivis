// Package indexer walks photo folders and catalogues every image: metadata,
// quality metrics, faces, CLIP embeddings, tags and thumbnails. It is
// incremental (unchanged files are skipped), resumable (interrupt at any time)
// and recognises moved files and byte-identical copies by content fingerprint.
package indexer

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/store"
)

// Config controls an indexing run.
type Config struct {
	Roots       []string
	Exclude     []string // substrings or glob patterns matched against paths/dir names
	Workers     int
	Force       bool // re-analyse even unchanged files
	RetryErrors bool
	MinBytes    int64 // skip files smaller than this (icons, tiny thumbnails)
	Progress    io.Writer
	// OnSaved is called (from the writer goroutine) after each batch commits.
	OnSaved func([]*store.Result)
}

// Stats are live counters for a run.
type Stats struct {
	Seen, Queued, Indexed, Copied, Moved, Unchanged, Errors, Faces atomic.Int64
	WalkDone                                                       atomic.Bool
}

// Indexer runs the pipeline.
type Indexer struct {
	cfg    Config
	store  *store.Store
	eng    *Engines
	layout Layout
	Stats  Stats
	legacy bool // the catalogue has photos with pre-full-hash fingerprints

	startedAt time.Time
}

func New(cfg Config, st *store.Store, eng *Engines, layout Layout) *Indexer {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}
	if cfg.MinBytes <= 0 {
		cfg.MinBytes = 4 << 10
	}
	ix := &Indexer{cfg: cfg, store: st, eng: eng, layout: layout}
	// 24-digit fingerprints come from before full-content hashing.
	var one int
	ix.legacy = st.DB.QueryRow(`SELECT 1 FROM photos WHERE length(fingerprint) = 24 LIMIT 1`).Scan(&one) == nil
	return ix
}

type job struct {
	path string
	info os.FileInfo
}

type outcome struct {
	res   *store.Result
	move  *moveOp
	path  string
	info  os.FileInfo
	err   error
	faces int
}

type moveOp struct {
	id    int64
	path  string
	mtime int64
}

var skipDirNames = map[string]bool{
	"@eaDir": true, "#recycle": true, "#snapshot": true, "$RECYCLE.BIN": true,
	"System Volume Information": true, "node_modules": true, "lost+found": true,
}

func (ix *Indexer) excluded(path string, isDir bool) bool {
	base := filepath.Base(path)
	if isDir {
		if strings.HasPrefix(base, ".") && len(base) > 1 {
			return true
		}
		if skipDirNames[base] || strings.HasSuffix(base, ".lrdata") || strings.HasSuffix(base, ".noindex") {
			return true
		}
	} else if strings.HasPrefix(base, "._") { // macOS AppleDouble files
		return true
	}
	for _, e := range ix.cfg.Exclude {
		if ok, _ := filepath.Match(e, base); ok {
			return true
		}
		if !strings.ContainsAny(e, "*?[") && strings.Contains(path, e) {
			return true
		}
	}
	return false
}

// Run indexes all roots until done or ctx is cancelled.
func (ix *Indexer) Run(ctx context.Context) error {
	ix.startedAt = time.Now()
	jobs := make(chan job, ix.cfg.Workers*4)
	outs := make(chan outcome, ix.cfg.Workers*4)

	// walker
	var walkErr error
	go func() {
		defer close(jobs)
		defer ix.Stats.WalkDone.Store(true)
		for _, root := range ix.cfg.Roots {
			abs, err := filepath.Abs(root)
			if err != nil {
				walkErr = err
				return
			}
			err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil {
					ix.logf("warning: %v\n", err)
					if d != nil && d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					if p != abs && ix.excluded(p, true) {
						return fs.SkipDir
					}
					return nil
				}
				if !imageio.IsImagePath(p) || ix.excluded(p, false) {
					return nil
				}
				var info os.FileInfo
				switch {
				case d.Type().IsRegular():
					info, err = d.Info()
				case d.Type()&fs.ModeSymlink != 0:
					// follow links to files (links to folders are not followed,
					// which avoids loops)
					info, err = os.Stat(p)
					if err == nil && !info.Mode().IsRegular() {
						return nil
					}
				default:
					return nil
				}
				if err != nil || info.Size() < ix.cfg.MinBytes {
					return nil
				}
				ix.Stats.Seen.Add(1)
				if !ix.cfg.Force {
					k, err := ix.store.Lookup(p)
					if err == nil && k != nil && k.Size == info.Size() && k.MTime == info.ModTime().Unix() && k.Version >= store.SchemaVersion {
						ix.Stats.Unchanged.Add(1)
						return nil
					}
					if !ix.cfg.RetryErrors {
						if bad, _ := ix.store.ErrorFor(p, info.Size(), info.ModTime().Unix()); bad {
							ix.Stats.Unchanged.Add(1)
							return nil
						}
					}
				}
				ix.Stats.Queued.Add(1)
				select {
				case jobs <- job{p, info}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			})
			if err != nil && ctx.Err() == nil {
				walkErr = err
				return
			}
		}
	}()

	// workers
	var wg sync.WaitGroup
	for i := 0; i < ix.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue // drain
				}
				outs <- ix.process(j)
			}
		}()
	}
	go func() { wg.Wait(); close(outs) }()

	// progress
	stopProgress := make(chan struct{})
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ix.progressLoop(stopProgress)
	}()

	// writer (this goroutine)
	var batch []*store.Result
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := ix.store.Save(batch); err != nil {
			// One bad row must not sink the batch: save photos one by one.
			// A photo that still fails is counted and logged but not
			// recorded as a bad file -- save errors (a busy or full disk)
			// are not the file's fault, so the next run retries it.
			ix.logf("\nerror saving a batch of %d (%v); saving one by one\n", len(batch), err)
			var saved []*store.Result
			for _, r := range batch {
				if err := ix.store.Save([]*store.Result{r}); err != nil {
					ix.Stats.Errors.Add(1)
					ix.logf("error saving %s: %v (will retry next run)\n", r.Photo.Path, err)
					continue
				}
				saved = append(saved, r)
			}
			if len(saved) > 0 && ix.cfg.OnSaved != nil {
				ix.cfg.OnSaved(saved)
			}
		} else if ix.cfg.OnSaved != nil {
			ix.cfg.OnSaved(batch)
		}
		batch = batch[:0]
	}
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
loop:
	for {
		select {
		case o, ok := <-outs:
			if !ok {
				break loop
			}
			switch {
			case o.err != nil:
				ix.Stats.Errors.Add(1)
				msg := o.err.Error()
				if err := ix.store.RecordError(o.path, o.info.Size(), o.info.ModTime().Unix(), msg); err != nil {
					ix.logf("\nerror recording failure of %s: %v\n", o.path, err)
				}
			case o.move != nil:
				ix.Stats.Moved.Add(1)
				if err := ix.store.MovePath(o.move.id, o.move.path, o.move.mtime); err != nil {
					ix.logf("\nerror recording move of %s: %v\n", o.path, err)
				}
			default:
				ix.Stats.Faces.Add(int64(o.faces))
				batch = append(batch, o.res)
				if len(batch) >= 64 {
					flush()
				}
			}
		case <-tick.C:
			flush()
		}
	}
	flush()
	close(stopProgress)
	<-progressDone
	ix.printProgress(true)
	if walkErr != nil {
		return walkErr
	}
	return ctx.Err()
}

func (ix *Indexer) process(j job) (o outcome) {
	o.path, o.info = j.path, j.info
	defer func() {
		if r := recover(); r != nil {
			o.err = fmt.Errorf("panic: %v", r)
		}
	}()
	fp, err := Fingerprint(j.path, j.info.Size())
	if err != nil {
		o.err = err
		return
	}
	if !ix.cfg.Force {
		if ix.reuse(j, fp, false, &o) {
			return
		}
		// Photos catalogued before full-content fingerprints carry the old
		// partial one; look them up by it so moves keep their labels.
		if ix.legacy {
			if lfp, err := LegacyFingerprint(j.path, j.info.Size()); err == nil && ix.reuse(j, lfp, true, &o) {
				return
			}
		}
	}
	a, err := AnalyzeFile(j.path, j.info, fp, ix.eng, ix.layout)
	if err != nil {
		o.err = err
		return
	}
	ix.Stats.Indexed.Add(1)
	o.res, o.faces = a.Result, len(a.Faces)
	return
}

// reuse looks for an existing record of this content (moved file or copy)
// and fills o if one is found. A legacy fingerprint only proves equal size
// and edges, so a legacy copy is accepted after a byte-for-byte comparison;
// a legacy move (old path gone) cannot be compared and is accepted as before.
func (ix *Indexer) reuse(j job, fp string, legacy bool, o *outcome) bool {
	prev, err := ix.store.ByFingerprint(fp)
	if err != nil {
		return false
	}
	for _, p := range prev {
		if p.Size != j.info.Size() || p.Version < store.SchemaVersion || p.Path == j.path {
			continue
		}
		if _, err := os.Stat(p.Path); os.IsNotExist(err) {
			// The file was moved or renamed: keep its record.
			o.move = &moveOp{p.ID, j.path, j.info.ModTime().Unix()}
			return true
		}
		if legacy {
			if same, err := sameContent(p.Path, j.path); err != nil || !same {
				continue
			}
		}
		// A byte-identical copy: reuse the analysis.
		r, err := ix.copyResult(p, j)
		if err == nil {
			ix.Stats.Copied.Add(1)
			o.res, o.faces = r, len(r.Faces)
			return true
		}
	}
	return false
}

// sameContent compares two files byte for byte.
func sameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	ra, rb := bufio.NewReaderSize(fa, 1<<20), bufio.NewReaderSize(fb, 1<<20)
	ba, bb := make([]byte, 1<<16), make([]byte, 1<<16)
	for {
		na, ea := io.ReadFull(ra, ba)
		nb, eb := io.ReadFull(rb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		if ea != nil || eb != nil {
			done := func(e error) bool { return e == io.EOF || e == io.ErrUnexpectedEOF }
			if done(ea) && done(eb) {
				return true, nil
			}
			return false, cmp.Or(ea, eb)
		}
	}
}

func (ix *Indexer) copyResult(p store.Photo, j job) (*store.Result, error) {
	faces, err := ix.store.FacesForPhoto(p.ID)
	if err != nil {
		return nil, err
	}
	tags, err := ix.store.TagsForPhoto(p.ID)
	if err != nil {
		return nil, err
	}
	clip, err := ix.store.CLIP(p.ID)
	if err != nil {
		return nil, err
	}
	scores, err := ix.store.AestheticScores(p.ID)
	if err != nil {
		return nil, err
	}
	np := p
	np.CLIP = clip
	np.ID = 0
	np.Path = j.path
	np.MTime = j.info.ModTime().Unix()
	np.IndexedAt = time.Now().Unix()
	for i := range faces {
		faces[i].ID, faces[i].PhotoID = 0, 0
		if faces[i].PersonSource.String != "manual" {
			faces[i].PersonID = sql.NullInt64{}
			faces[i].PersonSource = sql.NullString{}
			faces[i].PersonSim = sql.NullFloat64{}
		}
	}
	return &store.Result{Photo: np, Faces: faces, Tags: tags, AestheticScores: scores}, nil
}

func (ix *Indexer) logf(format string, args ...any) {
	if ix.cfg.Progress != nil {
		fmt.Fprintf(ix.cfg.Progress, format, args...)
	}
}

// interactive reports whether progress goes to a terminal. Otherwise (a
// systemd journal, a launchd log file) each update is a whole line, once a
// minute, instead of a line redrawn in place every two seconds.
func (ix *Indexer) interactive() bool {
	f, ok := ix.cfg.Progress.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (ix *Indexer) progressLoop(stop chan struct{}) {
	every := 2 * time.Second
	if !ix.interactive() {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ix.printProgress(false)
		}
	}
}

func (ix *Indexer) printProgress(final bool) {
	if ix.cfg.Progress == nil {
		return
	}
	s := &ix.Stats
	done := s.Indexed.Load() + s.Copied.Load() + s.Moved.Load() + s.Errors.Load()
	el := time.Since(ix.startedAt).Seconds()
	rate := 0.0
	if el > 0 {
		rate = float64(s.Indexed.Load()) / el
	}
	eta := ""
	if s.WalkDone.Load() && rate > 0 {
		left := s.Queued.Load() - done
		eta = fmt.Sprintf(" | ETA %s", (time.Duration(float64(left)/rate) * time.Second).Round(time.Second))
	}
	end := "\r"
	if final || !ix.interactive() {
		end = "\n"
	}
	walk := "scanning"
	if s.WalkDone.Load() {
		walk = "scanned"
	}
	fmt.Fprintf(ix.cfg.Progress, "%s %d | analysed %d (%.1f/s) | copies %d | moved %d | unchanged %d | errors %d | faces %d%s   %s",
		walk, s.Seen.Load(), s.Indexed.Load(), rate, s.Copied.Load(), s.Moved.Load(), s.Unchanged.Load(), s.Errors.Load(), s.Faces.Load(), eta, end)
}

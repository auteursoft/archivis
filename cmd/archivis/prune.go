package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/auteursoft/archivis/internal/store"
)

// folderProblem says why a folder to index can't be trusted to show which
// of its files were deleted, or "" if it can. A drive that isn't mounted
// leaves its folder gone, or an empty mount point; one the service may not
// read (no Full Disk Access) can't be listed.
func folderProblem(dir string) string {
	st, err := os.Stat(dir)
	switch {
	case err != nil && os.IsNotExist(err):
		return "is not there (drive not mounted?)"
	case err != nil:
		return fmt.Sprintf("can't be read: %v", err)
	case !st.IsDir():
		return "is not a folder"
	}
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Sprintf("can't be read: %v (Full Disk Access?)", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if len(names) == 0 {
		if err != nil && err != io.EOF {
			return fmt.Sprintf("can't be read: %v (Full Disk Access?)", err)
		}
		return "is empty (drive not mounted?)"
	}
	return ""
}

// pruneAfterIndex (index --prune) removes the catalogue entries of files
// deleted from the folders just indexed. It runs after a complete index and
// its clean-ups, so moved and renamed files already have their new paths
// and copies' names and ratings are on their originals, and only when the
// run saw everything: every folder the job names was ready when it started
// (atStart holds those that weren't, with why) and still is, and the run
// read and recorded every file (gaps is 0). Otherwise it logs an error with
// each reason and prunes nothing, keeping those photos until a run that
// sees everything (after a restart, several drives can be late). Prune
// itself also keeps any folder whose files are all gone and that is itself
// gone or empty.
func pruneAfterIndex(st *store.Store, folders []string, atStart map[string]string, gaps int64, w io.Writer) (store.PruneResult, bool) {
	var reasons []string
	for _, f := range folders {
		if p, ok := atStart[f]; ok {
			reasons = append(reasons, fmt.Sprintf("%s: when indexing started, it %s", f, p))
		} else if p := folderProblem(f); p != "" {
			reasons = append(reasons, fmt.Sprintf("%s %s", f, p))
		}
	}
	if gaps > 0 {
		reasons = append(reasons, fmt.Sprintf("this run could not read or record %d folders or files (see the warnings and errors above)", gaps))
	}
	if len(reasons) > 0 {
		fmt.Fprintf(w, "error: not removing deleted photos, because this run may not have seen them all:\n  %s\n  Their photos stay in the catalogue; deleted ones are removed after a run that finds every folder and reads every file.\n",
			strings.Join(reasons, "\n  "))
		return store.PruneResult{}, false
	}
	res, err := st.Prune(folders, false, false)
	if err != nil {
		fmt.Fprintf(w, "error: removing deleted photos: %v (retried on the next run)\n", err)
		return res, false
	}
	fmt.Fprintf(w, "pruned: removed %d deleted photos from the catalogue", len(res.Gone))
	if len(res.Held) > 0 {
		fmt.Fprintf(w, "; kept %d folders whose files are all missing and that are gone or empty, as an offline drive looks (archivis prune --all-missing removes them): %s",
			len(res.Held), strings.Join(res.Held, ", "))
	}
	fmt.Fprintln(w)
	return res, true
}

// pruneJob is what index --prune needs once the run is over.
type pruneJob struct {
	folders  []string          // the folders the job names
	notReady map[string]string // those not ready as indexing started, and why
	gaps     int64             // what the run could not read or record
}

// cleanUpAfterIndex removes the copies photo apps keep (moving their face
// names and ratings to the photos they copy), then, for index --prune
// (pj not nil), the photos deleted from the folders indexed. In that
// order: a copy whose file is gone, among others still there, would lose
// its names and ratings if pruned first. Before auto-labelling, so moved
// names count as examples and deleted photos' faces don't.
func cleanUpAfterIndex(ctx context.Context, st *store.Store, pj *pruneJob, w io.Writer) {
	// Apple's copies catalogued from inside Photos libraries by older
	// versions: removed once their originals are in (names moved first)
	res, err := st.CleanPhotosLibraries(ctx)
	if err != nil {
		fmt.Fprintf(w, "warning: cleaning up Photos library copies: %v (retried on the next run)\n", err)
	}
	if res.Removed > 0 || res.Kept > 0 {
		fmt.Fprintf(w, "Photos libraries: removed %d of Apple's copies (moved %d face names, %d ratings to the originals); kept %d with no original catalogued\n",
			res.Removed, res.LabelsMoved, res.FeedbackMoved, res.Kept)
	}
	// then any app copies left: Photos copies with a twin elsewhere,
	// iPhoto, Aperture and Capture One caches
	cres, err := st.CleanAppCaches(ctx)
	if err != nil {
		fmt.Fprintf(w, "warning: cleaning up photo apps' copies: %v (retried on the next run)\n", err)
	}
	if cres.Removed > 0 || cres.Kept > 0 {
		fmt.Fprintf(w, "app copies: removed %d thumbnails and previews of photo apps (moved %d face names, %d ratings to the photos they copy); kept %d (no other copy, or hand-named faces or ratings on them)\n",
			cres.Removed, cres.LabelsMoved, cres.FeedbackMoved, cres.Kept)
	}
	if pj != nil && ctx.Err() == nil {
		pruneAfterIndex(st, pj.folders, pj.notReady, pj.gaps, w)
	}
}

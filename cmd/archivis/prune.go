package main

import (
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
// deleted from the folders just indexed. It runs after a complete index, so
// moved and renamed files already have their new paths, and only when every
// folder the job names was there for the whole run: checked before it
// (missing lists those it had to skip) and again now. Otherwise it logs an
// error naming each missing folder and prunes nothing, keeping their photos
// until a run finds every folder (after a restart, several drives can be
// late). Prune itself also keeps any folder whose files are all gone and
// that is itself gone or empty.
func pruneAfterIndex(st *store.Store, folders, missing []string, w io.Writer) (store.PruneResult, bool) {
	problems := map[string]string{}
	for _, f := range missing {
		problems[f] = "was not there when indexing started (drive not mounted?)"
	}
	for _, f := range folders {
		if _, ok := problems[f]; !ok {
			if p := folderProblem(f); p != "" {
				problems[f] = p
			}
		}
	}
	if len(problems) > 0 {
		var b strings.Builder
		for _, f := range folders {
			if p, ok := problems[f]; ok {
				fmt.Fprintf(&b, "\n  %s %s", f, p)
			}
		}
		fmt.Fprintf(w, "error: not removing deleted photos: %d of the %d folders to index can't be checked:%s\n  Their photos stay in the catalogue; deleted ones are removed on the next run that finds every folder.\n",
			len(problems), len(folders), b.String())
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

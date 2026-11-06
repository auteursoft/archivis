package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/store"
)

// index --prune removes deleted photos only when every folder to index is
// there and not empty, before the run and after it; one missing drive, or
// several after a restart, keeps every folder's photos and logs which.
func TestPruneAfterIndex(t *testing.T) {
	setup := func(t *testing.T) (st *store.Store, a, b string) {
		root := t.TempDir()
		a, b = filepath.Join(root, "Archive1"), filepath.Join(root, "Archive2")
		for _, d := range []string{a, b} {
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, "keep.jpg"), []byte("x"), 0o644)
		}
		st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		var rs []*store.Result
		for i, p := range []string{
			filepath.Join(a, "keep.jpg"), filepath.Join(a, "deleted.jpg"),
			filepath.Join(b, "keep.jpg"), filepath.Join(b, "deleted.jpg"),
		} {
			rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Version: store.SchemaVersion}})
		}
		if err := st.Save(rs); err != nil {
			t.Fatal(err)
		}
		return st, a, b
	}
	count := func(t *testing.T, st *store.Store) int64 {
		s, err := st.Stats()
		if err != nil {
			t.Fatal(err)
		}
		return s.Photos
	}

	// a folder named by a link is walked like any other (with or without a
	// trailing separator), so its deleted photos are pruned too
	t.Run("a folder that is a link", func(t *testing.T) {
		st, a, b := setup(t)
		symlinkFolder(t, b)
		var log bytes.Buffer
		res, ok := pruneAfterIndex(st, []string{a, b + string(filepath.Separator)}, nil, 0, &log)
		if !ok || len(res.Gone) != 2 || count(t, st) != 2 {
			t.Fatalf("ok=%v gone=%v photos=%d, want both deleted.jpg removed; log: %s", ok, res.Gone, count(t, st), &log)
		}
	})

	t.Run("every folder there", func(t *testing.T) {
		st, a, b := setup(t)
		var log bytes.Buffer
		res, ok := pruneAfterIndex(st, []string{a + "/", b}, nil, 0, &log)
		if !ok || len(res.Gone) != 2 || count(t, st) != 2 {
			t.Fatalf("ok=%v gone=%v photos=%d, want both deleted.jpg removed; log: %s", ok, res.Gone, count(t, st), &log)
		}
		if !strings.Contains(log.String(), "pruned: removed 2 deleted photos") {
			t.Errorf("log: %q", &log)
		}
	})

	for _, c := range []struct {
		name   string
		break_ func(a, b string) (notReady map[string]string, gaps int64) // what happens to the drives and the run
		want   []string                                                   // in the error
	}{
		{"one drive unmounted (folder gone)", func(a, b string) (map[string]string, int64) { os.RemoveAll(b); return nil, 0 },
			[]string{"Archive2 is not there"}},
		{"one drive unmounted (empty mount point)", func(a, b string) (map[string]string, int64) {
			os.Remove(filepath.Join(b, "keep.jpg"))
			return nil, 0
		}, []string{"Archive2 is empty"}},
		{"both missing after a restart", func(a, b string) (map[string]string, int64) { os.RemoveAll(a); os.RemoveAll(b); return nil, 0 },
			[]string{"Archive1 is not there", "Archive2 is not there"}},
		// not walked, or walked while empty: photos moved onto it weren't
		// seen, though it is ready by the end
		{"not there at the start, mounted by the end", func(a, b string) (map[string]string, int64) {
			return map[string]string{b: folderProblemOf(t, filepath.Join(t.TempDir(), "gone"))}, 0
		}, []string{"Archive2: when indexing started, it is not there"}},
		{"an empty mount point at the start, mounted by the end", func(a, b string) (map[string]string, int64) {
			return map[string]string{b: folderProblemOf(t, t.TempDir())}, 0
		}, []string{"Archive2: when indexing started, it is empty"}},
		{"a file, not a folder", func(a, b string) (map[string]string, int64) {
			os.RemoveAll(b)
			os.WriteFile(b, []byte("x"), 0o644)
			return nil, 0
		}, []string{"Archive2 is not a folder"}},
		// a link to a folder on a drive that isn't mounted
		{"a link to a folder that isn't there", func(a, b string) (map[string]string, int64) {
			target := symlinkFolder(t, b)
			os.RemoveAll(target)
			return nil, 0
		}, []string{"Archive2 is not there"}},
		// a subfolder it couldn't read, a move it couldn't save: a photo
		// moved there still has its old path
		{"every folder there, but the run had gaps", func(a, b string) (map[string]string, int64) { return nil, 2 },
			[]string{"could not read or record 2 folders or files"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, a, b := setup(t)
			notReady, gaps := c.break_(a, b)
			var log bytes.Buffer
			res, ok := pruneAfterIndex(st, []string{a, b}, notReady, gaps, &log)
			if ok || len(res.Gone) != 0 || count(t, st) != 4 {
				t.Fatalf("ok=%v gone=%v photos=%d, want nothing removed from either folder", ok, res.Gone, count(t, st))
			}
			out := log.String()
			for _, w := range append([]string{"error: not removing deleted photos", "after a run that finds every folder and reads every file"}, c.want...) {
				if !strings.Contains(out, w) {
					t.Errorf("log lacks %q:\n%s", w, out)
				}
			}
		})
	}

	t.Run("a folder the service may not read", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("permissions are not enforced here")
		}
		st, a, b := setup(t)
		os.Chmod(b, 0)
		t.Cleanup(func() { os.Chmod(b, 0o755) })
		var log bytes.Buffer
		if _, ok := pruneAfterIndex(st, []string{a, b}, nil, 0, &log); ok || count(t, st) != 4 {
			t.Fatalf("pruned with an unreadable folder; log: %s", &log)
		}
		if !strings.Contains(log.String(), "Archive2 can't be read") {
			t.Errorf("log: %q", &log)
		}
	})
}

func folderProblemOf(t *testing.T, dir string) string {
	t.Helper()
	p := folderProblem(dir)
	if p == "" {
		t.Fatalf("%s: no problem found", dir)
	}
	return p
}

// The clean-ups run before pruning: a Photos copy whose file is gone, while
// its folder still has others, hands its rating to its original instead of
// losing it to the prune.
func TestCleanUpBeforePrune(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "Photos Library.photoslibrary")
	const u = "AAAA1111-0000-4000-8000-000000000001"
	orig := filepath.Join(lib, "originals", "A", u+".heic")
	copyPath := filepath.Join(lib, "resources", "derivatives", "A", u+"_1_105_c.jpeg") // its file deleted
	other := filepath.Join(lib, "resources", "derivatives", "A", "other.jpeg")         // still there
	for _, p := range []string{orig, other} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*store.Result
	for i, p := range []string{orig, copyPath} {
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Version: store.SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`INSERT INTO aesthetic_feedback (photo_id, rater, rating, created_at) VALUES (?, 'sean', 8, 1)`, rs[1].Photo.ID)
	var log bytes.Buffer
	cleanUpAfterIndex(context.Background(), st, &pruneJob{folders: []string{lib}}, &log)
	var on int64
	if err := st.DB.QueryRow(`SELECT photo_id FROM aesthetic_feedback WHERE rater = 'sean'`).Scan(&on); err != nil || on != rs[0].Photo.ID {
		t.Fatalf("the copy's rating is on photo %d (err %v), want the original %d; log:\n%s", on, err, rs[0].Photo.ID, &log)
	}
	if !strings.Contains(log.String(), "pruned: removed 0 deleted photos") {
		t.Errorf("log: %s", &log)
	}
}

// symlinkFolder replaces dir with a link to a folder holding the same
// file, and returns that folder.
func symlinkFolder(t *testing.T, dir string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "real")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "keep.jpg"), []byte("x"), 0o644)
	os.RemoveAll(dir)
	if err := os.Symlink(target, dir); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	return target
}

// A clean-up that fails leaves copies whose names and ratings weren't
// moved; pruning waits rather than remove them.
func TestCleanUpFailureHoldsPrune(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "Photos Library.photoslibrary")
	const u = "AAAA1111-0000-4000-8000-000000000001"
	orig := filepath.Join(lib, "originals", "A", u+".heic")
	copyPath := filepath.Join(lib, "resources", "derivatives", "A", u+"_1_105_c.jpeg") // its file deleted
	other := filepath.Join(lib, "resources", "derivatives", "A", "other.jpeg")
	for _, p := range []string{orig, other} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*store.Result
	for i, p := range []string{orig, copyPath} {
		rs = append(rs, &store.Result{Photo: store.Photo{Path: p, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Version: store.SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`INSERT INTO aesthetic_feedback (photo_id, rater, rating, created_at) VALUES (?, 'sean', 8, 1)`, rs[1].Photo.ID)
	// the Photos clean-up can't remove the copy (a busy database, say)
	if _, err := st.DB.Exec(`CREATE TRIGGER busy BEFORE DELETE ON photos WHEN OLD.path LIKE '%derivatives%'
		BEGIN SELECT RAISE(ABORT, 'database is locked (simulated)'); END`); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	cleanUpAfterIndex(context.Background(), st, &pruneJob{folders: []string{lib}}, &log)
	out := log.String()
	for _, w := range []string{"error: not removing deleted photos", "the clean-up of Photos library copies failed"} {
		if !strings.Contains(out, w) {
			t.Errorf("log lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "error: removing deleted photos") || strings.Contains(out, "pruned:") {
		t.Errorf("pruned after a failed clean-up:\n%s", out)
	}
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM aesthetic_feedback WHERE photo_id = ?`, rs[1].Photo.ID).Scan(&n)
	if n != 1 {
		t.Errorf("the copy's rating is gone")
	}
}

// --force analyses moved photos as new ones, so their old records would
// be pruned with their names and ratings: the two are refused together,
// before anything is opened or indexed.
func TestIndexRefusesForcePrune(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	err := runIndex(&globals{data: data}, []string{"--force", "--prune", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "--prune can't be used with --force") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(data); err == nil {
		t.Error("the data folder was created")
	}
}

// The nightly prune keeps a missing Photos copy with a rating on it (no
// original catalogued to take it), and says so.
func TestPruneAfterIndexSparesLabelledCopies(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "Photos Library.photoslibrary")
	der := filepath.Join(lib, "resources", "derivatives", "A")
	os.MkdirAll(der, 0o755)
	os.WriteFile(filepath.Join(der, "other.jpeg"), []byte("x"), 0o644)
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &store.Result{Photo: store.Photo{Path: filepath.Join(der, "rated.jpeg"), Size: 1, MTime: 1, Fingerprint: "f", Version: store.SchemaVersion}}
	if err := st.Save([]*store.Result{r}); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`INSERT INTO aesthetic_feedback (photo_id, rater, rating, created_at) VALUES (?, 'sean', 8, 1)`, r.Photo.ID)
	var log bytes.Buffer
	res, ok := pruneAfterIndex(st, []string{lib}, nil, 0, &log)
	if !ok || len(res.Gone) != 0 || len(res.Spared) != 1 {
		t.Fatalf("ok=%v gone=%v spared=%v; log: %s", ok, res.Gone, res.Spared, &log)
	}
	if !strings.Contains(log.String(), "kept 1 copies photo apps made") {
		t.Errorf("log: %s", &log)
	}
}

package main

import (
	"bytes"
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

	t.Run("every folder there", func(t *testing.T) {
		st, a, b := setup(t)
		var log bytes.Buffer
		res, ok := pruneAfterIndex(st, []string{a + "/", b}, nil, &log)
		if !ok || len(res.Gone) != 2 || count(t, st) != 2 {
			t.Fatalf("ok=%v gone=%v photos=%d, want both deleted.jpg removed; log: %s", ok, res.Gone, count(t, st), &log)
		}
		if !strings.Contains(log.String(), "pruned: removed 2 deleted photos") {
			t.Errorf("log: %q", &log)
		}
	})

	for _, c := range []struct {
		name   string
		break_ func(a, b string) (missing []string) // what happens to the drives
		want   []string                             // in the error, per folder
	}{
		{"one drive unmounted (folder gone)", func(a, b string) []string { os.RemoveAll(b); return nil },
			[]string{"1 of the 2 folders", "Archive2 is not there"}},
		{"one drive unmounted (empty mount point)", func(a, b string) []string {
			os.Remove(filepath.Join(b, "keep.jpg"))
			return nil
		}, []string{"Archive2 is empty"}},
		{"both missing after a restart", func(a, b string) []string { os.RemoveAll(a); os.RemoveAll(b); return nil },
			[]string{"2 of the 2 folders", "Archive1 is not there", "Archive2 is not there"}},
		{"skipped at the start, mounted by the end", func(a, b string) []string { return []string{b} },
			[]string{"Archive2 was not there when indexing started"}},
		{"a file, not a folder", func(a, b string) []string {
			os.RemoveAll(b)
			os.WriteFile(b, []byte("x"), 0o644)
			return nil
		}, []string{"Archive2 is not a folder"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, a, b := setup(t)
			missing := c.break_(a, b)
			var log bytes.Buffer
			res, ok := pruneAfterIndex(st, []string{a, b}, missing, &log)
			if ok || len(res.Gone) != 0 || count(t, st) != 4 {
				t.Fatalf("ok=%v gone=%v photos=%d, want nothing removed from either folder", ok, res.Gone, count(t, st))
			}
			out := log.String()
			for _, w := range append([]string{"error: not removing deleted photos", "next run that finds every folder"}, c.want...) {
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
		if _, ok := pruneAfterIndex(st, []string{a, b}, nil, &log); ok || count(t, st) != 4 {
			t.Fatalf("pruned with an unreadable folder; log: %s", &log)
		}
		if !strings.Contains(log.String(), "Archive2 can't be read") {
			t.Errorf("log: %q", &log)
		}
	})
}

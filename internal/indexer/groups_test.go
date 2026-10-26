package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/auteursoft/archivis/internal/store"
)

// Regroup works out the folders with the groups, and on their own when
// only the folders are out of date (as after an upgrade to Albums).
func TestRegroupWorksOutFolders(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Save([]*store.Result{{Photo: store.Photo{Path: "/a/b/1.jpg", Size: 1, MTime: 1, Fingerprint: "x", Version: store.SchemaVersion}}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	o := st.GroupOptions()
	if _, ran, err := Regroup(ctx, dir, st, o, false); err != nil || !ran {
		t.Fatalf("Regroup: ran %v, %v", ran, err)
	}
	if st.GroupsStale(o) || st.FoldersStale() {
		t.Fatal("stale after Regroup")
	}
	if f, _ := st.Folder("/a"); f == nil || f.Total != 1 {
		t.Fatalf("Folder(/a) = %+v", f)
	}
	if err := st.SetMeta("folders_version", ""); err != nil { // folders alone out of date
		t.Fatal(err)
	}
	stats, ran, err := Regroup(ctx, dir, st, o, false)
	if err != nil || !ran || !stats.FoldersOnly || stats.Folders != 3 { // /a/b, /a, /
		t.Fatalf("folders-only Regroup: %+v, ran %v, %v", stats, ran, err)
	}
	if st.FoldersStale() {
		t.Fatal("folders stale after Regroup")
	}
	if _, ran, _ := Regroup(ctx, dir, st, o, false); ran {
		t.Fatal("Regroup ran with everything up to date")
	}
}

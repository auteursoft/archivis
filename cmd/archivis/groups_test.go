package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/store"
)

// groups rebuild works out the groups, and is refused (not queued) while
// another process regroups; an index lock does not stop it.
func TestGroupsCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARCHIVIS_DATA", dir)
	t.Setenv("HOME", t.TempDir())
	g := &globals{data: dir}
	unlockIndex, err := indexer.LockIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockIndex()
	for _, args := range [][]string{{"status"}, {"rebuild"}, {}} {
		if err := runGroups(g, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	unlock, err := indexer.LockGroups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runGroups(g, []string{"rebuild"}); !errors.Is(err, indexer.ErrGroupsRunning) {
		t.Fatalf("rebuild while another regroups: %v", err)
	}
	unlock()
	if err := runGroups(g, []string{"nonsense"}); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}

// An index run whose folders are all offline still records them for Albums
// (in an existing catalogue), and still fails.
func TestIndexRecordsOfflineRoots(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARCHIVIS_DATA", dir)
	t.Setenv("HOME", t.TempDir())
	st, err := store.Open(indexer.Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	gone := filepath.Join(t.TempDir(), "unmounted-drive")
	if err := runIndex(&globals{data: dir}, []string{gone}); err == nil {
		t.Fatal("indexing only missing folders succeeded")
	}
	st, err = store.Open(indexer.Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if roots, _ := st.Roots(); len(roots) != 1 || roots[0] != gone {
		t.Fatalf("Roots = %v, want [%s]", roots, gone)
	}
	// no catalogue: none is created just to record them
	empty := t.TempDir()
	runIndex(&globals{data: empty}, []string{gone})
	if _, err := os.Stat(indexer.Layout{Dir: empty}.DB()); err == nil {
		t.Fatal("a catalogue was created for offline folders")
	}
}

// roots name gives a recorded folder (or the iPhone album) a display name,
// as typed or with a trailing separator, and refuses unknown folders.
func TestRootsName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARCHIVIS_DATA", dir)
	g := &globals{data: dir}
	root := filepath.Join(t.TempDir(), "Photo Stage")
	if err := runRoots(g, []string{"add", root}); err != nil {
		t.Fatal(err)
	}
	if err := runRoots(g, []string{"name", root + string(filepath.Separator), "Stage"}); err != nil {
		t.Fatal(err)
	}
	if err := runRoots(g, []string{"name", "iPhone", "Phones"}); err != nil {
		t.Fatal(err)
	}
	if err := runRoots(g, []string{"name", filepath.Join(t.TempDir(), "elsewhere"), "x"}); err == nil {
		t.Fatal("named a folder never recorded")
	}
	st, err := store.Open(indexer.Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if names, _ := st.AlbumNames(); names[root] != "Stage" || names[store.IPhoneAlbum] != "Phones" {
		t.Fatalf("names = %v", names)
	}
}

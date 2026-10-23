package main

import (
	"errors"
	"testing"

	"github.com/auteursoft/archivis/internal/indexer"
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

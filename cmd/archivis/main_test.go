package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"archivis/internal/indexer"
)

func TestSplitCommand(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		cmd  string
		args []string
	}{
		{[]string{"stats"}, "stats", []string{}},
		{[]string{"--data", "/x", "stats", "-limit", "3"}, "stats", []string{"-limit", "3", "--data", "/x"}},
		{[]string{"--data=/x", "index", "/p"}, "index", []string{"/p", "--data=/x"}},
		{[]string{"--provider", "cpu"}, "", []string{"--provider", "cpu"}},
	} {
		cmd, args := splitCommand(tc.in)
		if cmd != tc.cmd || !reflect.DeepEqual(args, tc.args) {
			t.Errorf("%v: got %q %v", tc.in, cmd, args)
		}
	}
}

// A catalogue made before the rename (~/.photodex) is moved to ~/.archivis
// once, and an existing ~/.archivis is never overwritten.
func TestAdoptOldDataDir(t *testing.T) {
	home := t.TempDir()
	old, dir := filepath.Join(home, ".photodex"), filepath.Join(home, ".archivis")
	if got := adoptOldDataDir(old, dir); got != dir {
		t.Fatalf("fresh install: got %s", got)
	}
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "photodex.db"), []byte("labels"), 0o644)
	if got := adoptOldDataDir(old, dir); got != dir {
		t.Fatalf("got %s, want %s", got, dir)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "photodex.db")); string(b) != "labels" {
		t.Fatal("old catalogue not moved")
	}
	os.MkdirAll(old, 0o755) // both exist: keep the new one, leave the old alone
	if got := adoptOldDataDir(old, dir); got != dir {
		t.Fatalf("got %s", got)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("old directory touched although the new one exists")
	}
}

// Only a run that relies on the default location adopts ~/.photodex; an
// explicit --data (or data variable) must never move anything.
func TestUsesDefaultData(t *testing.T) {
	t.Setenv("ARCHIVIS_DATA", "")
	t.Setenv("PHOTODEX_DATA", "")
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"-limit", "3"}, true},
		{[]string{"--data", "/x"}, false},
		{[]string{"-data=/x"}, false},
		{[]string{"--data=/x", "stats"}, false},
		{[]string{"--", "--data"}, true}, // after "--" it is a folder name, not a flag
		{[]string{"/photos/data"}, true},
	} {
		if got := usesDefaultData(tc.args); got != tc.want {
			t.Errorf("%v: got %v", tc.args, got)
		}
	}
	t.Setenv("ARCHIVIS_DATA", "/y")
	if usesDefaultData(nil) {
		t.Error("ARCHIVIS_DATA set but treated as default")
	}
}

// Re-exporting with --copy into a folder holding links from an earlier
// export must never write through them into the originals.
func TestExportNeverWritesThroughLinks(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "IMG_0001.JPG")
	other := filepath.Join(dir, "IMG_0002.JPG")
	os.WriteFile(orig, []byte("original photo bytes"), 0o644)
	os.WriteFile(other, []byte("a different photo"), 0o644)
	dest := filepath.Join(dir, "picks")
	os.Mkdir(dest, 0o755)
	target := filepath.Join(dest, "0001_IMG_0001.JPG")

	if ok, err := exportPhoto(orig, target, false); !ok || err != nil {
		t.Fatalf("link: %v %v", ok, err)
	}
	if ok, _ := exportPhoto(orig, target, false); !ok {
		t.Fatal("re-linking the same photo should count as exported")
	}
	// same name, different photo, link mode: must not count or change anything
	if ok, _ := exportPhoto(other, target, false); ok {
		t.Fatal("a link to another photo was counted as exported")
	}
	// now --copy over the link
	if ok, err := exportPhoto(orig, target, true); !ok || err != nil {
		t.Fatalf("copy: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(orig); string(b) != "original photo bytes" {
		t.Fatalf("original was modified: %q", b)
	}
	st, _ := os.Lstat(target)
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("target is still a link")
	}
	if b, _ := os.ReadFile(target); string(b) != "original photo bytes" {
		t.Fatalf("copy has wrong content: %q", b)
	}
	// a different photo copied onto an existing real file: left alone
	if ok, _ := exportPhoto(other, target, true); ok {
		t.Fatal("existing file overwritten")
	}
	if b, _ := os.ReadFile(target); string(b) != "original photo bytes" {
		t.Fatalf("existing export changed: %q", b)
	}
	// a link pointing at another photo, then --copy of a different photo:
	// the original behind the link must survive
	t2 := filepath.Join(dest, "0002_x.JPG")
	os.Symlink(other, t2)
	exportPhoto(orig, t2, true)
	if b, _ := os.ReadFile(other); string(b) != "a different photo" {
		t.Fatalf("photo behind a link was overwritten: %q", b)
	}
	if ents, _ := os.ReadDir(dest); len(ents) != 2 {
		t.Fatalf("leftover temporary files: %d entries", len(ents))
	}
}

func TestLoopbackOnly(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8088": true, "localhost:8088": true, "[::1]:8088": true,
		":8088": false, "0.0.0.0:8088": false, "[::]:8088": false, "192.168.1.5:8088": false, "photos.lan:8088": false,
	} {
		if got := loopbackOnly(addr); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
	for addr, want := range map[string]string{":8088": "localhost:8088", "0.0.0.0:8088": "localhost:8088", "[::]:8088": "localhost:8088", "127.0.0.1:8088": "127.0.0.1:8088"} {
		if got := browseURL(addr); got != want {
			t.Errorf("browseURL(%s) = %s", addr, got)
		}
	}
}

func TestFiniteFlags(t *testing.T) {
	for _, args := range [][]string{{"-x", "NaN"}, {"-x", "inf"}, {"-x", "-Inf"}} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.Float64("x", 0.4, "")
		fs.Parse(args)
		if finiteFlags(fs) == nil {
			t.Errorf("%v accepted", args)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Float64("x", 0.4, "")
	fs.Parse([]string{"-x", "0.55"})
	if err := finiteFlags(fs); err != nil {
		t.Fatal(err)
	}
}

// Changing the aesthetic blend is refused while an index runs (it would keep
// scoring new photos with the previous models); a dry run and listing work.
func TestAestheticChangesWaitForIndex(t *testing.T) {
	dir := t.TempDir()
	// the commands' --data default; never the real ~/.archivis
	t.Setenv("ARCHIVIS_DATA", dir)
	t.Setenv("HOME", t.TempDir())
	unlock, err := indexer.LockIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := &globals{data: dir}
	for _, args := range [][]string{{"use", "eva"}, {"train"}} {
		if err := runAesthetic(g, args); !errors.Is(err, indexer.ErrIndexRunning) {
			t.Errorf("%v during an index: %v", args, err)
		}
	}
	if err := runAesthetic(g, []string{"models"}); err != nil {
		t.Errorf("models during an index: %v", err)
	}
	if err := runAesthetic(g, []string{"train", "--dry-run"}); errors.Is(err, indexer.ErrIndexRunning) {
		t.Errorf("dry run refused: %v", err)
	}
	unlock()
	if err := runAesthetic(g, []string{"use", "eva"}); err != nil {
		t.Errorf("use after the index: %v", err)
	}
}

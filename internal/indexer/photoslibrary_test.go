package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestPhotosLibraryChildAllowed(t *testing.T) {
	lib := filepath.Join("Users", "sean", "Pictures", "Photos Library.photoslibrary")
	for _, tc := range []struct {
		parent, base string
		want         bool
	}{
		{lib, "originals", true},
		{lib, "Masters", true},    // older libraries
		{lib, "ORIGINALS", true},  // case-insensitive volumes
		{lib, "resources", false}, // derivatives, renders, thumbnails
		{lib, "database", false},
		{lib, "private", false},
		{lib, "scopes", false},
		{lib, "Photos.sqlite", false},
		{filepath.Join(lib, "originals"), "A", true},                    // below originals, anything
		{filepath.Join("Users", "sean", "Pictures"), "resources", true}, // not a library
		{"Other.PHOTOSLIBRARY", "resources", false},
	} {
		if got := photosLibraryChildAllowed(tc.parent, tc.base); got != tc.want {
			t.Errorf("%s / %s: %v, want %v", tc.parent, tc.base, got, tc.want)
		}
	}
}

// A walk over a fake library finds only the originals, whether the library
// is inside a root or is the root.
func TestWalkPhotosLibrary(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "Pictures", "Photos Library.photoslibrary")
	files := []string{
		"originals/A/AAAA1111-0000-0000-0000-000000000001.jpeg",
		"originals/B/BBBB2222-0000-0000-0000-000000000002.heic",
		"resources/derivatives/A/AAAA1111-0000-0000-0000-000000000001_1_105_c.jpeg",
		"resources/derivatives/masters/B/BBBB2222-0000-0000-0000-000000000002_4_5005_c.jpeg",
		"resources/renders/A/AAAA1111-0000-0000-0000-000000000001_1_201_a.jpeg",
		"private/com.apple.photoanalysisd/x.jpg",
	}
	for _, f := range files {
		p := filepath.Join(lib, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	other := filepath.Join(root, "Pictures", "trip", "resources", "beach.jpg") // an ordinary folder named resources
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("x"), 0o644)

	walk := func(start string) []string {
		ix := &Indexer{}
		var got []string
		filepath.WalkDir(start, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p != start && ix.excluded(p, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				rel, _ := filepath.Rel(root, p)
				got = append(got, filepath.ToSlash(rel))
			}
			return nil
		})
		sort.Strings(got)
		return got
	}
	want := "Pictures/Photos Library.photoslibrary/originals/A/AAAA1111-0000-0000-0000-000000000001.jpeg," +
		"Pictures/Photos Library.photoslibrary/originals/B/BBBB2222-0000-0000-0000-000000000002.heic," +
		"Pictures/trip/resources/beach.jpg"
	if got := strings.Join(walk(root), ","); got != want {
		t.Fatalf("walk from a root above the library:\n got %s\nwant %s", got, want)
	}
	if got := walk(lib); len(got) != 2 || !strings.Contains(got[0], "/originals/") || !strings.Contains(got[1], "/originals/") {
		t.Fatalf("walk with the library as the root: %v", got)
	}
}

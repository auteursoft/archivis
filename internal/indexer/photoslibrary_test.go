package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/store"
)

func TestPackageChildAllowed(t *testing.T) {
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
		// other photo apps: their own copies are skipped, their originals kept
		{"iPhoto Library.photolibrary", "Masters", true},
		{"iPhoto Library.photolibrary", "Originals", true}, // iPhoto 8 and older
		{"iPhoto Library.photolibrary", "Thumbnails", false},
		{"iPhoto Library.photolibrary", "Previews", false},
		{"Aperture Library.aplibrary", "Masters", true},
		{"Aperture Library.aplibrary", "Thumbnails", false},
		{"Aperture Library.aplibrary", "Previews", false},
		{"Weddings.cocatalog", "Originals", true},
		{"Weddings.cocatalog", "Cache", false},
		{filepath.Join("Sessions", "Smith"), "CaptureOne", false}, // a session's cache and settings, anywhere
		{filepath.Join("Sessions", "Smith"), "Capture", true},
	} {
		if got := store.PackageChildAllowed(tc.parent, tc.base); got != tc.want {
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

// A walk over Capture One, iPhoto and Aperture libraries finds only their
// originals, not their thumbnails, previews or proxies.
func TestWalkAppCaches(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{
		"Weddings.cocatalog/Originals/2024/IMG_1.CR3",
		"Weddings.cocatalog/Cache/Thumbnails/IMG_1.jpg",
		"Weddings.cocatalog/Cache/Previews/IMG_1.jpg",
		"Sessions/Smith/Capture/IMG_2.CR3",
		"Sessions/Smith/Capture/CaptureOne/Cache/Thumbnails/IMG_2.jpg",
		"Sessions/Smith/Output/IMG_2.jpg", // an export: a photo
		"iPhoto Library.photolibrary/Masters/2012/IMG_3.jpg",
		"iPhoto Library.photolibrary/Thumbnails/2012/IMG_3.jpg",
		"iPhoto Library.photolibrary/Previews/2012/IMG_3.jpg",
		"Aperture Library.aplibrary/Masters/2010/IMG_4.NEF",
		"Aperture Library.aplibrary/Thumbnails/IMG_4.jpg",
	} {
		p := filepath.Join(root, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	ix := &Indexer{}
	var got []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != root && ix.excluded(p, d.IsDir()) {
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
	want := "Aperture Library.aplibrary/Masters/2010/IMG_4.NEF,Sessions/Smith/Capture/IMG_2.CR3,Sessions/Smith/Output/IMG_2.jpg," +
		"Weddings.cocatalog/Originals/2024/IMG_1.CR3,iPhoto Library.photolibrary/Masters/2012/IMG_3.jpg"
	if g := strings.Join(got, ","); g != want {
		t.Fatalf("walk:\n got %s\nwant %s", g, want)
	}
}

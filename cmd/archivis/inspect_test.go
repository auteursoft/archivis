package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
)

func TestInspectWarnings(t *testing.T) {
	good := imageio.Meta{Make: "SONY", Model: "ILCE-7RM5", Lens: "FE 24-70mm F2.8 GM II", TakenAt: time.Unix(1, 0), Width: 9504, Height: 6336}
	for _, c := range []struct {
		name string
		d    imageio.Decoded
		want []string // substrings expected; nil = no warnings
	}{
		{"clean RAW", imageio.Decoded{Format: "raw:arw", SrcW: 7360, SrcH: 4912, Meta: good}, nil},
		{"small Sony-style preview", imageio.Decoded{Format: "raw:arw", SrcW: 1616, SrcH: 1080, Meta: good}, nil},
		{"tiny preview", imageio.Decoded{Format: "raw:arw", SrcW: 640, SrcH: 424, Meta: good}, []string{"small preview"}},
		{"cropped preview", imageio.Decoded{Format: "raw:nef", SrcW: 1920, SrcH: 1080, Meta: good}, []string{"preview shape"}},
		{"thumbnail-sized record is not compared", imageio.Decoded{Format: "raw:nef", SrcW: 4256, SrcH: 2832,
			Meta: imageio.Meta{Make: "NIKON", Lens: "28mm f/1.4", TakenAt: time.Unix(1, 0), Width: 160, Height: 120}}, nil},
		{"missing metadata", imageio.Decoded{Format: "raw:nef", SrcW: 7360, SrcH: 4912}, []string{"no camera", "no capture time", "no lens"}},
		{"camera JPEG without a lens", imageio.Decoded{Format: "jpeg", SrcW: 6000, SrcH: 4000,
			Meta: imageio.Meta{Make: "NIKON CORPORATION", Model: "NIKON D810", TakenAt: time.Unix(1, 0)}}, []string{"no lens"}},
		{"scan: no camera, so no lens warning", imageio.Decoded{Format: "jpeg", SrcW: 3000, SrcH: 2000,
			Meta: imageio.Meta{TakenAt: time.Unix(1, 0)}}, []string{"no camera"}},
		{"sideways", imageio.Decoded{Format: "heif", SrcW: 4032, SrcH: 3024, Meta: imageio.Meta{Make: "Apple", TakenAt: time.Unix(1, 0), Orientation: 6}}, []string{"decoded as landscape"}},
	} {
		got := strings.Join(inspectWarnings(&c.d), "; ")
		if c.name == "scan: no camera, so no lens warning" && strings.Contains(got, "no lens") {
			t.Errorf("%s: %q", c.name, got)
		}
		if c.want == nil && got != "" {
			t.Errorf("%s: unexpected warnings %q", c.name, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
	}
}

// The whole command: photos with the same name in two card folders stay two
// rows and two saved previews; a corrupt file is reported, not fatal.
func TestInspectCommand(t *testing.T) {
	root := t.TempDir()
	img := imageio.Resize(&imageio.RGB{W: 2, H: 2, Pix: make([]byte, 12)}, 64, 48, imageio.Bilinear)
	jpg, err := imageio.EncodeJPEG(img, 90)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"100MSDCF", "101MSDCF"} {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
		os.WriteFile(filepath.Join(root, dir, "DSC00001.JPG"), jpg, 0o644)
	}
	os.WriteFile(filepath.Join(root, "broken.jpg"), []byte("not a jpeg at all"), 0o644)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("not a photo"), 0o644)
	prev := t.TempDir()
	var out bytes.Buffer
	if err := inspect(&out, []string{root}, prev); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"100MSDCF/DSC00001.JPG", "101MSDCF/DSC00001.JPG", "broken.jpg", "ERROR:", "3 files, 3 with errors or warnings"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	saved, _ := filepath.Glob(filepath.Join(prev, "*.jpg"))
	if len(saved) != 2 {
		t.Fatalf("saved previews %v, want one per decoded photo", saved)
	}
	// the same folder given twice: still distinct
	out.Reset()
	if err := inspect(&out, []string{filepath.Join(root, "100MSDCF"), filepath.Join(root, "101MSDCF")}, prev); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "DSC00001.JPG (2)") {
		t.Errorf("same name under two arguments not told apart:\n%s", out.String())
	}
	// names that flatten to the same file name still get a preview each
	clash := t.TempDir()
	os.MkdirAll(filepath.Join(clash, "a"), 0o755)
	os.WriteFile(filepath.Join(clash, "a", "b.jpg"), jpg, 0o644)
	os.WriteFile(filepath.Join(clash, "a__b.jpg"), jpg, 0o644)
	os.WriteFile(filepath.Join(clash, "A__B.JPG"), jpg, 0o644)
	prev2 := t.TempDir()
	if err := inspect(&out, []string{clash}, prev2); err != nil {
		t.Fatal(err)
	}
	// On case-insensitive disks (macOS, Windows) A__B.JPG replaced a__b.jpg,
	// so count the photos that exist: one preview each.
	photos := 0
	filepath.WalkDir(clash, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			photos++
		}
		return nil
	})
	if saved, _ := filepath.Glob(filepath.Join(prev2, "*.jpg")); len(saved) != photos || photos < 2 {
		t.Fatalf("%d photos but previews %v", photos, saved)
	}
	if err := inspect(&out, []string{t.TempDir()}, ""); err == nil {
		t.Error("empty folder accepted")
	}
}

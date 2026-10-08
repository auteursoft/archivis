package imageio

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRealSamples checks real camera files (tools/samples/fetch.sh
// downloads them): the preview found, its size and orientation, and the
// metadata. Without ARCHIVIS_SAMPLES it is skipped; with
// ARCHIVIS_REQUIRE_SAMPLES=1 (CI) a missing sample fails it.
func TestRealSamples(t *testing.T) {
	dir := os.Getenv("ARCHIVIS_SAMPLES")
	skip := t.Skip
	if os.Getenv("ARCHIVIS_REQUIRE_SAMPLES") != "" {
		skip = t.Fatal
	}
	if dir == "" {
		skip("ARCHIVIS_SAMPLES not set (tools/samples/fetch.sh DIR)")
	}
	for _, c := range []struct {
		file, format string
		w, h         int // upright size of the decoded source
		make, model  string
		lens         string
		taken        string
		focal        float64
		heif         bool
	}{
		// orientation 3 (camera upside down): an upright manuscript page
		{"M0054341_01_00005.cr2", "raw:cr2", 1936, 1288, "Canon", "Canon EOS DIGITAL REBEL XTi", "EF-S18-55mm f/3.5-5.6", "2009-05-20 02:22:05", 48, false},
		{"RAW_CANON_40D_SRAW_V103.CR2", "raw:cr2", 1936, 1288, "Canon", "Canon EOS 40D", "EF-S17-85mm f/4-5.6 IS USM", "2007-09-14 11:32:02", 17, false},
		{"RAW_CANON_5DMARK2_PREPROD.CR2", "raw:cr2", 5616, 3744, "Canon", "Canon EOS 5D Mark II", "EF50mm f/1.2L USM", "2008-10-29 20:05:00", 50, false},
		{"iss030e122639.NEF", "raw:nef", 4256, 2832, "NIKON CORPORATION", "NIKON D3S", "28mm f/1.4", "2012-03-04 17:20:59", 28, false},
		{"iss042e297200.NEF", "raw:nef", 4928, 3280, "NIKON CORPORATION", "NIKON D4", "28mm f/1.4", "2015-02-28 20:51:19", 28, false},
		{"canon_eos_6d.dng", "raw:dng", 1024, 683, "Canon", "Canon EOS 6D", "EF24-105mm f/4L IS USM", "2019-06-06 07:29:51", 65, false},
		// rotated by the HEIF container (irot) and in EXIF
		{"iphone_15_pro.heic", "heif", 3024, 4032, "Apple", "iPhone 15 Pro", "Apple iPhone 15 Pro back triple camera 2.22mm f/2.2", "2023-10-31 11:44:43", 2.22, true},
		// rotated only in EXIF: was decoded sideways before
		{"iphone_7.heic", "heif", 3024, 4032, "Apple", "iPhone 7", "Apple iPhone 7 back camera 3.99mm f/1.8", "2018-09-10 12:16:13", 3.99, true},
	} {
		t.Run(c.file, func(t *testing.T) {
			p := filepath.Join(dir, c.file)
			if _, err := os.Stat(p); err != nil {
				skip(err)
			}
			d, err := Load(p, 800)
			if c.heif && err != nil {
				skip("no HEIF converter here: ", err) // not every machine has one
			}
			if err != nil {
				t.Fatal(err)
			}
			m := d.Meta
			if d.Format != c.format || d.SrcW != c.w || d.SrcH != c.h {
				t.Errorf("format %s, %dx%d; want %s, %dx%d", d.Format, d.SrcW, d.SrcH, c.format, c.w, c.h)
			}
			if (d.Img.W > d.Img.H) != (c.w > c.h) {
				t.Errorf("decoded %dx%d: wrong way round", d.Img.W, d.Img.H)
			}
			if m.Make != c.make || m.Model != c.model || m.Lens != c.lens {
				t.Errorf("camera %q %q lens %q; want %q %q %q", m.Make, m.Model, m.Lens, c.make, c.model, c.lens)
			}
			if got := m.TakenAt.Format("2006-01-02 15:04:05"); got != c.taken {
				t.Errorf("taken %s, want %s", got, c.taken)
			}
			if m.FocalLength < c.focal-0.01 || m.FocalLength > c.focal+0.01 {
				t.Errorf("focal length %.2f, want %.2f", m.FocalLength, c.focal)
			}
		})
	}
}

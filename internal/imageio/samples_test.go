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
	required := os.Getenv("ARCHIVIS_REQUIRE_SAMPLES") != ""
	// skipper returns t's own Skip, or Fatal when samples are required. It
	// must be called on each subtest's t: Skip or Fatal on a parent test
	// from inside a subtest is itself a failure.
	skipper := func(t *testing.T) func(...any) {
		if required {
			return t.Fatal
		}
		return t.Skip
	}
	if dir == "" {
		skipper(t)("ARCHIVIS_SAMPLES not set (tools/samples/fetch.sh DIR)")
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
		// current bodies (raw.pixls.us). Nikon: the full-size JpgFromRaw in
		// SubIFD0, also beside High Efficiency* (TicoRAW) data.
		{"DSC_1882.NEF", "raw:nef", 7360, 4912, "NIKON CORPORATION", "NIKON D810", "50mm f/1.4", "2017-12-05 23:48:05", 50, false},
		{"Nikon-D850-14bit-lossless-compressed.NEF", "raw:nef", 8256, 5504, "NIKON CORPORATION", "NIKON D850", "35mm f/1.4", "2017-10-15 17:34:59", 35, false},
		{"Z72_0027.NEF", "raw:nef", 8256, 5504, "NIKON CORPORATION", "NIKON Z 7_2", "NIKON NIKKOR Z 24-70mm f/2.8 S", "2020-12-12 11:47:42", 70, false},
		{"Nikon_Z8_raw_14_bit_lossless_compression.NEF", "raw:nef", 8256, 5504, "NIKON CORPORATION", "NIKON Z 8", "NIKON NIKKOR Z 14-24mm f/2.8 S", "2023-06-02 18:52:32", 24, false},
		{"Nikon_Z8_raw_high_efficiency_hight.NEF", "raw:nef", 8256, 5504, "NIKON CORPORATION", "NIKON Z 8", "NIKON NIKKOR Z 14-24mm f/2.8 S", "2023-06-02 18:53:02", 24, false},
		{"Nikon_-_NIKON_Z_9_-_14bit_compressed_(Lossless).NEF", "raw:nef", 8256, 5504, "NIKON CORPORATION", "NIKON Z 9", "NIKON NIKKOR Z 14-30mm f/4 S", "2022-02-05 13:45:03", 18, false},
		{"Nikon_-_NIKON_Z_9_-_14bit_compressed_(Lossy_High_Efficiency_Star).NEF", "raw:nef", 8256, 5504, "NIKON CORPORATION", "NIKON Z 9", "NIKON NIKKOR Z 14-30mm f/4 S", "2022-02-05 13:46:41", 18, false},
		{"Nikon_Z6__3_Lossless_compressed_FX.NEF", "raw:nef", 6048, 4032, "NIKON CORPORATION", "NIKON Z6_3", "NIKON NIKKOR Z 24-200mm f/4-6.3 VR", "2024-06-29 15:36:48", 86, false},
		{"Nikon_Z6__3_High_Efficiency_Star_FX.NEF", "raw:nef", 6048, 4032, "NIKON CORPORATION", "NIKON Z6_3", "NIKON NIKKOR Z 24-200mm f/4-6.3 VR", "2024-06-29 15:37:42", 86, false},
		// uncompressed ARW: the 1616x1080 preview is the largest it holds
		{"DSC00395.ARW", "raw:arw", 1616, 1080, "SONY", "ILCE-7RM4", "FE 24-70mm F4 ZA OSS", "2019-10-13 07:06:39", 25, false},
		// newer ARW: a full-size JPEG in IFD2
		{"7RM5-LosslessCompressedLarge.ARW", "raw:arw", 9504, 6336, "SONY", "ILCE-7RM5", "FE 50mm F1.2 GM", "2022-12-17 16:03:36", 50, false},
		{"A1_07627_lossless_compressed_large_full_frame.ARW", "raw:arw", 8640, 5760, "SONY", "ILCE-1M2", "FE 70-200mm F2.8 GM OSS II", "2025-05-09 09:35:33", 133, false},
		// a third-party lens that records only its focal range
		{"B13A0729.CR2", "raw:cr2", 6720, 4480, "Canon", "Canon EOS 5D Mark IV", "24-70mm", "2017-01-26 09:36:26", 40, false},
		// CR3: metadata from the CMT boxes in moov, not the embedded JPEGs
		{"Canon_EOS_R5_RAW_ISO_100_nocrop_nodual.CR3", "raw:cr3", 8192, 5464, "Canon", "Canon EOS R5", "50mm F1.4 DG HSM | Art 014", "2021-05-15 13:02:02", 50, false},
		{"canon-r6m2-img_0346-raw-normal-full.cr3", "raw:cr3", 6000, 4000, "Canon", "Canon EOS R6m2", "EF50mm f/1.8 II", "2023-02-19 14:31:58", 50, false},
		{"_C3A1200.CR3", "raw:cr3", 6000, 4000, "Canon", "Canon EOS R3", "RF100-400mm F5.6-8 IS USM", "2021-12-29 10:57:50", 174, false},
		{"Canon-90D-RAW-ISO-100.CR3", "raw:cr3", 6960, 4640, "Canon", "Canon EOS 90D", "EF-S18-55mm f/3.5-5.6 IS", "2021-05-12 18:45:30", 20, false},
		{"canon_eos_6d.dng", "raw:dng", 1024, 683, "Canon", "Canon EOS 6D", "EF24-105mm f/4L IS USM", "2019-06-06 07:29:51", 65, false},
		// rotated by the HEIF container (irot) and in EXIF
		{"iphone_15_pro.heic", "heif", 3024, 4032, "Apple", "iPhone 15 Pro", "Apple iPhone 15 Pro back triple camera 2.22mm f/2.2", "2023-10-31 11:44:43", 2.22, true},
		// rotated only in EXIF: was decoded sideways before
		{"iphone_7.heic", "heif", 3024, 4032, "Apple", "iPhone 7", "Apple iPhone 7 back camera 3.99mm f/1.8", "2018-09-10 12:16:13", 3.99, true},
	} {
		t.Run(c.file, func(t *testing.T) {
			skip := skipper(t)
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

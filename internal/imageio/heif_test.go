package imageio

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func mkbox(typ string, body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(b)))
	return append(append(out, typ...), b...)
}

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// heifFile builds a minimal HEIF: primary item 1 with an ispe of w x h, an
// irot if rot != 0, a thumbnail item 2 with its own properties, and an EXIF
// block with the given orientation (0 = none).
func heifFile(w, h uint32, rot byte, exifOrient uint16) []byte {
	full := []byte{0, 0, 0, 0}
	props := [][]byte{
		mkbox("ispe", full, u32(160), u32(120)), // 1: the thumbnail's
		mkbox("ispe", full, u32(w), u32(h)),     // 2: the primary's
	}
	assoc := []byte{2} // primary: ispe at index 2
	if rot != 0 {
		props = append(props, mkbox("irot", []byte{rot}))
		assoc = append(assoc, 3)
	}
	ipma := bytes.Join([][]byte{full, u32(2),
		u16(2), {1, 1}, // thumbnail item 2: property 1 (listed first)
		u16(1), {byte(len(assoc))}, assoc,
	}, nil)
	meta := mkbox("meta", full,
		mkbox("hdlr", full, u32(0), []byte("pict"), make([]byte, 13)),
		mkbox("pitm", full, u16(1)),
		mkbox("iprp", mkbox("ipco", props...), mkbox("ipma", ipma)),
	)
	file := append(mkbox("ftyp", []byte("heic"), u32(0), []byte("mif1heic")), meta...)
	if exifOrient != 0 {
		tiff := []byte("MM\x00\x2a")
		tiff = append(tiff, u32(8)...)
		tiff = append(tiff, u16(1)...)
		tiff = append(tiff, u16(tagOrientation)...)
		tiff = append(tiff, u16(tShort)...)
		tiff = append(tiff, u32(1)...)
		tiff = append(tiff, u16(exifOrient)...)
		tiff = append(tiff, 0, 0, 0, 0, 0, 0)
		file = append(file, mkbox("mdat", u32(6), []byte("Exif\x00\x00"), tiff)...)
	}
	return file
}

func TestReadHEIFInfo(t *testing.T) {
	for _, c := range []struct {
		name string
		file []byte
		want heifInfo
	}{
		{"rotation in container and EXIF (current iPhones)", heifFile(4032, 3024, 3, 6), heifInfo{4032, 3024, true, 6}},
		{"rotation only in EXIF (re-saved files)", heifFile(4032, 3024, 0, 6), heifInfo{4032, 3024, false, 6}},
		{"irot of 0 is no rotation", heifFile(4032, 3024, 4, 1), heifInfo{4032, 3024, false, 1}},
		{"no EXIF", heifFile(800, 600, 0, 0), heifInfo{800, 600, false, 0}},
		{"garbage", []byte("not a heif file at all"), heifInfo{}},
	} {
		got := readHEIFInfo(bytes.NewReader(c.file), int64(len(c.file)))
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestHEIFOrientation(t *testing.T) {
	stored := heifInfo{W: 4032, H: 3024}
	for _, c := range []struct {
		name      string
		info      heifInfo
		w, h      int
		converter string
		want      int
	}{
		{"container rotation was applied", heifInfo{4032, 3024, true, 6}, 3024, 4032, "heif-convert", 1},
		{"EXIF-only 90°, converter left pixels as stored", heifInfo{4032, 3024, false, 6}, 4032, 3024, "heif-convert", 6},
		{"EXIF-only 90°, converter already rotated", heifInfo{4032, 3024, false, 6}, 3024, 4032, "sips", 1},
		{"EXIF-only 90°, size unknown", heifInfo{0, 0, false, 8}, 4032, 3024, "magick", 1},
		{"EXIF-only 180°, libheif ignores EXIF", heifInfo{4032, 3024, false, 3}, 4032, 3024, "vips", 3},
		{"EXIF-only 180°, sips applies EXIF", heifInfo{4032, 3024, false, 3}, 4032, 3024, "sips", 1},
		{"upright", stored, 4032, 3024, "heif-convert", 1},
		{"square, cannot tell", heifInfo{3000, 3000, false, 6}, 3000, 3000, "heif-convert", 1},
	} {
		if got := heifOrientation(c.info, c.w, c.h, c.converter); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestNikonLensName(t *testing.T) {
	for in, want := range map[[4]float64]string{
		{24, 70, 2.8, 2.8}: "24-70mm f/2.8",
		{18, 55, 3.5, 5.6}: "18-55mm f/3.5-5.6",
		{50, 50, 1.4, 1.4}: "50mm f/1.4",
		{0, 0, 0, 0}:       "",
		{70, 24, 2.8, 2.8}: "",
		{24, 70, 0, 2.8}:   "",
	} {
		if got := nikonLensName(in[:]); got != want {
			t.Errorf("%v: %q, want %q", in, got, want)
		}
	}
}

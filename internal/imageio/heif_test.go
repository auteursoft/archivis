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

// heifOpts describes a synthetic HEIF file.
type heifOpts struct {
	w, h       uint32
	rot        byte   // irot angle code (0 = no irot property)
	exifOrient uint16 // 0 = no EXIF item
	exifFor    uint16 // item the EXIF describes (cdsc); 0 = no reference
	marker     bool   // put "Exif\0\0" before the TIFF header, as Apple does
	pad        int    // bytes of image data before the EXIF payload
	inIdat     bool   // store the EXIF in the meta box (construction method 1)
}

// heifFile builds a minimal HEIF: primary item 1 with an ispe of w x h (and
// an irot if rot != 0), a thumbnail item 2 with its own ispe, and EXIF item
// 3 located through iloc, as encoders write them.
func heifFile(o heifOpts) []byte {
	full := []byte{0, 0, 0, 0}
	props := [][]byte{
		mkbox("ispe", full, u32(160), u32(120)), // 1: the thumbnail's
		mkbox("ispe", full, u32(o.w), u32(o.h)), // 2: the primary's
	}
	assoc := []byte{2}
	if o.rot != 0 {
		props = append(props, mkbox("irot", []byte{o.rot}))
		assoc = append(assoc, 3)
	}
	ipma := bytes.Join([][]byte{full, u32(2),
		u16(2), {1, 1}, // thumbnail: property 1
		u16(1), {byte(len(assoc))}, assoc,
	}, nil)
	infe := func(id uint16, typ string) []byte {
		return mkbox("infe", []byte{2, 0, 0, 0}, u16(id), u16(0), []byte(typ), []byte{0})
	}
	items := [][]byte{infe(1, "hvc1"), infe(2, "hvc1")}
	var payload []byte
	if o.exifOrient != 0 {
		items = append(items, infe(3, "Exif"))
		tiff := bytes.Join([][]byte{[]byte("MM\x00\x2a"), u32(8), u16(1),
			u16(tagOrientation), u16(tShort), u32(1), u16(o.exifOrient), {0, 0, 0, 0, 0, 0}}, nil)
		if o.marker {
			payload = bytes.Join([][]byte{u32(6), []byte("Exif\x00\x00"), tiff}, nil)
		} else {
			payload = append(u32(0), tiff...)
		}
	}
	iinf := mkbox("iinf", full[:4], u16(uint16(len(items))), bytes.Join(items, nil))
	var iref []byte
	if o.exifFor != 0 {
		iref = mkbox("iref", full, mkbox("cdsc", u16(3), u16(1), u16(o.exifFor)))
	}
	// iloc version 1, 4-byte offsets and lengths, no base offset
	iloc := func(exifOff uint32, method uint16) []byte {
		n := uint16(0)
		var entries []byte
		if o.exifOrient != 0 {
			n = 1
			entries = bytes.Join([][]byte{u16(3), u16(method), u16(0), u16(1), u32(exifOff), u32(uint32(len(payload)))}, nil)
		}
		return mkbox("iloc", []byte{1, 0, 0, 0}, []byte{0x44, 0x00}, u16(n), entries)
	}
	ftyp := mkbox("ftyp", []byte("heic"), u32(0), []byte("mif1heic"))
	build := func(exifOff uint32) ([]byte, []byte) {
		method := uint16(0)
		var idat []byte
		if o.inIdat {
			method, exifOff = 1, 0
			idat = mkbox("idat", payload)
		}
		meta := mkbox("meta", full,
			mkbox("hdlr", full, u32(0), []byte("pict"), make([]byte, 13)),
			mkbox("pitm", full, u16(1)), iinf, iref, iloc(exifOff, method),
			mkbox("iprp", mkbox("ipco", props...), mkbox("ipma", ipma)), idat)
		return meta, idat
	}
	meta, _ := build(0)
	// the payload sits in mdat after pad bytes of image data
	exifOff := uint32(len(ftyp) + len(meta) + 8 + o.pad)
	meta, _ = build(exifOff)
	file := append(ftyp, meta...)
	if !o.inIdat {
		file = append(file, mkbox("mdat", make([]byte, o.pad), payload)...)
	}
	return file
}

func TestReadHEIFInfo(t *testing.T) {
	for _, c := range []struct {
		name string
		file []byte
		want heifInfo
	}{
		{"rotation in container and EXIF (current iPhones)", heifFile(heifOpts{w: 4032, h: 3024, rot: 3, exifOrient: 6, exifFor: 1, marker: true}), heifInfo{4032, 3024, true, 6}},
		{"rotation only in EXIF (re-saved files)", heifFile(heifOpts{w: 4032, h: 3024, exifOrient: 6, exifFor: 1, marker: true}), heifInfo{4032, 3024, false, 6}},
		{"EXIF without the Exif marker", heifFile(heifOpts{w: 4032, h: 3024, exifOrient: 8, exifFor: 1}), heifInfo{4032, 3024, false, 8}},
		{"EXIF beyond the first 4 MiB", heifFile(heifOpts{w: 4032, h: 3024, exifOrient: 6, exifFor: 1, pad: 5 << 20}), heifInfo{4032, 3024, false, 6}},
		{"EXIF stored in the meta box", heifFile(heifOpts{w: 4032, h: 3024, exifOrient: 3, exifFor: 1, inIdat: true}), heifInfo{4032, 3024, false, 3}},
		{"EXIF describing only the thumbnail", heifFile(heifOpts{w: 4032, h: 3024, exifOrient: 6, exifFor: 2}), heifInfo{4032, 3024, false, 0}},
		{"lone EXIF item without a reference", heifFile(heifOpts{w: 4032, h: 3024, exifOrient: 6}), heifInfo{4032, 3024, false, 6}},
		{"irot of 0 is no rotation", heifFile(heifOpts{w: 4032, h: 3024, rot: 4, exifOrient: 1, exifFor: 1}), heifInfo{4032, 3024, false, 1}},
		{"no EXIF", heifFile(heifOpts{w: 800, h: 600}), heifInfo{800, 600, false, 0}},
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
		{"EXIF-only 90°, sips applied it", heifInfo{4032, 3024, false, 6}, 3024, 4032, "sips", 1},
		{"EXIF-only 90°, output already transposed", heifInfo{4032, 3024, false, 6}, 3024, 4032, "magick", 1},
		{"EXIF-only 90°, stored size unknown", heifInfo{0, 0, false, 8}, 4032, 3024, "magick", 8},
		{"EXIF-only 180°, libheif ignores EXIF", heifInfo{4032, 3024, false, 3}, 4032, 3024, "vips", 3},
		{"EXIF-only 180°, sips applies EXIF", heifInfo{4032, 3024, false, 3}, 4032, 3024, "sips", 1},
		{"upright", stored, 4032, 3024, "heif-convert", 1},
		{"square, EXIF-only 90°, libheif", heifInfo{3000, 3000, false, 6}, 3000, 3000, "heif-convert", 6},
		{"square, EXIF-only 90°, sips", heifInfo{3000, 3000, false, 6}, 3000, 3000, "sips", 1},
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

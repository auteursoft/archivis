package imageio

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// tiffEntry is one IFD entry for tiffStream: its value bytes are stored
// inline or after the IFD as their size requires.
type tiffEntry struct {
	tag, typ uint16
	val      []byte
}

func asciiVal(s string) []byte { return append([]byte(s), 0) }

func rationalVal(n, d uint32) []byte {
	return binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, n), d)
}

// tiffStream builds a little-endian TIFF stream with one IFD at offset 8,
// as each CR3 CMT box holds.
func tiffStream(entries []tiffEntry) []byte {
	le := binary.LittleEndian
	b := []byte("II*\x00")
	b = le.AppendUint32(b, 8)
	b = le.AppendUint16(b, uint16(len(entries)))
	data := uint32(8 + 2 + 12*len(entries) + 4)
	var extra []byte
	for _, e := range entries {
		b = le.AppendUint16(b, e.tag)
		b = le.AppendUint16(b, e.typ)
		b = le.AppendUint32(b, uint32(len(e.val)/typeSize[e.typ]))
		if len(e.val) <= 4 {
			var inl [4]byte
			copy(inl[:], e.val)
			b = append(b, inl[:]...)
			continue
		}
		b = le.AppendUint32(b, data+uint32(len(extra)))
		extra = append(extra, e.val...)
		if len(extra)%2 == 1 {
			extra = append(extra, 0)
		}
	}
	b = le.AppendUint32(b, 0) // no next IFD
	return append(b, extra...)
}

// cr3File builds a minimal CR3: ftyp "crx ", a moov whose Canon uuid box
// holds the given CMT boxes, and an mdat with a JPEG (which carries no
// EXIF of its own).
func cr3File(cmt map[string][]byte, jpg []byte) []byte {
	var canon []byte
	for _, name := range []string{"CMT1", "CMT2", "CMT3", "CMT4"} {
		if b, ok := cmt[name]; ok {
			canon = append(canon, mkbox(name, b)...)
		}
	}
	return bytes.Join([][]byte{
		mkbox("ftyp", []byte("crx "), u32(1), []byte("crx isom")),
		mkbox("moov", mkbox("uuid", cr3CanonUUID, canon)),
		mkbox("mdat", jpg),
	}, nil)
}

func TestCR3Metadata(t *testing.T) {
	jpg, err := os.ReadFile("testdata/coffee.jpg") // 600x400, no EXIF
	if err != nil {
		t.Fatal(err)
	}
	cmt := map[string][]byte{
		"CMT1": tiffStream([]tiffEntry{
			{tagMake, tASCII, asciiVal("Canon")},
			{tagModel, tASCII, asciiVal("Canon EOS R5")},
			{tagOrientation, tShort, u16le(6)},
			{tagDateTime, tASCII, asciiVal("2021:05:15 13:05:00")},
		}),
		"CMT2": tiffStream([]tiffEntry{
			{tagExposureTime, tRational, rationalVal(1, 250)},
			{tagFNumber, tRational, rationalVal(56, 10)},
			{tagISO, tShort, u16le(400)},
			{tagDateOriginal, tASCII, asciiVal("2021:05:15 13:02:02")},
			{tagOffsetOriginal, tASCII, asciiVal("+02:00")},
			{tagFocalLength, tRational, rationalVal(50, 1)},
		}),
		// the lens only in the maker notes, as in files whose EXIF has none
		"CMT3": tiffStream([]tiffEntry{
			{tagCanonLensModel, tASCII, asciiVal("RF50mm F1.2 L USM")},
		}),
		"CMT4": tiffStream([]tiffEntry{
			{1, tASCII, asciiVal("N")},
			{2, tRational, bytes.Join([][]byte{rationalVal(48, 1), rationalVal(51, 1), rationalVal(0, 1)}, nil)},
			{3, tASCII, asciiVal("W")},
			{4, tRational, bytes.Join([][]byte{rationalVal(2, 1), rationalVal(21, 1), rationalVal(0, 1)}, nil)},
		}),
	}
	p := filepath.Join(t.TempDir(), "IMG_0001.CR3")
	if err := os.WriteFile(p, cr3File(cmt, jpg), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := d.Meta
	if d.Format != "raw:cr3" || m.Make != "Canon" || m.Model != "Canon EOS R5" || m.Lens != "RF50mm F1.2 L USM" {
		t.Fatalf("format %q camera %q %q lens %q", d.Format, m.Make, m.Model, m.Lens)
	}
	if got := m.TakenAt.Format("2006-01-02 15:04:05 -07:00"); got != "2021-05-15 13:02:02 +02:00" {
		t.Errorf("taken %s", got)
	}
	if m.FocalLength != 50 || m.FNumber != 5.6 || m.ISO != 400 || m.ExposureTime != 1.0/250 {
		t.Errorf("exposure %+v", m)
	}
	if !m.HasGPS || m.GPSLat < 48.84 || m.GPSLat > 48.86 || m.GPSLon > -2.34 || m.GPSLon < -2.36 {
		t.Errorf("GPS %v %f %f", m.HasGPS, m.GPSLat, m.GPSLon)
	}
	// orientation 6 from CMT1 turns the 600x400 preview upright
	if m.Orientation != 6 || d.SrcW != 400 || d.SrcH != 600 {
		t.Errorf("orientation %d, %dx%d", m.Orientation, d.SrcW, d.SrcH)
	}

	// Without CMT boxes nothing is read, and a corrupt box is skipped
	// without harm.
	var none Meta
	if parseCR3Meta(bytes.NewReader(cr3File(nil, jpg)), int64(len(cr3File(nil, jpg))), &none) || none.Make != "" {
		t.Errorf("no CMT boxes: %+v", none)
	}
	bad := cr3File(map[string][]byte{"CMT1": []byte("II*\x00\xff\xff\xff\x7f")}, jpg)
	if parseCR3Meta(bytes.NewReader(bad), int64(len(bad)), &none) {
		t.Error("corrupt CMT1 parsed")
	}
}

// zeroTail reads as head followed by zeros, up to size: a large file
// without the memory.
type zeroTail struct {
	head []byte
	size int64
}

func (z zeroTail) ReadAt(p []byte, off int64) (int, error) {
	if off >= z.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), z.size-off))
	clear(p[:n])
	if off < int64(len(z.head)) {
		copy(p[:n], z.head[off:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// A moov box larger than cr3MaxMoov is not read into memory.
func TestCR3MoovBounded(t *testing.T) {
	ftyp := mkbox("ftyp", []byte("crx "), u32(1))
	head := append(ftyp, append(u32(cr3MaxMoov+64), "moov"...)...)
	size := int64(len(ftyp)) + cr3MaxMoov + 64
	if moov := cr3Moov(zeroTail{head, size}, size); moov != nil {
		t.Fatalf("read a moov of %d bytes", len(moov))
	}
	// a small one is read
	head = append(ftyp, append(u32(1024), "moov"...)...)
	if moov := cr3Moov(zeroTail{head, size}, size); len(moov) != 1016 {
		t.Fatalf("moov of %d bytes, want 1016", len(moov))
	}
}

func u16le(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

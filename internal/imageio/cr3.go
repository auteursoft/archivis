package imageio

import (
	"bytes"
	"encoding/binary"
	"io"
)

// Canon CR3 is an ISO-BMFF container. Its metadata is not in the embedded
// JPEGs but in four TIFF streams inside a Canon box in moov:
//
//	moov / uuid 85c0b687-820f-11e0-8111-f4ce462b6a48 /
//	  CMT1  IFD0 (make, model, orientation, date)
//	  CMT2  EXIF IFD (exposure, capture time, lens)
//	  CMT3  Canon maker notes
//	  CMT4  GPS IFD
//
// Each is a complete TIFF stream ("II*\0", IFD at 8) whose offsets are
// relative to its own start.

var cr3CanonUUID = []byte{0x85, 0xc0, 0xb6, 0x87, 0x82, 0x0f, 0x11, 0xe0, 0x81, 0x11, 0xf4, 0xce, 0x46, 0x2b, 0x6a, 0x48}

// cr3MaxMoov bounds the moov box read into memory; real ones are tens of KB.
const cr3MaxMoov = 16 << 20

// cr3MaxBoxes bounds the walk over top-level boxes looking for moov
// (Canon writes it second; each step reads one 16-byte header, so a file
// edited by another tool may put it further on at little cost).
const cr3MaxBoxes = 1024

// parseCR3Meta fills m from the CMT boxes of a CR3 file. It reports whether
// the boxes were found; the caller falls back to an embedded JPEG's EXIF.
func parseCR3Meta(r io.ReaderAt, size int64, m *Meta) bool {
	moov := cr3Moov(r, size)
	if moov == nil {
		return false
	}
	var cmt []box
	for _, b := range childBoxes(moov) {
		if b.typ == "uuid" && len(b.body) >= 16 && bytes.Equal(b.body[:16], cr3CanonUUID) {
			cmt = childBoxes(b.body[16:])
			break
		}
	}
	found := false
	for _, b := range cmt {
		var fill func(*ifd)
		switch b.typ {
		case "CMT1":
			fill = func(d *ifd) { fillPrimary(d, m) }
		case "CMT2":
			fill = func(d *ifd) { fillExif(d, m) }
		case "CMT3":
			fill = func(d *ifd) {
				if m.Lens == "" {
					m.Lens = cleanString(d.str(tagCanonLensModel))
				}
			}
		case "CMT4":
			fill = func(d *ifd) { fillGPS(d, m) }
		default:
			continue
		}
		t, off, err := newTIFFReader(bytes.NewReader(b.body), 0, int64(len(b.body)))
		if err != nil {
			continue
		}
		d, _, err := t.readIFD(off)
		if err != nil {
			continue
		}
		fill(d)
		found = true
	}
	return found
}

// cr3Moov returns the body of the top-level moov box, or nil when there is
// none within the first cr3MaxBoxes boxes or it is implausibly large.
func cr3Moov(r io.ReaderAt, size int64) []byte {
	var hdr [16]byte
	pos := int64(0)
	for i := 0; i < cr3MaxBoxes && pos+8 <= size; i++ {
		if _, err := r.ReadAt(hdr[:], pos); err != nil && pos+16 <= size {
			return nil
		}
		sz := int64(binary.BigEndian.Uint32(hdr[:4]))
		h := int64(8)
		switch sz {
		case 1:
			sz, h = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		case 0:
			sz = size - pos
		}
		if sz < h || sz > size-pos {
			return nil
		}
		if string(hdr[4:8]) == "moov" {
			if sz-h > cr3MaxMoov {
				return nil
			}
			body := make([]byte, sz-h)
			if _, err := r.ReadAt(body, pos+h); err != nil {
				return nil
			}
			return body
		}
		pos += sz
	}
	return nil
}

package imageio

import (
	"bytes"
	"encoding/binary"
	"io"
)

// heifInfo is what Load needs to know about a HEIF file's primary image to
// orient it correctly after an external converter has decoded it.
type heifInfo struct {
	W, H       int  // stored size of the primary image (ispe), before transforms
	Transform  bool // the container rotates or mirrors it (irot/imir)
	ExifOrient int  // EXIF orientation recorded in the file (0 = none)
}

// readHEIFInfo parses the HEIF box structure: the primary item (pitm), its
// properties (ipco, via ipma) and the EXIF block. Converters built on
// libheif apply irot/imir to the pixels but ignore EXIF orientation, as the
// HEIF standard says they should; some files (for instance ones re-saved by
// editing tools) carry their rotation only in EXIF, so it must be checked.
func readHEIFInfo(r io.ReaderAt, size int64) heifInfo {
	var info heifInfo
	// The meta box sits near the start; 4 MiB bounds the read.
	n := min(size, 4<<20)
	buf := make([]byte, n)
	if got, err := r.ReadAt(buf, 0); err != nil && got == 0 {
		return info
	} else {
		buf = buf[:got]
	}
	meta := findBox(buf, "meta")
	if len(meta) < 4 {
		return info
	}
	meta = meta[4:] // full box: version + flags
	primary := uint32(0)
	if p := findBox(meta, "pitm"); len(p) >= 6 {
		if p[0] == 0 {
			primary = uint32(binary.BigEndian.Uint16(p[4:6]))
		} else if len(p) >= 8 {
			primary = binary.BigEndian.Uint32(p[4:8])
		}
	}
	iprp := findBox(meta, "iprp")
	props := childBoxes(findBox(iprp, "ipco"))
	for _, idx := range primaryProperties(findBox(iprp, "ipma"), primary) {
		if idx < 1 || idx > len(props) {
			continue
		}
		p := props[idx-1]
		switch p.typ {
		case "ispe":
			if len(p.body) >= 12 {
				info.W = int(binary.BigEndian.Uint32(p.body[4:8]))
				info.H = int(binary.BigEndian.Uint32(p.body[8:12]))
			}
		case "irot":
			if len(p.body) >= 1 && p.body[0]&3 != 0 {
				info.Transform = true
			}
		case "imir":
			info.Transform = true
		}
	}
	info.ExifOrient = heifExifOrientation(r, buf)
	return info
}

type box struct {
	typ  string
	body []byte
}

// childBoxes splits b into ISO-BMFF boxes (stopping at anything malformed).
func childBoxes(b []byte) []box {
	var out []box
	for len(b) >= 8 {
		sz := uint64(binary.BigEndian.Uint32(b[:4]))
		typ := string(b[4:8])
		hdr := uint64(8)
		if sz == 1 {
			if len(b) < 16 {
				break
			}
			sz, hdr = binary.BigEndian.Uint64(b[8:16]), 16
		} else if sz == 0 {
			sz = uint64(len(b))
		}
		if sz < hdr || sz > uint64(len(b)) {
			break
		}
		out = append(out, box{typ, b[hdr:sz]})
		b = b[sz:]
	}
	return out
}

func findBox(b []byte, typ string) []byte {
	for _, c := range childBoxes(b) {
		if c.typ == typ {
			return c.body
		}
	}
	return nil
}

// primaryProperties returns the 1-based ipco indices associated with item id.
func primaryProperties(ipma []byte, id uint32) []int {
	if len(ipma) < 8 {
		return nil
	}
	version, flags := ipma[0], ipma[3]
	p := ipma[4:]
	count := binary.BigEndian.Uint32(p[:4])
	p = p[4:]
	for i := uint32(0); i < count && len(p) > 0; i++ {
		var item uint32
		if version < 1 {
			if len(p) < 2 {
				return nil
			}
			item, p = uint32(binary.BigEndian.Uint16(p[:2])), p[2:]
		} else {
			if len(p) < 4 {
				return nil
			}
			item, p = binary.BigEndian.Uint32(p[:4]), p[4:]
		}
		if len(p) < 1 {
			return nil
		}
		k := int(p[0])
		p = p[1:]
		var idx []int
		for j := 0; j < k; j++ {
			if flags&1 != 0 {
				if len(p) < 2 {
					return nil
				}
				idx, p = append(idx, int(binary.BigEndian.Uint16(p[:2])&0x7fff)), p[2:]
			} else {
				if len(p) < 1 {
					return nil
				}
				idx, p = append(idx, int(p[0]&0x7f)), p[1:]
			}
		}
		if item == id {
			return idx
		}
	}
	return nil
}

// heifExifOrientation finds the EXIF block ("Exif\0\0" followed by a TIFF
// header) in the first part of the file, where encoders put it, and returns
// its orientation (0 if there is none).
func heifExifOrientation(r io.ReaderAt, head []byte) int {
	scan := func(b []byte, base int64) (int, bool) {
		for i := 0; ; {
			j := bytes.Index(b[i:], []byte("Exif\x00\x00"))
			if j < 0 {
				return 0, false
			}
			t := base + int64(i+j+6)
			if tr, ifd0, err := newTIFFReader(r, t, 0); err == nil {
				if d, _, err := tr.readIFD(ifd0); err == nil {
					o, _ := d.uint(tagOrientation)
					return int(o), true
				}
			}
			i += j + 6
		}
	}
	o, _ := scan(head, 0)
	return o
}

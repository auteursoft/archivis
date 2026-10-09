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
// properties (ipco, via ipma) and the EXIF item that describes it. Converters built on
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
	info.ExifOrient = heifExifOrientation(r, size, meta, primary)
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

// heifExifOrientation returns the orientation in the EXIF item that
// describes the primary image (0 if there is none). The item is found the
// way the HEIF standard defines: its type in iinf, its "content describes"
// reference (cdsc) to the primary item in iref, and its bytes through iloc,
// wherever in the file they are. An EXIF item's payload starts with the
// 32-bit offset of the TIFF header that follows it.
func heifExifOrientation(r io.ReaderAt, size int64, meta []byte, primary uint32) int {
	types := heifItemTypes(findBox(meta, "iinf"))
	var exif []uint32
	for id, typ := range types {
		if typ == "Exif" {
			exif = append(exif, id)
		}
	}
	if len(exif) == 0 {
		return 0
	}
	refs := heifRefs(findBox(meta, "iref"), "cdsc")
	pick := uint32(0)
	for _, id := range exif {
		for _, to := range refs[id] {
			if to == primary {
				pick = id
			}
		}
	}
	if pick == 0 && len(exif) == 1 && len(refs[exif[0]]) == 0 {
		pick = exif[0] // a lone EXIF item that describes nothing in particular
	}
	if pick == 0 {
		return 0
	}
	data := heifItemData(r, size, meta, pick, 1<<20)
	if len(data) < 4 {
		return 0
	}
	off := int64(binary.BigEndian.Uint32(data[:4])) + 4
	if off >= int64(len(data)) {
		return 0
	}
	tr, ifd0, err := newTIFFReader(bytes.NewReader(data), off, int64(len(data)))
	if err != nil {
		return 0
	}
	d, _, err := tr.readIFD(ifd0)
	if err != nil {
		return 0
	}
	o, _ := d.uint(tagOrientation)
	return int(o)
}

// heifItemTypes maps item IDs to their types (iinf: infe boxes, version 2
// or later).
func heifItemTypes(iinf []byte) map[uint32]string {
	out := map[uint32]string{}
	if len(iinf) < 6 {
		return out
	}
	body := iinf[6:] // version, flags, 16-bit entry count
	if iinf[0] != 0 {
		if len(iinf) < 8 {
			return out
		}
		body = iinf[8:] // 32-bit entry count
	}
	for _, e := range childBoxes(body) {
		b := e.body
		if e.typ != "infe" || len(b) < 4 {
			continue
		}
		switch v := b[0]; {
		case v == 2 && len(b) >= 12:
			out[uint32(binary.BigEndian.Uint16(b[4:6]))] = string(b[8:12])
		case v >= 3 && len(b) >= 14:
			out[binary.BigEndian.Uint32(b[4:8])] = string(b[10:14])
		}
	}
	return out
}

// heifRefs returns the references of one type from iref: from ID -> to IDs.
func heifRefs(iref []byte, typ string) map[uint32][]uint32 {
	out := map[uint32][]uint32{}
	if len(iref) < 4 {
		return out
	}
	wide := iref[0] != 0 // version 1: 32-bit item IDs
	id := func(p []byte) (uint32, []byte, bool) {
		if wide {
			if len(p) < 4 {
				return 0, nil, false
			}
			return binary.BigEndian.Uint32(p[:4]), p[4:], true
		}
		if len(p) < 2 {
			return 0, nil, false
		}
		return uint32(binary.BigEndian.Uint16(p[:2])), p[2:], true
	}
	for _, ref := range childBoxes(iref[4:]) {
		if ref.typ != typ {
			continue
		}
		from, p, ok := id(ref.body)
		if !ok || len(p) < 2 {
			continue
		}
		n := int(binary.BigEndian.Uint16(p[:2]))
		p = p[2:]
		for i := 0; i < n; i++ {
			var to uint32
			if to, p, ok = id(p); !ok {
				break
			}
			out[from] = append(out[from], to)
		}
	}
	return out
}

// heifItemData reads an item's bytes (at most limit) as located by iloc:
// from the file (construction method 0) or from the meta box's idat (1).
func heifItemData(r io.ReaderAt, size int64, meta []byte, item uint32, limit int) []byte {
	b := findBox(meta, "iloc")
	if len(b) < 8 {
		return nil
	}
	version := b[0]
	offSize, lenSize := int(b[4]>>4), int(b[4]&15)
	baseSize, idxSize := int(b[5]>>4), 0
	if version == 1 || version == 2 {
		idxSize = int(b[5] & 15)
	}
	p := b[6:]
	take := func(n int) (uint64, bool) {
		if n == 0 {
			return 0, true
		}
		if len(p) < n || (n != 4 && n != 8 && n != 2) {
			return 0, false
		}
		var v uint64
		for _, c := range p[:n] {
			v = v<<8 | uint64(c)
		}
		p = p[n:]
		return v, true
	}
	countSize := 2
	if version == 2 {
		countSize = 4
	}
	count, ok := take(countSize)
	if !ok {
		return nil
	}
	for i := uint64(0); i < count; i++ {
		idSize := 2
		if version == 2 {
			idSize = 4
		}
		id, ok1 := take(idSize)
		method := uint64(0)
		if version == 1 || version == 2 {
			m, ok := take(2)
			if !ok {
				return nil
			}
			method = m & 15
		}
		_, ok2 := take(2) // data reference index
		base, ok3 := take(baseSize)
		extents, ok4 := take(2)
		if !(ok1 && ok2 && ok3 && ok4) {
			return nil
		}
		var out []byte
		for e := uint64(0); e < extents; e++ {
			if idxSize > 0 {
				if _, ok := take(idxSize); !ok {
					return nil
				}
			}
			off, okO := take(offSize)
			n, okN := take(lenSize)
			if !okO || !okN {
				return nil
			}
			if uint32(id) != item {
				continue
			}
			start := base + off
			if n == 0 || n > uint64(limit-len(out)) {
				n = uint64(limit - len(out))
			}
			switch method {
			case 0:
				if int64(start) >= size {
					return nil
				}
				n = min(n, uint64(size)-start)
				chunk := make([]byte, n)
				got, _ := r.ReadAt(chunk, int64(start))
				out = append(out, chunk[:got]...)
			case 1:
				idat := findBox(meta, "idat")
				if start >= uint64(len(idat)) {
					return nil
				}
				out = append(out, idat[start:min(uint64(len(idat)), start+n)]...)
			default:
				return nil // item-offset construction: not used for EXIF
			}
		}
		if uint32(id) == item {
			return out
		}
	}
	return nil
}

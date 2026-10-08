package imageio

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// Minimal TIFF/IFD reader. It is used both for EXIF blocks (inside JPEG APP1)
// and for TIFF-based RAW containers (CR2, NEF, ARW, DNG, PEF, ORF, RW2, ...)
// from which we pull metadata and embedded JPEG previews.

const (
	tByte      = 1
	tASCII     = 2
	tShort     = 3
	tLong      = 4
	tRational  = 5
	tSByte     = 6
	tUndefined = 7
	tSShort    = 8
	tSLong     = 9
	tSRational = 10
	tFloat     = 11
	tDouble    = 12
	tIFD       = 13
)

var typeSize = map[uint16]int{
	tByte: 1, tASCII: 1, tShort: 2, tLong: 4, tRational: 8, tSByte: 1,
	tUndefined: 1, tSShort: 2, tSLong: 4, tSRational: 8, tFloat: 4, tDouble: 8, tIFD: 4,
}

type tiffReader struct {
	r    io.ReaderAt
	bo   binary.ByteOrder
	base int64 // offset of the TIFF header within r
	size int64 // size of r (0 = unknown)
}

type ifdEntry struct {
	Tag   uint16
	Type  uint16
	Count uint32
	inl   [4]byte // inline value / offset bytes
}

type ifd struct {
	t       *tiffReader
	entries map[uint16]ifdEntry
}

var errNotTIFF = errors.New("not a TIFF stream")

// newTIFFReader parses the 8-byte header at base and returns the reader and
// the offset of IFD0. ORF ("IIRO"/"MMOR") and RW2 ("IIU\0") magics are accepted.
func newTIFFReader(r io.ReaderAt, base, size int64) (*tiffReader, uint32, error) {
	var hdr [8]byte
	if _, err := r.ReadAt(hdr[:], base); err != nil {
		return nil, 0, err
	}
	var bo binary.ByteOrder
	switch string(hdr[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return nil, 0, errNotTIFF
	}
	magic := bo.Uint16(hdr[2:4])
	if magic != 42 && magic != 0x4F52 && magic != 0x5352 && magic != 0x55 {
		return nil, 0, errNotTIFF
	}
	return &tiffReader{r: r, bo: bo, base: base, size: size}, bo.Uint32(hdr[4:8]), nil
}

func (t *tiffReader) readIFD(off uint32) (*ifd, uint32, error) {
	if off == 0 || (t.size > 0 && int64(off)+t.base+2 > t.size) {
		return nil, 0, errors.New("bad IFD offset")
	}
	var nb [2]byte
	if _, err := t.r.ReadAt(nb[:], t.base+int64(off)); err != nil {
		return nil, 0, err
	}
	n := int(t.bo.Uint16(nb[:]))
	if n == 0 || n > 1000 {
		return nil, 0, errors.New("implausible IFD entry count")
	}
	buf := make([]byte, n*12+4)
	got, err := t.r.ReadAt(buf, t.base+int64(off)+2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	// Every entry must be present; only the trailing next-IFD pointer may be
	// cut off (treated as "no next IFD").
	if got < n*12 {
		return nil, 0, errors.New("truncated IFD")
	}
	if got < len(buf) {
		clear(buf[n*12:])
	}
	d := &ifd{t: t, entries: make(map[uint16]ifdEntry, n)}
	for i := 0; i < n; i++ {
		e := buf[i*12 : i*12+12]
		var en ifdEntry
		en.Tag = t.bo.Uint16(e[0:2])
		en.Type = t.bo.Uint16(e[2:4])
		en.Count = t.bo.Uint32(e[4:8])
		copy(en.inl[:], e[8:12])
		d.entries[en.Tag] = en
	}
	next := t.bo.Uint32(buf[n*12:])
	return d, next, nil
}

// raw returns the raw bytes of the entry value (bounded to 1 MiB for safety).
func (d *ifd) raw(tag uint16) ([]byte, uint16, bool) {
	e, ok := d.entries[tag]
	if !ok {
		return nil, 0, false
	}
	sz, ok := typeSize[e.Type]
	if !ok {
		return nil, 0, false
	}
	total := int64(sz) * int64(e.Count)
	if total <= 4 {
		return e.inl[:total], e.Type, true
	}
	if total > 1<<20 {
		return nil, 0, false
	}
	off := int64(d.t.bo.Uint32(e.inl[:]))
	buf := make([]byte, total)
	if _, err := d.t.r.ReadAt(buf, d.t.base+off); err != nil {
		return nil, 0, false
	}
	return buf, e.Type, true
}

// uints returns integer values of BYTE/SHORT/LONG/IFD entries.
func (d *ifd) uints(tag uint16) []uint32 {
	b, typ, ok := d.raw(tag)
	if !ok {
		return nil
	}
	bo := d.t.bo
	var out []uint32
	switch typ {
	case tByte, tUndefined:
		for _, v := range b {
			out = append(out, uint32(v))
		}
	case tShort, tSShort:
		for i := 0; i+2 <= len(b); i += 2 {
			out = append(out, uint32(bo.Uint16(b[i:])))
		}
	case tLong, tSLong, tIFD:
		for i := 0; i+4 <= len(b); i += 4 {
			out = append(out, bo.Uint32(b[i:]))
		}
	}
	return out
}

func (d *ifd) uint(tag uint16) (uint32, bool) {
	v := d.uints(tag)
	if len(v) == 0 {
		return 0, false
	}
	return v[0], true
}

// floats returns values of numeric entries as float64 (rationals divided out).
func (d *ifd) floats(tag uint16) []float64 {
	b, typ, ok := d.raw(tag)
	if !ok {
		return nil
	}
	bo := d.t.bo
	var out []float64
	switch typ {
	case tRational:
		for i := 0; i+8 <= len(b); i += 8 {
			n, den := bo.Uint32(b[i:]), bo.Uint32(b[i+4:])
			if den == 0 {
				out = append(out, math.NaN())
			} else {
				out = append(out, float64(n)/float64(den))
			}
		}
	case tSRational:
		for i := 0; i+8 <= len(b); i += 8 {
			n, den := int32(bo.Uint32(b[i:])), int32(bo.Uint32(b[i+4:]))
			if den == 0 {
				out = append(out, math.NaN())
			} else {
				out = append(out, float64(n)/float64(den))
			}
		}
	case tFloat:
		for i := 0; i+4 <= len(b); i += 4 {
			out = append(out, float64(math.Float32frombits(bo.Uint32(b[i:]))))
		}
	case tDouble:
		for i := 0; i+8 <= len(b); i += 8 {
			out = append(out, math.Float64frombits(bo.Uint64(b[i:])))
		}
	case tSShort:
		for i := 0; i+2 <= len(b); i += 2 {
			out = append(out, float64(int16(bo.Uint16(b[i:]))))
		}
	case tSLong:
		for i := 0; i+4 <= len(b); i += 4 {
			out = append(out, float64(int32(bo.Uint32(b[i:]))))
		}
	default:
		for _, v := range d.uints(tag) {
			out = append(out, float64(v))
		}
	}
	return out
}

func (d *ifd) float(tag uint16) (float64, bool) {
	v := d.floats(tag)
	if len(v) == 0 || math.IsNaN(v[0]) || math.IsInf(v[0], 0) {
		return 0, false
	}
	return v[0], true
}

func (d *ifd) str(tag uint16) string {
	b, typ, ok := d.raw(tag)
	if !ok || (typ != tASCII && typ != tUndefined && typ != tByte) {
		return ""
	}
	for i, c := range b {
		if c == 0 {
			b = b[:i]
			break
		}
	}
	return cleanString(string(b))
}

// offsetOf returns the absolute file offset of an entry's out-of-line data.
func (d *ifd) dataOffset(tag uint16) (int64, int64, bool) {
	e, ok := d.entries[tag]
	if !ok {
		return 0, 0, false
	}
	sz := typeSize[e.Type]
	total := int64(sz) * int64(e.Count)
	if total <= 4 {
		return 0, 0, false
	}
	return d.t.base + int64(d.t.bo.Uint32(e.inl[:])), total, true
}

func cleanString(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == 0xFFFD || (r < 32 && r != '\t') {
			continue
		}
		out = append(out, r)
	}
	// trim spaces
	i, j := 0, len(out)
	for i < j && (out[i] == ' ' || out[i] == '\t') {
		i++
	}
	for j > i && (out[j-1] == ' ' || out[j-1] == '\t') {
		j--
	}
	return string(out[i:j])
}

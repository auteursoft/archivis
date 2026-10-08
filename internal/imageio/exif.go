package imageio

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// Meta holds the photographic metadata we care about.
type Meta struct {
	Make         string
	Model        string
	Lens         string
	Orientation  int // EXIF orientation 1..8 (0 = unknown)
	TakenAt      time.Time
	FocalLength  float64 // mm
	Focal35      float64 // 35mm-equivalent focal length
	FNumber      float64
	ExposureTime float64 // seconds
	ISO          int
	ExposureBias float64
	Flash        bool
	GPSLat       float64
	GPSLon       float64
	HasGPS       bool
	Artist       string
	Copyright    string
	Description  string
	Width        int // pixel dimensions as recorded (may be raw sensor size)
	Height       int
}

const (
	tagImageWidth      = 0x0100
	tagImageLength     = 0x0101
	tagCompression     = 0x0103
	tagDescription     = 0x010E
	tagMake            = 0x010F
	tagModel           = 0x0110
	tagStripOffsets    = 0x0111
	tagOrientation     = 0x0112
	tagStripByteCounts = 0x0117
	tagDateTime        = 0x0132
	tagArtist          = 0x013B
	tagSubIFDs         = 0x014A
	tagJPEGOffset      = 0x0201
	tagJPEGLength      = 0x0202
	tagCopyright       = 0x8298
	tagExposureTime    = 0x829A
	tagFNumber         = 0x829D
	tagExifIFD         = 0x8769
	tagGPSIFD          = 0x8825
	tagISO             = 0x8827
	tagDateOriginal    = 0x9003
	tagOffsetOriginal  = 0x9011
	tagExposureBias    = 0x9204
	tagFlash           = 0x9209
	tagFocalLength     = 0x920A
	tagSubSecOriginal  = 0x9291
	tagPixelX          = 0xA002
	tagPixelY          = 0xA003
	tagFocal35         = 0xA405
	tagLensMake        = 0xA433
	tagLensModel       = 0xA434
	tagRW2JpgFromRaw   = 0x002E
)

// jpegCandidate is an embedded JPEG stream found inside a container.
type jpegCandidate struct {
	off, size int64
}

// parseTIFFMeta walks IFD0 (+chain), EXIF, GPS and SubIFDs, filling meta and
// collecting embedded JPEG candidates.
func parseTIFFMeta(t *tiffReader, ifd0 uint32, m *Meta) []jpegCandidate {
	var cands []jpegCandidate
	seen := map[uint32]bool{}
	var walk func(off uint32, depth int, primary bool)
	walk = func(off uint32, depth int, primary bool) {
		for off != 0 && depth < 8 && !seen[off] {
			seen[off] = true
			d, next, err := t.readIFD(off)
			if err != nil {
				return
			}
			if primary {
				fillPrimary(d, m)
				if eo, ok := d.uint(tagExifIFD); ok {
					if e, _, err := t.readIFD(eo); err == nil {
						fillExif(e, m)
						if m.Lens == "" {
							m.Lens = makerNoteLens(e, m.Make)
						}
					}
				}
				if g, ok := d.uint(tagGPSIFD); ok {
					if e, _, err := t.readIFD(g); err == nil {
						fillGPS(e, m)
					}
				}
			}
			cands = append(cands, ifdJPEGs(d)...)
			for _, s := range d.uints(tagSubIFDs) {
				walk(s, depth+1, false)
			}
			primary = false
			off = next
			depth++
		}
	}
	walk(ifd0, 0, true)
	return cands
}

func ifdJPEGs(d *ifd) []jpegCandidate {
	var out []jpegCandidate
	if o, ok := d.uint(tagJPEGOffset); ok {
		if l, ok := d.uint(tagJPEGLength); ok && l > 0 {
			out = append(out, jpegCandidate{d.t.base + int64(o), int64(l)})
		}
	}
	if c, ok := d.uint(tagCompression); ok && (c == 6 || c == 7 || c == 99) {
		offs, lens := d.uints(tagStripOffsets), d.uints(tagStripByteCounts)
		if len(offs) == 1 && len(lens) == 1 && lens[0] > 0 {
			out = append(out, jpegCandidate{d.t.base + int64(offs[0]), int64(lens[0])})
		}
	}
	// Panasonic RW2 stores its full-size JPEG as an UNDEFINED blob.
	if o, l, ok := d.dataOffset(tagRW2JpgFromRaw); ok {
		out = append(out, jpegCandidate{o, l})
	}
	return out
}

func fillPrimary(d *ifd, m *Meta) {
	if s := d.str(tagMake); s != "" {
		m.Make = s
	}
	if s := d.str(tagModel); s != "" {
		m.Model = s
	}
	if v, ok := d.uint(tagOrientation); ok && v >= 1 && v <= 8 {
		m.Orientation = int(v)
	}
	if s := d.str(tagArtist); s != "" {
		m.Artist = s
	}
	if s := d.str(tagCopyright); s != "" {
		m.Copyright = s
	}
	if s := d.str(tagDescription); s != "" {
		m.Description = s
	}
	if m.TakenAt.IsZero() {
		m.TakenAt = parseExifTime(d.str(tagDateTime), "", "")
	}
	if w, ok := d.uint(tagImageWidth); ok && m.Width == 0 {
		if h, ok := d.uint(tagImageLength); ok {
			m.Width, m.Height = int(w), int(h)
		}
	}
}

func fillExif(d *ifd, m *Meta) {
	if v, ok := d.float(tagExposureTime); ok {
		m.ExposureTime = v
	}
	if v, ok := d.float(tagFNumber); ok {
		m.FNumber = v
	}
	if v, ok := d.uint(tagISO); ok {
		m.ISO = int(v)
	}
	if v, ok := d.float(tagFocalLength); ok {
		m.FocalLength = v
	}
	if v, ok := d.uint(tagFocal35); ok {
		m.Focal35 = float64(v)
	}
	if v, ok := d.float(tagExposureBias); ok {
		m.ExposureBias = v
	}
	if v, ok := d.uint(tagFlash); ok {
		m.Flash = v&1 == 1
	}
	if t := parseExifTime(d.str(tagDateOriginal), d.str(tagSubSecOriginal), d.str(tagOffsetOriginal)); !t.IsZero() {
		m.TakenAt = t
	}
	lens := d.str(tagLensModel)
	if mk := d.str(tagLensMake); mk != "" && lens != "" && !strings.HasPrefix(strings.ToLower(lens), strings.ToLower(mk)) {
		lens = mk + " " + lens
	}
	if lens != "" {
		m.Lens = lens
	}
	if w, ok := d.uint(tagPixelX); ok {
		if h, ok := d.uint(tagPixelY); ok && w > 0 && h > 0 {
			m.Width, m.Height = int(w), int(h)
		}
	}
}

func fillGPS(d *ifd, m *Meta) {
	lat, lon := d.floats(2), d.floats(4)
	if len(lat) != 3 || len(lon) != 3 {
		return
	}
	la := lat[0] + lat[1]/60 + lat[2]/3600
	lo := lon[0] + lon[1]/60 + lon[2]/3600
	if math.IsNaN(la) || math.IsNaN(lo) || (la == 0 && lo == 0) {
		return
	}
	if strings.HasPrefix(d.str(1), "S") {
		la = -la
	}
	if strings.HasPrefix(d.str(3), "W") {
		lo = -lo
	}
	if math.Abs(la) > 90 || math.Abs(lo) > 180 {
		return
	}
	m.GPSLat, m.GPSLon, m.HasGPS = la, lo, true
}

func parseExifTime(s, subsec, offset string) time.Time {
	s = strings.TrimSpace(s)
	if len(s) < 19 || strings.HasPrefix(s, "0000") {
		return time.Time{}
	}
	loc := time.UTC // EXIF times without offset are "local wall clock"; stored as-is.
	if len(offset) == 6 && (offset[0] == '+' || offset[0] == '-') {
		if t, err := time.Parse("-07:00", offset); err == nil {
			loc = t.Location()
		}
	}
	t, err := time.ParseInLocation("2006:01:02 15:04:05", s[:19], loc)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02 15:04:05", s[:19], loc)
		if err != nil {
			return time.Time{}
		}
	}
	if subsec != "" {
		ns := 0
		scale := int(time.Second / 10)
		for _, c := range subsec {
			if c < '0' || c > '9' || scale == 0 {
				break
			}
			ns += int(c-'0') * scale
			scale /= 10
		}
		t = t.Add(time.Duration(ns))
	}
	return t
}

// parseJPEGExif looks for an APP1 Exif segment in a JPEG stream starting at off
// and parses it into m.
func parseJPEGExif(r io.ReaderAt, off int64, m *Meta) {
	pos := off + 2
	var hdr [4]byte
	for i := 0; i < 32; i++ {
		if _, err := r.ReadAt(hdr[:], pos); err != nil {
			return
		}
		if hdr[0] != 0xFF {
			return
		}
		marker := hdr[1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			pos += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return
		}
		ln := int64(binary.BigEndian.Uint16(hdr[2:4]))
		if marker == 0xE1 && ln > 8 {
			var id [6]byte
			if _, err := r.ReadAt(id[:], pos+4); err == nil && bytes.Equal(id[:], []byte("Exif\x00\x00")) {
				base := pos + 10
				size := ln - 8
				sec := io.NewSectionReader(r, base, size)
				if t, ifd0, err := newTIFFReader(sec, 0, size); err == nil {
					parseTIFFMeta(t, ifd0, m)
				}
				return
			}
		}
		pos += 2 + ln
	}
}

const (
	tagMakerNote      = 0x927C
	tagCanonLensModel = 0x0095 // Canon maker note: lens name
	tagNikonLens      = 0x0084 // Nikon maker note: focal and aperture range
)

// makerNoteLens reads the lens from the camera maker's private notes, for
// cameras that predate the EXIF LensModel tag (most before ~2012). Canon
// records the lens name; Nikon only its focal and aperture range, which is
// formatted the way Nikon names lenses ("24-70mm f/2.8"). Other makers'
// notes are not read.
func makerNoteLens(exif *ifd, make string) string {
	e, ok := exif.entries[tagMakerNote]
	if !ok || e.Count < 16 {
		return ""
	}
	t := exif.t
	off := t.bo.Uint32(e.inl[:])
	mk := strings.ToUpper(make)
	switch {
	case strings.HasPrefix(mk, "CANON"):
		// a plain IFD; offsets are relative to the enclosing TIFF header
		d, _, err := t.readIFD(off)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(d.str(tagCanonLensModel))
	case strings.HasPrefix(mk, "NIKON"):
		// "Nikon\0" + version (4 bytes), then a TIFF header of its own
		var hdr [6]byte
		if _, err := t.r.ReadAt(hdr[:], t.base+int64(off)); err != nil || string(hdr[:]) != "Nikon\x00" {
			return ""
		}
		nt, ifd0, err := newTIFFReader(t.r, t.base+int64(off)+10, t.size)
		if err != nil {
			return ""
		}
		d, _, err := nt.readIFD(ifd0)
		if err != nil {
			return ""
		}
		return nikonLensName(d.floats(tagNikonLens))
	}
	return ""
}

// nikonLensName formats Nikon's lens tag {min focal, max focal, f-number at
// min focal, f-number at max focal}: "24-70mm f/2.8", "18-55mm f/3.5-5.6",
// "50mm f/1.4".
func nikonLensName(v []float64) string {
	if len(v) != 4 || !(v[0] > 0) || !(v[1] >= v[0]) || !(v[2] > 0) || v[1] > 5000 || v[2] > 64 {
		return ""
	}
	n := strconv.FormatFloat(v[0], 'f', -1, 64)
	if v[1] > v[0] {
		n += "-" + strconv.FormatFloat(v[1], 'f', -1, 64)
	}
	n += "mm f/" + strconv.FormatFloat(v[2], 'f', -1, 64)
	if v[3] > v[2] {
		n += "-" + strconv.FormatFloat(v[3], 'f', -1, 64)
	}
	return n
}

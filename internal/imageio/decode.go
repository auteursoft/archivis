package imageio

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// ErrUnsupported is returned for files we recognise but cannot decode.
var ErrUnsupported = errors.New("unsupported image format")

// Extensions the indexer will consider.
var (
	rasterExts = map[string]bool{
		".jpg": true, ".jpeg": true, ".jpe": true, ".jfif": true, ".png": true,
		".gif": true, ".webp": true, ".bmp": true, ".tif": true, ".tiff": true,
	}
	heifExts = map[string]bool{".heic": true, ".heif": true, ".hif": true, ".avif": true}
	rawExts  = map[string]bool{
		".cr2": true, ".cr3": true, ".crw": true, ".nef": true, ".nrw": true,
		".arw": true, ".srf": true, ".sr2": true, ".dng": true, ".raf": true,
		".orf": true, ".rw2": true, ".rwl": true, ".pef": true, ".srw": true,
		".3fr": true, ".iiq": true, ".erf": true, ".kdc": true, ".mos": true,
		".mrw": true, ".x3f": true,
	}
)

// IsImagePath reports whether the path has an extension we try to index.
func IsImagePath(p string) bool {
	e := strings.ToLower(filepath.Ext(p))
	return rasterExts[e] || rawExts[e] || heifExts[e]
}

// IsRawPath reports whether the path looks like a camera RAW file.
func IsRawPath(p string) bool { return rawExts[strings.ToLower(filepath.Ext(p))] }

// MaxPixels caps the decoded size of a single image. Image headers are read
// before any pixels are decoded, so a corrupt or malicious file claiming huge
// dimensions (a "decompression bomb": a few MB that would need many GB of
// RAM) is rejected instead of exhausting memory. 300 MP covers medium-format
// backs and most stitched panoramas.
var MaxPixels = 300_000_000

// ErrTooLarge is returned for images over MaxPixels.
var ErrTooLarge = errors.New("image dimensions exceed the decode limit")

func checkPixels(w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("invalid image dimensions %dx%d", w, h)
	}
	if int64(w)*int64(h) > int64(MaxPixels) {
		return fmt.Errorf("%w (%dx%d = %d MP; limit %d MP)", ErrTooLarge, w, h, int64(w)*int64(h)/1_000_000, MaxPixels/1_000_000)
	}
	return nil
}

// checkConfig reads only the image header to validate dimensions.
func checkConfig(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil // let the real decoder report the format error
	}
	return checkPixels(cfg.Width, cfg.Height)
}

// Decoded is the result of loading a photo.
type Decoded struct {
	Img    *RGB // oriented image, downscaled so the long edge <= maxDim
	Meta   Meta
	Format string // "jpeg", "png", "raw:nef", "heic", ...
	// Width/Height of the decoded source after orientation, before downscaling.
	SrcW, SrcH int
	// Blockiness of JPEG sources at native resolution (1 = none); see Blockiness.
	Blockiness float64
}

// Load decodes a photo from disk, applying EXIF orientation. For RAW files the
// largest embedded JPEG preview is used. maxDim <= 0 disables downscaling.
func Load(path string, maxDim int) (*Decoded, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size < 16 {
		return nil, fmt.Errorf("file too small (%d bytes)", size)
	}
	var head [32]byte
	if _, err := f.ReadAt(head[:], 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	d := &Decoded{}

	var img image.Image
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		// Plain JPEG: read whole file in one go (best for spinning disks).
		data, err := readAllN(f, size)
		if err != nil {
			return nil, err
		}
		if err := checkConfig(data); err != nil {
			return nil, err
		}
		r := bytes.NewReader(data)
		parseJPEGExif(r, 0, &d.Meta)
		img, err = jpeg.Decode(r)
		if err != nil {
			return nil, fmt.Errorf("jpeg: %w", err)
		}
		d.Format = "jpeg"

	case string(head[:15]) == "FUJIFILMCCD-RAW":
		img, err = loadRAF(f, size, &d.Meta)
		if err != nil {
			return nil, err
		}
		d.Format = "raw:raf"

	case string(head[4:8]) == "ftyp":
		brand := string(head[8:12])
		if brand == "crx " {
			img, err = scanEmbeddedJPEG(f, size, &d.Meta)
			d.Format = "raw:cr3"
		} else {
			img, err = loadExternal(path, &d.Meta)
			d.Format = "heif"
		}
		if err != nil {
			return nil, err
		}

	case string(head[:2]) == "II" || string(head[:2]) == "MM":
		t, ifd0, terr := newTIFFReader(f, 0, size)
		if terr != nil {
			return nil, terr
		}
		cands := parseTIFFMeta(t, ifd0, &d.Meta)
		if rawExts[ext] || len(cands) > 0 && !rasterExts[ext] {
			img, err = bestJPEG(f, size, cands)
			if err != nil {
				img, err = scanEmbeddedJPEG(f, size, nil)
			}
			if err != nil {
				return nil, fmt.Errorf("raw preview: %w", err)
			}
			d.Format = "raw:" + strings.TrimPrefix(ext, ".")
		} else {
			data, rerr := readAllN(f, size)
			if rerr != nil {
				return nil, rerr
			}
			if err := checkConfig(data); err != nil {
				return nil, err
			}
			img, _, err = image.Decode(bytes.NewReader(data))
			if err != nil {
				// Some TIFF variants (16-bit, odd compression) fail; try a preview.
				if img2, err2 := bestJPEG(f, size, cands); err2 == nil {
					img, err = img2, nil
				} else {
					return nil, fmt.Errorf("tiff: %w", err)
				}
			}
			d.Format = "tiff"
		}

	default:
		if heifExts[ext] {
			img, err = loadExternal(path, &d.Meta)
			if err != nil {
				return nil, err
			}
			d.Format = "heif"
			break
		}
		if rawExts[ext] {
			img, err = scanEmbeddedJPEG(f, size, &d.Meta)
			if err != nil {
				return nil, err
			}
			d.Format = "raw:" + strings.TrimPrefix(ext, ".")
			break
		}
		data, rerr := readAllN(f, size)
		if rerr != nil {
			return nil, rerr
		}
		if err := checkConfig(data); err != nil {
			return nil, err
		}
		var format string
		img, format, err = image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		d.Format = format
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 8 || h < 8 {
		return nil, fmt.Errorf("image too small (%dx%d)", w, h)
	}
	d.Blockiness = 1
	if strings.HasPrefix(d.Format, "jpeg") || strings.HasPrefix(d.Format, "raw") || d.Format == "heif" {
		d.Blockiness = blockinessOf(img)
	}
	// Downscale before orienting (cheaper); orientation transposes dims.
	rgb := ToRGB(img, maxDim)
	img = nil
	rgb = Orient(rgb, d.Meta.Orientation)
	d.Img = rgb
	if d.Meta.Orientation >= 5 {
		w, h = h, w
	}
	d.SrcW, d.SrcH = w, h
	return d, nil
}

func readAllN(f *os.File, size int64) ([]byte, error) {
	if size > 1<<30 {
		return nil, fmt.Errorf("file too large (%d bytes)", size)
	}
	buf := make([]byte, size)
	_, err := io.ReadFull(io.NewSectionReader(f, 0, size), buf)
	return buf, err
}

// jpegInfo inspects the JPEG stream at off and returns its dimensions if it is
// a baseline/progressive (decodable) JPEG.
func jpegInfo(r io.ReaderAt, off, size int64) (w, h int, ok bool) {
	var b [2]byte
	if _, err := r.ReadAt(b[:], off); err != nil || b[0] != 0xFF || b[1] != 0xD8 {
		return 0, 0, false
	}
	pos := off + 2
	end := off + size
	var hdr [9]byte
	for i := 0; i < 64 && pos+4 <= end; i++ {
		if _, err := r.ReadAt(hdr[:4], pos); err != nil {
			return 0, 0, false
		}
		if hdr[0] != 0xFF {
			return 0, 0, false
		}
		m := hdr[1]
		if m == 0xFF { // fill byte
			pos++
			continue
		}
		ln := int64(binary.BigEndian.Uint16(hdr[2:4]))
		if ln < 2 {
			return 0, 0, false
		}
		switch m {
		case 0xC0, 0xC1, 0xC2:
			if _, err := r.ReadAt(hdr[:9], pos); err != nil {
				return 0, 0, false
			}
			h = int(binary.BigEndian.Uint16(hdr[5:7]))
			w = int(binary.BigEndian.Uint16(hdr[7:9]))
			return w, h, w > 0 && h > 0
		case 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF:
			return 0, 0, false // lossless / arithmetic: not decodable by image/jpeg
		case 0xDA, 0xD9:
			return 0, 0, false
		}
		pos += 2 + ln
	}
	return 0, 0, false
}

// bestJPEG decodes the largest valid embedded JPEG among cands. Offsets and
// lengths come from the (untrusted) file, so each candidate is clamped to the
// real file size and decoded directly from that region, never copied into a
// buffer sized by the file's own claims.
func bestJPEG(r io.ReaderAt, srcSize int64, cands []jpegCandidate) (image.Image, error) {
	type scored struct {
		c    jpegCandidate
		area int
	}
	var list []scored
	for _, c := range cands {
		if c.off < 0 || c.size <= 0 || c.off >= srcSize {
			continue
		}
		c.size = min(c.size, srcSize-c.off)
		if w, h, ok := jpegInfo(r, c.off, c.size); ok && checkPixels(w, h) == nil {
			list = append(list, scored{c, w * h})
		}
	}
	// Try largest first; fall back to smaller ones if decoding fails.
	for len(list) > 0 {
		bi := 0
		for i := range list {
			if list[i].area > list[bi].area {
				bi = i
			}
		}
		c := list[bi].c
		list = append(list[:bi], list[bi+1:]...)
		sec := bufio.NewReaderSize(io.NewSectionReader(r, c.off, c.size), 1<<20)
		if img, err := jpeg.Decode(sec); err == nil {
			return img, nil
		}
	}
	return nil, errors.New("no decodable embedded JPEG")
}

// scanEmbeddedJPEG searches the file for embedded JPEG streams and decodes the
// largest. Used for CR3 and RAW formats without a parseable TIFF structure.
func scanEmbeddedJPEG(f *os.File, size int64, m *Meta) (image.Image, error) {
	const maxScan = 256 << 20
	n := size
	if n > maxScan {
		n = maxScan
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, n), data); err != nil {
		return nil, err
	}
	r := bytes.NewReader(data)
	var cands []jpegCandidate
	for i := 0; i+3 < len(data); {
		j := bytes.Index(data[i:], []byte{0xFF, 0xD8, 0xFF})
		if j < 0 {
			break
		}
		off := int64(i + j)
		if _, _, ok := jpegInfo(r, off, n-off); ok {
			cands = append(cands, jpegCandidate{off, n - off})
		}
		i += j + 3
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w: no embedded preview found", ErrUnsupported)
	}
	img, err := bestJPEG(r, n, cands)
	if err != nil {
		return nil, err
	}
	if m != nil && m.Make == "" {
		// The first JPEG with EXIF often carries the camera metadata.
		for _, c := range cands {
			parseJPEGExif(r, c.off, m)
			if m.Make != "" {
				break
			}
		}
	}
	return img, nil
}

// loadRAF handles Fujifilm RAF: a fixed header pointing at a full JPEG.
func loadRAF(f *os.File, size int64, m *Meta) (image.Image, error) {
	var hdr [8]byte
	if _, err := f.ReadAt(hdr[:], 84); err != nil {
		return nil, err
	}
	off := int64(binary.BigEndian.Uint32(hdr[0:4]))
	ln := int64(binary.BigEndian.Uint32(hdr[4:8]))
	if off <= 0 || ln <= 0 || off+ln > size {
		return scanEmbeddedJPEG(f, size, m)
	}
	parseJPEGExif(f, off, m)
	img, err := bestJPEG(f, size, []jpegCandidate{{off, ln}})
	if err != nil {
		return scanEmbeddedJPEG(f, size, m)
	}
	return img, nil
}

// loadExternal converts HEIF/AVIF via whatever converter is installed.
func loadExternal(path string, m *Meta) (image.Image, error) {
	tmp, err := os.CreateTemp("", "archivis-*.jpg")
	if err != nil {
		return nil, err
	}
	out := tmp.Name()
	tmp.Close()
	defer os.Remove(out)

	var cmds [][]string
	if runtime.GOOS == "darwin" {
		cmds = append(cmds, []string{"sips", "-s", "format", "jpeg", "-s", "formatOptions", "best", path, "--out", out})
	}
	cmds = append(cmds,
		[]string{"heif-convert", "-q", "95", path, out},
		[]string{"magick", path, "-quality", "95", out},
		[]string{"vips", "copy", path, out + "[Q=95]"},
	)
	var lastErr error = fmt.Errorf("%w: no HEIF converter found (install libheif, ImageMagick or libvips)", ErrUnsupported)
	for _, c := range cmds {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		if outb, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("%s: %v: %s", c[0], err, strings.TrimSpace(string(outb)))
			if c[0] == "heif-convert" && bytes.Contains(outb, []byte("Unsupported codec")) {
				// Debian/Ubuntu ship the HEVC decoder as a separate plugin.
				lastErr = fmt.Errorf("%w (install the HEVC decoder: sudo apt install libheif-plugin-libde265, then index --retry-errors)", lastErr)
			}
			continue
		}
		data, err := os.ReadFile(out)
		if err != nil || len(data) == 0 {
			lastErr = fmt.Errorf("%s produced no output", c[0])
			continue
		}
		if err := checkConfig(data); err != nil {
			return nil, err
		}
		r := bytes.NewReader(data)
		parseJPEGExif(r, 0, m)
		// m.Orientation is now the converted file's own tag: libheif-based
		// converters rotate the pixels and write 1; a converter that leaves
		// rotation to the viewer keeps the tag, and Load applies it.
		img, err := jpeg.Decode(r)
		if err != nil {
			return nil, err
		}
		if m.Orientation <= 1 {
			m.Orientation = heifOrientation(readHEIFFile(path), img.Bounds().Dx(), img.Bounds().Dy(), c[0])
		}
		return img, nil
	}
	return nil, lastErr
}

func readHEIFFile(path string) heifInfo {
	f, err := os.Open(path)
	if err != nil {
		return heifInfo{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return heifInfo{}
	}
	return readHEIFInfo(f, st.Size())
}

// heifOrientation is the rotation still to apply to a converter's output of
// width w and height h, when the output itself carries no orientation tag.
// The container's own rotation (irot/imir) has been applied by every
// converter. A rotation recorded only in EXIF has been applied by macOS's
// sips but not by the libheif-based ones (heif-convert, ImageMagick, vips),
// which follow the HEIF rule that EXIF orientation is informational. As a
// safeguard, output that is already transposed relative to the stored image
// is taken as rotated.
func heifOrientation(info heifInfo, w, h int, converter string) int {
	o := info.ExifOrient
	if info.Transform || o < 2 || o > 8 || converter == "sips" {
		return 1
	}
	if o >= 5 && info.W > 0 && w != h && w == info.H && h == info.W {
		return 1 // already turned through 90°
	}
	return o
}

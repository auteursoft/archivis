package imageio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func loadPNG(t *testing.T, p string) *RGB {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return ToRGB(img, 0)
}

func meanAbsDiff(a, b *RGB) float64 {
	var s float64
	for i := range a.Pix {
		s += math.Abs(float64(a.Pix[i]) - float64(b.Pix[i]))
	}
	return s / float64(len(a.Pix))
}

func TestResizeMatchesPIL(t *testing.T) {
	d, err := Load("testdata/astronaut.jpg", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ref  string
		w, h int
		f    Filter
	}{
		{"testdata/astro_224_bicubic.png", 224, 224, Bicubic},
		{"testdata/astro_100x77_bilinear.png", 100, 77, Bilinear},
	} {
		ref := loadPNG(t, tc.ref)
		got := Resize(d.Img, tc.w, tc.h, tc.f)
		if mad := meanAbsDiff(got, ref); mad > 1.0 {
			t.Errorf("%s: mean abs diff %.3f too large", tc.ref, mad)
		} else {
			t.Logf("%s: mean abs diff %.3f", tc.ref, mad)
		}
	}
}

func TestOrientation(t *testing.T) {
	d, err := Load("testdata/orient6.jpg", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Meta.Orientation != 6 || d.Meta.Make != "TestCam" || d.Meta.Model != "Model X" {
		t.Fatalf("meta = %+v", d.Meta)
	}
	if d.Img.W != 600 || d.Img.H != 400 || d.SrcW != 600 || d.SrcH != 400 {
		t.Fatalf("dims = %dx%d src %dx%d", d.Img.W, d.Img.H, d.SrcW, d.SrcH)
	}
	orig, _ := Load("testdata/coffee.jpg", 0)
	if mad := meanAbsDiff(d.Img, orig.Img); mad > 3 {
		t.Fatalf("oriented image differs from original: %.2f", mad)
	}
}

func TestAreaDownscale(t *testing.T) {
	d, err := Load("testdata/coffee.jpg", 150) // 600x400 -> 150x100, exact 4x
	if err != nil {
		t.Fatal(err)
	}
	if d.Img.W != 150 || d.Img.H != 100 || d.SrcW != 600 {
		t.Fatalf("dims %dx%d", d.Img.W, d.Img.H)
	}
	full, _ := Load("testdata/coffee.jpg", 0)
	// check one block average by hand
	var s float64
	for y := 40; y < 44; y++ {
		for x := 80; x < 84; x++ {
			s += float64(full.Img.Pix[(y*600+x)*3+1])
		}
	}
	want := s / 16
	got := float64(d.Img.Pix[(10*150+20)*3+1])
	if math.Abs(want-got) > 1 {
		t.Fatalf("block avg want %.1f got %.1f", want, got)
	}
	// non-integer ratio must still fill every row/column
	d2, _ := Load("testdata/coffee.jpg", 173)
	last := d2.Img.Pix[len(d2.Img.Pix)-3:]
	if last[0] == 0 && last[1] == 0 && last[2] == 0 {
		t.Fatal("last pixel empty after fractional downscale")
	}
}

// buildFakeRAW makes a little-endian TIFF whose IFD0 carries Make/Model, an
// orientation, and a JPEG preview via JPEGInterchangeFormat, mimicking ARW/NEF.
func buildFakeRAW(t *testing.T, jpg []byte) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("II")
	binary.Write(&b, le, uint16(42))
	binary.Write(&b, le, uint32(8))
	type ent struct {
		tag, typ uint16
		count    uint32
		val      uint32
	}
	const nEnt = 5
	ifdSize := 2 + nEnt*12 + 4
	strOff := uint32(8 + ifdSize)
	makeS, modelS := "FakeCam\x00", "RAW-1\x00"
	jpgOff := strOff + uint32(len(makeS)+len(modelS))
	ents := []ent{
		{tagMake, tASCII, uint32(len(makeS)), strOff},
		{tagModel, tASCII, uint32(len(modelS)), strOff + uint32(len(makeS))},
		{tagOrientation, tShort, 1, 8},
		{tagJPEGOffset, tLong, 1, jpgOff},
		{tagJPEGLength, tLong, 1, uint32(len(jpg))},
	}
	binary.Write(&b, le, uint16(nEnt))
	for _, e := range ents {
		binary.Write(&b, le, e.tag)
		binary.Write(&b, le, e.typ)
		binary.Write(&b, le, e.count)
		binary.Write(&b, le, e.val)
	}
	binary.Write(&b, le, uint32(0))
	b.WriteString(makeS)
	b.WriteString(modelS)
	b.Write(jpg)
	b.Write(make([]byte, 1024)) // trailing "sensor data"
	return b.Bytes()
}

func TestRAWPreview(t *testing.T) {
	jpg, err := os.ReadFile("testdata/coffee.jpg")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.arw")
	if err := os.WriteFile(p, buildFakeRAW(t, jpg), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Load(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Format != "raw:arw" || d.Meta.Make != "FakeCam" || d.Meta.Model != "RAW-1" {
		t.Fatalf("format %q meta %+v", d.Format, d.Meta)
	}
	// orientation 8 rotates 600x400 -> 400x600
	if d.Img.W != 400 || d.Img.H != 600 {
		t.Fatalf("dims %dx%d", d.Img.W, d.Img.H)
	}
	// unknown container: the fallback scanner must find the JPEG too
	p2 := filepath.Join(dir, "fake.x3f")
	os.WriteFile(p2, append(append(make([]byte, 333), jpg...), make([]byte, 99)...), 0o644)
	d2, err := Load(p2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Img.W != 600 {
		t.Fatalf("scan dims %dx%d", d2.Img.W, d2.Img.H)
	}
}

func TestPHash(t *testing.T) {
	a, _ := Load("testdata/coffee.jpg", 0)
	b, _ := Load("testdata/coffee.jpg", 200) // resized copy
	c, _ := Load("testdata/astronaut.jpg", 0)
	ha, hb, hc := PHash(a.Img), PHash(b.Img), PHash(c.Img)
	if HammingDistance(ha, hb) > 6 {
		t.Errorf("resized copy distance %d", HammingDistance(ha, hb))
	}
	if HammingDistance(ha, hc) < 20 {
		t.Errorf("different images distance %d", HammingDistance(ha, hc))
	}
}

func TestToRGBGeneric(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 4, 2))
	g.Pix[5] = 200
	m := ToRGB(g, 0)
	if m.Pix[15] != 200 || m.Pix[16] != 200 {
		t.Fatal("gray conversion")
	}
}

func TestDecompressionBombRejected(t *testing.T) {
	// A valid PNG header claiming 40000x40000 RGB, with almost no data.
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 40000)
	binary.BigEndian.PutUint32(ihdr[4:], 40000)
	ihdr[8], ihdr[9] = 8, 2
	writeChunk := func(typ string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		crc := crc32.ChecksumIEEE(append([]byte(typ), data...))
		binary.Write(&b, binary.BigEndian, crc)
	}
	writeChunk("IHDR", ihdr)
	writeChunk("IEND", nil)
	p := filepath.Join(t.TempDir(), "bomb.png")
	os.WriteFile(p, b.Bytes(), 0o644)
	_, err := Load(p, 0)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestBlockinessSeparatesCompression(t *testing.T) {
	d, err := Load("testdata/coffee.jpg", 0)
	if err != nil {
		t.Fatal(err)
	}
	vals := map[int]float64{}
	for _, q := range []int{95, 30, 8} {
		b, _ := EncodeJPEG(d.Img, q)
		p := filepath.Join(t.TempDir(), "q.jpg")
		os.WriteFile(p, b, 0o644)
		r, err := Load(p, 0)
		if err != nil {
			t.Fatal(err)
		}
		vals[q] = r.Blockiness
	}
	t.Logf("blockiness q95 %.3f q30 %.3f q8 %.3f", vals[95], vals[30], vals[8])
	if !(vals[95] < vals[30] && vals[30] < vals[8]) || vals[95] > 1.15 {
		t.Fatalf("blockiness not monotonic in compression: %v", vals)
	}
}

// A RAW whose IFD advertises an absurd preview length must not make the
// decoder allocate that much: lengths come from the (untrusted) file.
func TestRAWPreviewLengthIsBounded(t *testing.T) {
	jpg, err := os.ReadFile("testdata/coffee.jpg")
	if err != nil {
		t.Fatal(err)
	}
	raw := buildFakeRAW(t, jpg)
	// Patch JPEGInterchangeFormatLength (5th IFD entry value) to ~4 GiB.
	le := binary.LittleEndian
	n := int(le.Uint16(raw[8:10]))
	for i := 0; i < n; i++ {
		e := raw[10+i*12:]
		if le.Uint16(e[0:2]) == tagJPEGLength {
			le.PutUint32(e[8:12], 0xFFFFFFF0)
		}
	}
	p := filepath.Join(t.TempDir(), "evil.arw")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d, err := Load(p, 0)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("valid preview behind a bogus length should still decode: %v", err)
	}
	if d.Img.W != 400 || d.Img.H != 600 {
		t.Fatalf("dims %dx%d", d.Img.W, d.Img.H)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Fatalf("allocated %d MB decoding a %d KB file", grew>>20, len(raw)>>10)
	}
}

// A file cut off inside an IFD must be rejected, not parsed from the
// zero-filled tail of the read buffer.
func TestTruncatedIFD(t *testing.T) {
	jpg, err := os.ReadFile("testdata/coffee.jpg")
	if err != nil {
		t.Fatal(err)
	}
	raw := buildFakeRAW(t, jpg)
	for _, cut := range []int{12, 10 + 12*2 + 5, 10 + 12*5 + 2} {
		tf, first, err := newTIFFReader(bytes.NewReader(raw[:cut]), 0, int64(cut))
		if err != nil {
			continue
		}
		d, next, err := tf.readIFD(first)
		if cut < 10+12*5 {
			if err == nil {
				t.Errorf("cut %d: truncated entries accepted (%d entries)", cut, len(d.entries))
			}
		} else if err != nil || next != 0 || len(d.entries) != 5 {
			t.Errorf("cut %d (only next-IFD pointer missing): err %v next %d entries %d", cut, err, next, len(d.entries))
		}
	}
}

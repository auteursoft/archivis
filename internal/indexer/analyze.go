package indexer

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/quality"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

// Engines bundles the loaded models. Any of them may be nil (that analysis
// step is then skipped).
type Engines struct {
	Faces *ml.FaceEngine
	CLIP  *ml.CLIP
	// Aesthetics are the catalogue's active aesthetic models (set by
	// catalog.Open from the catalogue).
	Aesthetics []AestheticModel
	Tagger     *Tagger
}

// Layout of the data directory.
type Layout struct{ Dir string }

// DB is the catalogue database. A catalogue created before the rename to
// Archivis (photodex.db) is renamed on first use, with its WAL files.
func (l Layout) DB() string {
	p := filepath.Join(l.Dir, "archivis.db")
	old := filepath.Join(l.Dir, "photodex.db")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		if _, err := os.Stat(old); err == nil && os.Rename(old, p) == nil {
			for _, sfx := range []string{"-wal", "-shm"} {
				os.Rename(old+sfx, p+sfx)
			}
		}
	}
	return p
}

// Thumb is the grid thumbnail path for a content fingerprint.
func (l Layout) Thumb(fp string) string {
	return filepath.Join(l.Dir, "thumbs", fp[:2], fp+".jpg")
}

// FaceThumb is the face crop path for a fingerprint + face ordinal.
func (l Layout) FaceThumb(fp string, ordinal int) string {
	return filepath.Join(l.Dir, "faces", fp[:2], fmt.Sprintf("%s_%d.jpg", fp, ordinal))
}

const (
	// WorkSize is the long edge of the decoded working image used for face
	// detection/alignment and CLIP.
	WorkSize = 2560
	// ThumbSize is the long edge of grid thumbnails.
	ThumbSize = 480
	// FaceThumbSize is the edge of square face crops.
	FaceThumbSize = 160
)

// Fingerprint identifies file content: a SHA-256 of the size and every byte
// (32 hex digits). Equal fingerprints mean byte-identical files, so moved
// files and copies reuse their analysis, and it names the thumbnails.
func Fingerprint(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(size))
	h.Write(sz[:])
	if _, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}

// LegacyFingerprint is the fingerprint catalogues used before full-content
// hashing: size + first and last 64 KiB (24 hex digits). Files with equal
// edges but different middles share it, so a match is only a candidate.
func LegacyFingerprint(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(size))
	h.Write(sz[:])
	const chunk = 64 << 10
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, min(size, chunk))); err != nil {
		return "", err
	}
	if size > chunk {
		start := max(chunk, size-chunk)
		if _, err := io.Copy(h, io.NewSectionReader(f, start, size-start)); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:24], nil
}

// Analysis is the in-memory result of analysing one image.
type Analysis struct {
	Decoded *imageio.Decoded
	Faces   []ml.Face
	CLIP    []float32
	Metrics quality.Metrics
	Result  *store.Result
}

// AnalyzeFile fully analyses a file and builds the catalogue record. Thumbnails
// are written into the layout when layout.Dir is non-empty.
func AnalyzeFile(path string, st os.FileInfo, fp string, eng *Engines, layout Layout) (*Analysis, error) {
	d, err := imageio.Load(path, WorkSize)
	if err != nil {
		return nil, err
	}
	if d.SrcW < 64 || d.SrcH < 64 {
		return nil, fmt.Errorf("image too small (%dx%d)", d.SrcW, d.SrcH)
	}
	a, err := AnalyzeImage(d, eng)
	if err != nil {
		return nil, err
	}
	m := d.Meta
	p := &a.Result.Photo
	p.Path = path
	p.Size = st.Size()
	p.MTime = st.ModTime().Unix()
	p.Fingerprint = fp
	p.IndexedAt = time.Now().Unix()
	p.Format = d.Format
	p.Blockiness = d.Blockiness
	p.Width, p.Height = d.SrcW, d.SrcH
	if !m.TakenAt.IsZero() {
		p.TakenAt = m.TakenAt.Unix()
	} else {
		p.TakenAt = 0
	}
	p.CameraMake, p.CameraModel, p.Lens = m.Make, m.Model, m.Lens
	p.FocalLength, p.Focal35, p.FNumber, p.ExposureTime = m.FocalLength, m.Focal35, m.FNumber, m.ExposureTime
	p.ISO, p.Flash, p.Artist = m.ISO, m.Flash, m.Artist
	if m.HasGPS {
		p.GPSLat = sql.NullFloat64{Float64: m.GPSLat, Valid: true}
		p.GPSLon = sql.NullFloat64{Float64: m.GPSLon, Valid: true}
	}
	if layout.Dir != "" {
		if err := writeThumbs(layout, fp, a); err != nil {
			return nil, fmt.Errorf("writing thumbnails: %w", err)
		}
	}
	return a, nil
}

// AnalyzeImage runs every model and metric on a decoded image.
func AnalyzeImage(d *imageio.Decoded, eng *Engines) (*Analysis, error) {
	img := d.Img
	a := &Analysis{Decoded: d, Result: &store.Result{}}
	p := &a.Result.Photo
	if eng.Faces != nil {
		// Without an EXIF orientation tag (older cameras, scans) a portrait
		// shot may be stored sideways, so also look for rotated faces.
		faces, err := eng.Faces.DetectRobust(img, ml.DetectOptions{TryRotations: d.Meta.Orientation == 0})
		if err != nil {
			return nil, err
		}
		if err := eng.Faces.Embed(img, faces); err != nil {
			return nil, err
		}
		a.Faces = faces
	}
	if eng.CLIP != nil {
		e, err := eng.CLIP.EmbedImage(img)
		if err != nil {
			return nil, err
		}
		a.CLIP = e
		p.CLIP = vindex.EncodeF16(e)
		a.Result.AestheticScores, p.Aesthetic = ScoreAesthetic(eng.Aesthetics, e)
		a.Result.Tags = eng.Tagger.Tags(e)
	}
	a.Metrics = quality.Analyze(img)
	q := a.Metrics
	p.PHash = imageio.PHash(img)

	// Faces: store normalised boxes plus per-face quality.
	var bestFace *store.Face
	for i, f := range a.Faces {
		sf := store.Face{
			Ordinal:   i,
			X1:        clamp01(float64(f.Box[0]) / float64(img.W)),
			Y1:        clamp01(float64(f.Box[1]) / float64(img.H)),
			X2:        clamp01(float64(f.Box[2]) / float64(img.W)),
			Y2:        clamp01(float64(f.Box[3]) / float64(img.H)),
			Score:     float64(f.Score),
			WidthPx:   float64(f.Width()) * float64(d.SrcW) / float64(img.W),
			Sharpness: quality.RegionSharpness(f.Aligned),
			Yaw:       f.Yaw(),
			Embedding: vindex.EncodeF16(f.Embedding),
		}
		a.Result.Faces = append(a.Result.Faces, sf)
	}
	for i := range a.Result.Faces {
		f := &a.Result.Faces[i]
		if bestFace == nil || f.X2-f.X1 > bestFace.X2-bestFace.X1 {
			bestFace = f
		}
	}
	// Count only confident faces as people (see ml.PersonThresh); weaker
	// detections stay searchable but are often pets or statues.
	p.FaceCount = 0
	for _, f := range a.Faces {
		if f.Score >= ml.PersonThresh {
			p.FaceCount++
		}
	}

	p.Sharpness, p.SharpnessGlobal = q.Sharpness, q.SharpnessGlobal
	p.Brightness, p.Highlights, p.Shadows = q.Brightness, q.Highlights, q.Shadows
	p.DynamicRange, p.Contrast = q.DynamicRange, q.Contrast
	p.CastA, p.CastB, p.CastStrength = q.CastA, q.CastB, q.CastStrength
	p.Colorfulness, p.Saturation, p.Monochrome, p.Noise = q.Colorfulness, q.Saturation, q.Monochrome, q.Noise
	// When a face is a prominent subject (>= 6% of frame width and big
	// enough to judge), focus on the face matters most: a sharp background
	// behind a soft face is a missed shot.
	if bestFace != nil && bestFace.X2-bestFace.X1 >= 0.06 && bestFace.WidthPx >= 48 {
		p.FaceSharpness = sql.NullFloat64{Float64: bestFace.Sharpness, Valid: true}
		q.FocusScore = 0.7*FaceFocusScore(bestFace.Sharpness) + 0.3*q.FocusScore
	}
	p.FocusScore, p.ExposureScore, p.ColorScore, p.NoiseScore = q.FocusScore, q.ExposureScore, q.ColorScore, q.NoiseScore
	// Technical is the human-calibrated rating (see quality.Rate), which
	// tracks human judgements far better than a hand-weighted mix.
	p.Technical = round1(quality.Technical(quality.Rate(img), q, d.Blockiness))
	p.Overall = Overall(p.Technical, p.Aesthetic)
	a.Metrics = q
	p.Version = store.SchemaVersion
	return a, nil
}

// FaceFocusScore maps Laplacian variance of an aligned 112px face crop to 0..1.
func FaceFocusScore(v float64) float64 {
	lo, hi := math.Log10(1+FaceSharpLo), math.Log10(1+FaceSharpHi)
	t := (math.Log10(1+v) - lo) / (hi - lo)
	return math.Max(0, math.Min(1, t))
}

// Calibrated on aligned ArcFace crops: soft faces fall below ~20, crisp ones
// exceed ~250.
const (
	FaceSharpLo = 20.0
	FaceSharpHi = 250.0
)

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }
func round1(v float64) float64  { return math.Round(v*10) / 10 }

// AestheticWeight is aesthetics' share of the overall score. Validated on
// human ratings of appeal (EVA) and of everyday photo quality (BIQ2021),
// an even split serves both best (Spearman 0.71 on each; VALIDATION.md).
const AestheticWeight = 0.5

// Overall blends the technical score (0..100) with the aesthetic rating
// (0..10); without an aesthetic rating it is the technical score.
func Overall(technical float64, aesthetic sql.NullFloat64) float64 {
	if !aesthetic.Valid {
		return technical
	}
	return round1((1-AestheticWeight)*technical + AestheticWeight*ml.AestheticPercent(aesthetic.Float64))
}

func writeThumbs(l Layout, fp string, a *Analysis) error {
	img := a.Decoded.Img
	tp := l.Thumb(fp)
	if err := os.MkdirAll(filepath.Dir(tp), 0o755); err != nil {
		return err
	}
	b, err := imageio.EncodeJPEG(imageio.Thumbnail(img, ThumbSize), 82)
	if err != nil {
		return err
	}
	if err := WriteAtomic(tp, b); err != nil {
		return err
	}
	for i, f := range a.Faces {
		cx, cy := float64(f.Box[0]+f.Box[2])/2, float64(f.Box[1]+f.Box[3])/2
		side := 1.6 * math.Max(float64(f.Width()), float64(f.Box[3]-f.Box[1]))
		x0, y0 := int(cx-side/2), int(cy-side/2)
		crop := img.Crop(x0, y0, x0+int(side), y0+int(side))
		ft := imageio.Thumbnail(crop, FaceThumbSize)
		b, err := imageio.EncodeJPEG(ft, 85)
		if err != nil {
			return err
		}
		fpath := l.FaceThumb(fp, i)
		if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
			return err
		}
		if err := WriteAtomic(fpath, b); err != nil {
			return err
		}
	}
	return nil
}

// WriteAtomic writes b to path via a uniquely named temporary file in the
// same directory, so concurrent writers of one path (byte-identical photos
// share a fingerprint, hence thumbnail names) never touch each other's file.
func WriteAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644) // CreateTemp makes 0600
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

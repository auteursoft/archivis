package ml

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/auteursoft/archivis/internal/imageio"
)

// Face is one detected face, in the coordinates of the image passed to Detect.
type Face struct {
	Box       [4]float32    // x1, y1, x2, y2
	Kps       [5][2]float32 // left eye, right eye, nose, left mouth, right mouth
	Score     float32
	Embedding []float32 // L2-normalised ArcFace embedding (512-d)
	// EmbNorm is the L2 norm of the raw ArcFace output. It tracks how
	// recognisable the face is: blurred, tiny, occluded and non-human faces
	// (pets, statues) produce markedly smaller norms.
	EmbNorm float32
	Aligned *imageio.RGB
}

// Width of the face box in pixels.
func (f *Face) Width() float32 { return f.Box[2] - f.Box[0] }

// Yaw is a cheap head-turn estimate from the landmarks: 0 = frontal,
// ±1 ≈ profile.
func (f *Face) Yaw() float64 {
	ex := float64(f.Kps[0][0]+f.Kps[1][0]) / 2
	ed := math.Hypot(float64(f.Kps[1][0]-f.Kps[0][0]), float64(f.Kps[1][1]-f.Kps[0][1]))
	if ed < 1e-3 {
		return 1
	}
	y := (float64(f.Kps[2][0]) - ex) / ed
	return math.Max(-1, math.Min(1, y))
}

// FaceEngine runs SCRFD detection and ArcFace recognition.
type FaceEngine struct {
	det     *session
	rec     *session
	DetSize int     // detector input size (square), default 640
	Thresh  float32 // detection score threshold
	NMS     float32
	MinSize float32 // minimum face width in source pixels
}

// NewFaceEngine loads det_10g.onnx and w600k_r50.onnx from dir (InsightFace
// buffalo_l pack).
func NewFaceEngine(dir string, o Options) (*FaceEngine, error) {
	det, err := openSession(filepath.Join(dir, "det_10g.onnx"), o)
	if err != nil {
		return nil, err
	}
	rec, err := openSession(filepath.Join(dir, "w600k_r50.onnx"), o)
	if err != nil {
		det.close()
		return nil, err
	}
	return &FaceEngine{det: det, rec: rec, DetSize: 640, Thresh: 0.5, NMS: 0.4, MinSize: 24}, nil
}

func (e *FaceEngine) Close() {
	e.det.close()
	e.rec.close()
}

// Detect finds faces in img (any size). Returned coordinates are in img space.
func (e *FaceEngine) Detect(img *imageio.RGB) ([]Face, error) {
	S := e.DetSize
	// Letterbox: keep aspect ratio, pad bottom/right (as InsightFace does).
	var nw, nh int
	if float64(img.H)/float64(img.W) > 1 {
		nh = S
		nw = int(float64(S) * float64(img.W) / float64(img.H))
	} else {
		nw = S
		nh = int(float64(S) * float64(img.H) / float64(img.W))
	}
	nw, nh = max(nw, 1), max(nh, 1)
	scale := float32(nh) / float32(img.H)
	small := imageio.Resize(img, nw, nh, imageio.Bilinear)

	plane := S * S
	in := make([]float32, 3*plane)
	pad := float32(-127.5 / 128.0)
	for i := range in {
		in[i] = pad
	}
	for y := 0; y < nh; y++ {
		for x := 0; x < nw; x++ {
			p := small.Pix[(y*nw+x)*3:]
			o := y*S + x
			in[o] = (float32(p[0]) - 127.5) / 128
			in[plane+o] = (float32(p[1]) - 127.5) / 128
			in[2*plane+o] = (float32(p[2]) - 127.5) / 128
		}
	}
	t, err := ort.NewTensor(ort.NewShape(1, 3, int64(S), int64(S)), in)
	if err != nil {
		return nil, err
	}
	defer t.Destroy()
	outs, shapes, err := e.det.run(t)
	if err != nil {
		return nil, fmt.Errorf("face detection: %w", err)
	}

	// Group outputs by stride using their row counts and last dimension.
	type triple struct{ score, box, kps []float32 }
	strides := []int{8, 16, 32}
	byStride := map[int]*triple{}
	for _, s := range strides {
		byStride[s] = &triple{}
	}
	for i, sh := range shapes {
		if len(sh) == 0 {
			continue
		}
		last := sh[len(sh)-1]
		if last <= 0 {
			continue // not a fixed-size detector output
		}
		rows := len(outs[i]) / int(last)
		for _, s := range strides {
			if rows == (S/s)*(S/s)*2 {
				switch last {
				case 1:
					byStride[s].score = outs[i]
				case 4:
					byStride[s].box = outs[i]
				case 10:
					byStride[s].kps = outs[i]
				}
			}
		}
	}

	var faces []Face
	for _, s := range strides {
		tr := byStride[s]
		if tr.score == nil || tr.box == nil || tr.kps == nil {
			return nil, fmt.Errorf("unexpected detector outputs for stride %d", s)
		}
		fw := S / s
		for idx, sc := range tr.score {
			if sc < e.Thresh {
				continue
			}
			loc := idx / 2 // two anchors per location
			cx := float32((loc % fw) * s)
			cy := float32((loc / fw) * s)
			b := tr.box[idx*4:]
			fs := float32(s)
			f := Face{Score: sc}
			f.Box = [4]float32{
				(cx - b[0]*fs) / scale, (cy - b[1]*fs) / scale,
				(cx + b[2]*fs) / scale, (cy + b[3]*fs) / scale,
			}
			k := tr.kps[idx*10:]
			for j := 0; j < 5; j++ {
				f.Kps[j] = [2]float32{(cx + k[j*2]*fs) / scale, (cy + k[j*2+1]*fs) / scale}
			}
			faces = append(faces, f)
		}
	}
	faces = nms(faces, e.NMS)
	out := faces[:0]
	for _, f := range faces {
		if f.Width() >= e.MinSize {
			out = append(out, f)
		}
	}
	return out, nil
}

func nms(faces []Face, thr float32) []Face {
	sort.Slice(faces, func(i, j int) bool { return faces[i].Score > faces[j].Score })
	keep := make([]Face, 0, len(faces))
	suppressed := make([]bool, len(faces))
	for i := range faces {
		if suppressed[i] {
			continue
		}
		keep = append(keep, faces[i])
		a := faces[i].Box
		areaA := (a[2] - a[0] + 1) * (a[3] - a[1] + 1)
		for j := i + 1; j < len(faces); j++ {
			if suppressed[j] {
				continue
			}
			b := faces[j].Box
			w := min(a[2], b[2]) - max(a[0], b[0]) + 1
			h := min(a[3], b[3]) - max(a[1], b[1]) + 1
			if w <= 0 || h <= 0 {
				continue
			}
			inter := w * h
			areaB := (b[2] - b[0] + 1) * (b[3] - b[1] + 1)
			if inter/(areaA+areaB-inter) > thr {
				suppressed[j] = true
			}
		}
	}
	return keep
}

// arcfaceDst is the canonical 5-point template for 112x112 ArcFace crops.
var arcfaceDst = [5][2]float64{
	{38.2946, 51.6963}, {73.5318, 51.5014}, {56.0252, 71.7366},
	{41.5493, 92.3655}, {70.7299, 92.2041},
}

// similarity estimates the least-squares similarity transform mapping src to
// dst (equivalent to Umeyama without reflection). Returns [a, b, tx, ty] for
// x' = a*x - b*y + tx, y' = b*x + a*y + ty.
func similarity(src [5][2]float32, dst [5][2]float64) [4]float64 {
	var msx, msy, mdx, mdy float64
	for i := 0; i < 5; i++ {
		msx += float64(src[i][0])
		msy += float64(src[i][1])
		mdx += dst[i][0]
		mdy += dst[i][1]
	}
	msx, msy, mdx, mdy = msx/5, msy/5, mdx/5, mdy/5
	var num1, num2, den float64
	for i := 0; i < 5; i++ {
		xs, ys := float64(src[i][0])-msx, float64(src[i][1])-msy
		xd, yd := dst[i][0]-mdx, dst[i][1]-mdy
		num1 += xs*xd + ys*yd
		num2 += xs*yd - ys*xd
		den += xs*xs + ys*ys
	}
	if den == 0 {
		return [4]float64{1, 0, 0, 0}
	}
	a, b := num1/den, num2/den
	return [4]float64{a, b, mdx - (a*msx - b*msy), mdy - (b*msx + a*msy)}
}

// AlignFace warps the face to a size x size crop using the ArcFace template
// (scaled for sizes other than 112).
func AlignFace(img *imageio.RGB, kps [5][2]float32, size int) *imageio.RGB {
	dst := arcfaceDst
	if size != 112 {
		s := float64(size) / 112
		for i := range dst {
			dst[i][0] *= s
			dst[i][1] *= s
		}
	}
	m := similarity(kps, dst)
	a, b, tx, ty := m[0], m[1], m[2], m[3]
	// inverse of [[a,-b],[b,a]] is 1/(a²+b²) * [[a,b],[-b,a]]
	det := a*a + b*b
	ia, ib := a/det, b/det
	out := imageio.NewRGB(size, size)
	for v := 0; v < size; v++ {
		for u := 0; u < size; u++ {
			du, dv := float64(u)-tx, float64(v)-ty
			sx := ia*du + ib*dv
			sy := -ib*du + ia*dv
			bilinear(img, sx, sy, out.Pix[(v*size+u)*3:])
		}
	}
	return out
}

func bilinear(img *imageio.RGB, x, y float64, dst []uint8) {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(x0), y-float64(y0)
	var acc [3]float64
	for j := 0; j < 2; j++ {
		yy := y0 + j
		wy := fy
		if j == 0 {
			wy = 1 - fy
		}
		for i := 0; i < 2; i++ {
			xx := x0 + i
			wx := fx
			if i == 0 {
				wx = 1 - fx
			}
			if xx < 0 || yy < 0 || xx >= img.W || yy >= img.H {
				continue // constant 0 border, like cv2.warpAffine
			}
			p := img.Pix[(yy*img.W+xx)*3:]
			w := wx * wy
			acc[0] += w * float64(p[0])
			acc[1] += w * float64(p[1])
			acc[2] += w * float64(p[2])
		}
	}
	for c := 0; c < 3; c++ {
		dst[c] = uint8(math.Min(255, acc[c]+0.5))
	}
}

// Embed aligns every face and computes ArcFace embeddings in one batch.
// FaceEmbedDim is the size of the face embeddings Archivis stores and searches.
const FaceEmbedDim = 512

func (e *FaceEngine) Embed(img *imageio.RGB, faces []Face) error {
	if len(faces) == 0 {
		return nil
	}
	const S = 112
	plane := S * S
	in := make([]float32, len(faces)*3*plane)
	for fi := range faces {
		al := AlignFace(img, faces[fi].Kps, S)
		faces[fi].Aligned = al
		base := fi * 3 * plane
		for i := 0; i < plane; i++ {
			p := al.Pix[i*3:]
			in[base+i] = (float32(p[0]) - 127.5) / 127.5
			in[base+plane+i] = (float32(p[1]) - 127.5) / 127.5
			in[base+2*plane+i] = (float32(p[2]) - 127.5) / 127.5
		}
	}
	t, err := ort.NewTensor(ort.NewShape(int64(len(faces)), 3, S, S), in)
	if err != nil {
		return err
	}
	defer t.Destroy()
	outs, _, err := e.rec.run(t)
	if err != nil {
		return fmt.Errorf("face embedding: %w", err)
	}
	// The face index holds 512-d ArcFace embeddings; anything else would be
	// stored but never searchable, so refuse it loudly.
	if len(outs) == 0 || len(outs[0]) != len(faces)*FaceEmbedDim {
		got := 0
		if len(outs) > 0 {
			got = len(outs[0])
		}
		return fmt.Errorf("face embedding: model returned %d values for %d faces, want %d per face (ArcFace w600k_r50)", got, len(faces), FaceEmbedDim)
	}
	out := outs[0]
	dim := FaceEmbedDim
	for fi := range faces {
		raw := append([]float32(nil), out[fi*dim:(fi+1)*dim]...)
		var n float64
		for _, x := range raw {
			n += float64(x) * float64(x)
		}
		faces[fi].EmbNorm = float32(math.Sqrt(n))
		faces[fi].Embedding = Normalize(raw)
	}
	return nil
}

// Normalize scales v to unit L2 norm in place and returns it.
func Normalize(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// Dot product of two equal-length vectors.
func Dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// FallbackThresh is the minimum detection score for faces found by the
// fallback passes of DetectRobust.
var FallbackThresh float32 = 0.7

// PersonThresh is the detection score above which a face is counted as a
// person (face counts, "photos with people"). Lower-scoring detections are
// kept for face search but are often animals or statues: on real photos a
// 0.6 cut keeps 95% of human faces and drops 56% of animal faces.
const PersonThresh = 0.6

// DetectOptions control the fallback passes of DetectRobust.
type DetectOptions struct {
	// TryRotations also searches the image rotated by ±90° when nothing is
	// found upright. Worth it for photos without an EXIF orientation tag
	// (older cameras, scans), where portrait shots are often stored sideways.
	TryRotations bool
}

// DetectRobust runs Detect and, only if it finds nothing, retries with
// fallbacks for the detector's known blind spots: faces that fill the frame
// (the image is shrunk inside a larger canvas) and sideways faces.
// Returned coordinates are always in img space.
func (e *FaceEngine) DetectRobust(img *imageio.RGB, o DetectOptions) ([]Face, error) {
	faces, err := e.Detect(img)
	if err != nil || len(faces) > 0 {
		return faces, err
	}
	// Fallback passes search harder in photos where nothing was found, so
	// their detections must be more confident: on Open Images vs pet photos,
	// requiring 0.7 keeps 85% of real faces but only 12% of animal faces.
	confident := func(fs []Face) []Face {
		out := fs[:0]
		for _, f := range fs {
			if f.Score >= FallbackThresh {
				out = append(out, f)
			}
		}
		return out
	}
	// Close-ups: place the image in a canvas twice its size so a face that
	// filled the frame appears at half size, with context around it.
	padded := padCanvas(img, 2)
	if faces, err = e.Detect(padded); err != nil {
		return nil, err
	}
	if faces = confident(faces); len(faces) > 0 {
		return faces, nil
	}
	if !o.TryRotations {
		return nil, nil
	}
	for _, cw := range []bool{true, false} {
		rot := rotate90(img, cw)
		fs, err := e.Detect(rot)
		if err != nil {
			return nil, err
		}
		if fs = confident(fs); len(fs) > 0 {
			for i := range fs {
				fs[i] = unrotateFace(fs[i], img.W, img.H, cw)
			}
			return fs, nil
		}
	}
	return nil, nil
}

// padCanvas returns img in the top-left of a black canvas factor times larger,
// so detections keep img coordinates.
func padCanvas(img *imageio.RGB, factor int) *imageio.RGB {
	out := imageio.NewRGB(img.W*factor, img.H*factor)
	for y := 0; y < img.H; y++ {
		copy(out.Pix[y*out.W*3:], img.Pix[y*img.W*3:(y+1)*img.W*3])
	}
	return out
}

func rotate90(img *imageio.RGB, cw bool) *imageio.RGB {
	if cw {
		return imageio.Orient(img, 6)
	}
	return imageio.Orient(img, 8)
}

// unrotateFace maps a face found in a rotated copy back to the original
// (w x h) image. Landmarks keep their semantic order, so alignment still
// produces an upright crop.
func unrotateFace(f Face, w, h int, cw bool) Face {
	back := func(u, v float32) (float32, float32) {
		if cw { // rotated (x,y) -> (h-1-y, x)
			return v, float32(h-1) - u
		}
		// counter-clockwise: (x,y) -> (y, w-1-x)
		return float32(w-1) - v, u
	}
	x1, y1 := back(f.Box[0], f.Box[1])
	x2, y2 := back(f.Box[2], f.Box[3])
	f.Box = [4]float32{min(x1, x2), min(y1, y2), max(x1, x2), max(y1, y2)}
	for i := range f.Kps {
		f.Kps[i][0], f.Kps[i][1] = back(f.Kps[i][0], f.Kps[i][1])
	}
	return f
}

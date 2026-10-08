// Command validate measures archivis against ground truth. It writes raw
// per-item CSVs; tools/validate/stats.py turns them into metrics.
//
//	validate detect   -models M -det 640 -out det.csv DIR...       face detections per image
//	validate lfwpairs -data D -lfw ROOT -pairs pairs.txt -out p.csv  verification scores from a catalogue
//	validate autolabel -data D -lfw ROOT -k 1 -out a.csv             simulate naming people, then auto-match (on a snapshot of D)
//	validate ladder   -out l.csv DIR...                              quality metrics under graded degradations
//	validate quality  -out q.csv DIR...                              quality metrics per image (for MOS)
//	validate text     -data D -models M -queries q.tsv -out t.csv    text search rankings
//	validate personal -eva EVA -clip eva_clip.csv -out p.csv         training on one person's aesthetic judgements
package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"

	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/quality"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

var decodeF16 = vindex.DecodeF16

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: validate detect|lfwpairs|autolabel|ladder|quality|text ...")
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"detect": detect, "lfwpairs": lfwPairs, "autolabel": autolabel,
		"ladder": ladder, "quality": qualityCmd, "text": textCmd, "clip": clipCmd, "synth": synthCmd,
		"personal": personalCmd,
	}
	fn := cmds[os.Args[1]]
	if fn == nil {
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
	if err := fn(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func listImages(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		filepath.Walk(d, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && imageio.IsImagePath(p) {
				out = append(out, p)
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

func csvOut(path string, header []string) (*csv.Writer, func()) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	w := csv.NewWriter(f)
	w.Write(header)
	return w, func() { w.Flush(); f.Close() }
}

func ff(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

func initORT() ml.Options {
	o := ml.Options{IntraOpThreads: 1}
	if err := ml.Init(o); err != nil {
		panic(err)
	}
	return o
}

// parallel runs fn over items with n workers, serialising writes via mu.
func parallel(items []string, n int, fn func(string)) {
	ch := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				fn(it)
			}
		}()
	}
	for i, it := range items {
		if i%200 == 0 {
			fmt.Fprintf(os.Stderr, "\r%d/%d", i, len(items))
		}
		ch <- it
	}
	close(ch)
	wg.Wait()
	fmt.Fprintln(os.Stderr)
}

// ---- face detection -----------------------------------------------------

func detect(args []string) error {
	fs := flag.NewFlagSet("detect", flag.ExitOnError)
	models := fs.String("models", "", "models dir")
	det := fs.Int("det", 640, "detector input size")
	thresh := fs.Float64("thresh", 0.5, "detection threshold")
	minSize := fs.Float64("min", 0, "min face width in working-image px")
	robust := fs.Bool("robust", false, "use DetectRobust (close-up fallback)")
	rot := fs.Bool("rot", false, "with -robust, also try rotated images")
	embed := fs.Bool("embed", false, "also run ArcFace and report the raw embedding norm")
	out := fs.String("out", "det.csv", "output")
	fs.Parse(args)
	o := initORT()
	eng, err := ml.NewFaceEngine(filepath.Join(*models, "buffalo_l"), o)
	if err != nil {
		return err
	}
	eng.DetSize, eng.Thresh, eng.MinSize = *det, float32(*thresh), float32(*minSize)
	w, done := csvOut(*out, []string{"image", "w", "h", "x1", "y1", "x2", "y2", "score", "norm"})
	defer done()
	var mu sync.Mutex
	parallel(listImages(fs.Args()), 4, func(p string) {
		d, err := imageio.Load(p, indexer.WorkSize)
		if err != nil {
			return
		}
		var faces []ml.Face
		if *robust {
			faces, err = eng.DetectRobust(d.Img, ml.DetectOptions{TryRotations: *rot})
		} else {
			faces, err = eng.Detect(d.Img)
		}
		if err != nil {
			return
		}
		if *embed {
			eng.Embed(d.Img, faces)
		}
		mu.Lock()
		defer mu.Unlock()
		id := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		W, H := float64(d.Img.W), float64(d.Img.H)
		w.Write([]string{id, ff(float64(d.SrcW)), ff(float64(d.SrcH)), "", "", "", "", "", ""})
		for _, f := range faces {
			w.Write([]string{id, ff(float64(d.SrcW)), ff(float64(d.SrcH)),
				ff(float64(f.Box[0]) / W), ff(float64(f.Box[1]) / H), ff(float64(f.Box[2]) / W), ff(float64(f.Box[3]) / H), ff(float64(f.Score)), ff(float64(f.EmbNorm))})
		}
	})
	return nil
}

// ---- LFW verification ---------------------------------------------------

// centralFaces maps an LFW image path to the embedding of the face closest
// to the image centre (LFW labels only the centred person).
func centralFaces(st *store.Store) (map[string]int64, map[int64][]float32, error) {
	rows, err := st.DB.Query(`SELECT p.path, f.id, f.x1, f.y1, f.x2, f.y2, f.embedding FROM faces f JOIN photos p ON p.id = f.photo_id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	best := map[string]float64{}
	byPath := map[string]int64{}
	emb := map[int64][]float32{}
	for rows.Next() {
		var path string
		var id int64
		var x1, y1, x2, y2 float64
		var b []byte
		rows.Scan(&path, &id, &x1, &y1, &x2, &y2, &b)
		d := math.Hypot((x1+x2)/2-0.5, (y1+y2)/2-0.5)
		if cur, ok := best[path]; !ok || d < cur {
			best[path] = d
			byPath[path] = id
		}
		emb[id] = decodeF16(b)
	}
	return byPath, emb, rows.Err()
}

func lfwPath(root, name string, n int) string {
	return filepath.Join(root, name, fmt.Sprintf("%s_%04d.jpg", name, n))
}

func lfwPairs(args []string) error {
	fs := flag.NewFlagSet("lfwpairs", flag.ExitOnError)
	data := fs.String("data", "", "catalogue data dir")
	root := fs.String("lfw", "", "LFW image root (folder of person folders)")
	pairs := fs.String("pairs", "", "pairs.txt")
	out := fs.String("out", "pairs.csv", "output")
	fs.Parse(args)
	st, err := store.Open(indexer.Layout{Dir: *data}.DB()) // adopts a pre-rename photodex.db
	if err != nil {
		return err
	}
	defer st.Close()
	abs, _ := filepath.Abs(*root)
	byPath, emb, err := centralFaces(st)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(*pairs)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")[1:]
	w, done := csvOut(*out, []string{"fold", "same", "score", "missing"})
	defer done()
	for i, l := range lines {
		f := strings.Fields(l)
		var a, c string
		same := len(f) == 3
		if same {
			n1, _ := strconv.Atoi(f[1])
			n2, _ := strconv.Atoi(f[2])
			a, c = lfwPath(abs, f[0], n1), lfwPath(abs, f[0], n2)
		} else {
			n1, _ := strconv.Atoi(f[1])
			n2, _ := strconv.Atoi(f[3])
			a, c = lfwPath(abs, f[0], n1), lfwPath(abs, f[2], n2)
		}
		ia, oka := byPath[a]
		ic, okc := byPath[c]
		score, missing := -1.0, 1
		if oka && okc {
			score, missing = float64(ml.Dot(emb[ia], emb[ic])), 0
		}
		s := "0"
		if same {
			s = "1"
		}
		w.Write([]string{strconv.Itoa(i / 600), s, ff(score), strconv.Itoa(missing)})
	}
	return nil
}

// ---- auto-labelling simulation -----------------------------------------

func autolabel(args []string) error {
	fs := flag.NewFlagSet("autolabel", flag.ExitOnError)
	data := fs.String("data", "", "catalogue data dir (left untouched: the simulation runs on a snapshot)")
	inPlace := fs.Bool("in-place", false, "run directly on -data, DESTROYING its people and face labels")
	root := fs.String("lfw", "", "LFW root")
	k := fs.Int("k", 1, "faces confirmed per person")
	minImages := fs.Int("min-images", 6, "only name people with at least this many images")
	people := fs.Int("people", 0, "name at most this many people (0 = all eligible)")
	thr := fs.Float64("threshold", float64(catalog.DefaultMatch.Threshold), "auto-match threshold")
	margin := fs.Float64("margin", float64(catalog.DefaultMatch.Margin), "auto-match margin")
	seed := fs.Int64("seed", 1, "random seed")
	out := fs.String("out", "autolabel.csv", "output")
	fs.Parse(args)
	// The simulation clears every label and person. Unless explicitly told
	// otherwise, work on a consistent snapshot so the caller's catalogue
	// (and its hand-made labels) is never modified.
	dir := *data
	if !*inPlace {
		snap, err := os.MkdirTemp("", "archivis-autolabel-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(snap)
		src, err := sql.Open("sqlite3", "file:"+indexer.Layout{Dir: *data}.DB()+"?mode=ro")
		if err != nil {
			return err
		}
		_, err = src.Exec(`VACUUM INTO ?`, filepath.Join(snap, "archivis.db"))
		src.Close()
		if err != nil {
			return fmt.Errorf("snapshotting %s: %w", *data, err)
		}
		fmt.Fprintf(os.Stderr, "working on a snapshot of %s (use -in-place to modify it directly)\n", *data)
		dir = snap
	}
	cat, err := catalog.Open(dir, nil)
	if err != nil {
		return err
	}
	defer cat.Close()
	st := cat.Store
	st.DB.Exec(`UPDATE faces SET person_id = NULL, person_source = NULL, person_sim = NULL`)
	st.DB.Exec(`DELETE FROM people`)
	abs, _ := filepath.Abs(*root)
	byPath, _, err := centralFaces(st)
	if err != nil {
		return err
	}
	// identity of every central face, from the folder name
	idOf := map[int64]string{}
	facesOf := map[string][]int64{}
	for p, fid := range byPath {
		rel, _ := filepath.Rel(abs, p)
		name := strings.Split(rel, string(filepath.Separator))[0]
		idOf[fid] = name
		facesOf[name] = append(facesOf[name], fid)
	}
	var names []string
	for n, fs := range facesOf {
		if len(fs) >= *minImages {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	rng := rand.New(rand.NewSource(*seed))
	rng.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	if *people > 0 && len(names) > *people {
		names = names[:*people]
	}
	named := map[string]bool{}
	exemplar := map[int64]bool{}
	for _, n := range names {
		fs := append([]int64(nil), facesOf[n]...)
		sort.Slice(fs, func(i, j int) bool { return fs[i] < fs[j] })
		rng.Shuffle(len(fs), func(i, j int) { fs[i], fs[j] = fs[j], fs[i] })
		pid, err := st.EnsurePerson(n)
		if err != nil {
			return err
		}
		kk := min(*k, len(fs)) // -k may exceed -min-images
		if err := st.AssignFaces(pid, fs[:kk], "manual", nil); err != nil {
			return err
		}
		for _, f := range fs[:kk] {
			exemplar[f] = true
		}
		named[n] = true
	}
	nAuto, err := cat.AutoMatch(catalog.MatchOptions{Threshold: float32(*thr), Margin: float32(*margin)})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "named %d people with %d exemplars each; %d faces auto-labelled\n", len(names), *k, nAuto)
	pname := map[int64]string{}
	ps, _ := st.People()
	for _, p := range ps {
		pname[p.ID] = p.Name
	}
	rows, err := st.DB.Query(`SELECT id, COALESCE(person_id, 0), COALESCE(person_source, ''), COALESCE(person_sim, 0) FROM faces`)
	if err != nil {
		return err
	}
	defer rows.Close()
	w, done := csvOut(*out, []string{"face", "truth", "truth_named", "central", "exemplar", "assigned", "source", "sim"})
	defer done()
	for rows.Next() {
		var id, pid int64
		var src string
		var sim float64
		rows.Scan(&id, &pid, &src, &sim)
		truth, central := idOf[id]
		tn := "0"
		if named[truth] {
			tn = "1"
		}
		c, e := "0", "0"
		if central {
			c = "1"
		}
		if exemplar[id] {
			e = "1"
		}
		w.Write([]string{strconv.FormatInt(id, 10), truth, tn, c, e, pname[pid], src, ff(sim)})
	}
	return nil
}

// ---- quality: degradation ladders ---------------------------------------

type degrade struct {
	name   string
	levels []float64
	apply  func(*imageio.RGB, float64, *rand.Rand) *imageio.RGB
}

var srgb2lin, lin2srgb = func() ([256]float64, func(float64) uint8) {
	var t [256]float64
	for i := range t {
		c := float64(i) / 255
		if c <= 0.04045 {
			t[i] = c / 12.92
		} else {
			t[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	inv := func(v float64) uint8 {
		if v <= 0 {
			return 0
		}
		var c float64
		if v <= 0.0031308 {
			c = v * 12.92
		} else {
			c = 1.055*math.Pow(v, 1/2.4) - 0.055
		}
		return uint8(math.Max(0, math.Min(255, math.Round(c*255))))
	}
	return t, inv
}()

func gaussBlur(m *imageio.RGB, sigma float64) *imageio.RGB {
	if sigma <= 0 {
		return m
	}
	r := int(math.Ceil(3 * sigma))
	k := make([]float64, 2*r+1)
	var s float64
	for i := -r; i <= r; i++ {
		k[i+r] = math.Exp(-float64(i*i) / (2 * sigma * sigma))
		s += k[i+r]
	}
	for i := range k {
		k[i] /= s
	}
	return convolveSep(m, k, k)
}

func convolveSep(m *imageio.RGB, kx, ky []float64) *imageio.RGB {
	tmp := make([]float64, len(m.Pix))
	rx, ry := len(kx)/2, len(ky)/2
	for y := 0; y < m.H; y++ {
		for x := 0; x < m.W; x++ {
			for c := 0; c < 3; c++ {
				var a float64
				for i, w := range kx {
					xx := min(max(x+i-rx, 0), m.W-1)
					a += w * float64(m.Pix[(y*m.W+xx)*3+c])
				}
				tmp[(y*m.W+x)*3+c] = a
			}
		}
	}
	out := imageio.NewRGB(m.W, m.H)
	for y := 0; y < m.H; y++ {
		for x := 0; x < m.W; x++ {
			for c := 0; c < 3; c++ {
				var a float64
				for i, w := range ky {
					yy := min(max(y+i-ry, 0), m.H-1)
					a += w * tmp[(yy*m.W+x)*3+c]
				}
				out.Pix[(y*m.W+x)*3+c] = uint8(math.Max(0, math.Min(255, math.Round(a))))
			}
		}
	}
	return out
}

func mapLin(m *imageio.RGB, f func(c int, v float64) float64) *imageio.RGB {
	out := imageio.NewRGB(m.W, m.H)
	for i, v := range m.Pix {
		out.Pix[i] = lin2srgb(f(i%3, srgb2lin[v]))
	}
	return out
}

func jpegRoundTrip(m *imageio.RGB, q int) *imageio.RGB {
	var buf bytes.Buffer
	jpeg.Encode(&buf, m.ToImage(), &jpeg.Options{Quality: q})
	img, _ := jpeg.Decode(&buf)
	return imageio.ToRGB(img, 0)
}

var degradations = []degrade{
	{"blur", []float64{0.5, 1, 2, 4}, func(m *imageio.RGB, s float64, _ *rand.Rand) *imageio.RGB { return gaussBlur(m, s) }},
	{"motion", []float64{3, 7, 15, 31}, func(m *imageio.RGB, l float64, _ *rand.Rand) *imageio.RGB {
		k := make([]float64, int(l))
		for i := range k {
			k[i] = 1 / l
		}
		return convolveSep(m, k, []float64{1})
	}},
	{"noise", []float64{2, 4, 8, 16}, func(m *imageio.RGB, s float64, r *rand.Rand) *imageio.RGB {
		out := imageio.NewRGB(m.W, m.H)
		for i, v := range m.Pix {
			out.Pix[i] = uint8(math.Max(0, math.Min(255, math.Round(float64(v)+r.NormFloat64()*s))))
		}
		return out
	}},
	{"over", []float64{0.5, 1, 1.5, 2}, func(m *imageio.RGB, ev float64, _ *rand.Rand) *imageio.RGB {
		return mapLin(m, func(_ int, v float64) float64 { return v * math.Pow(2, ev) })
	}},
	{"under", []float64{0.5, 1, 2, 3}, func(m *imageio.RGB, ev float64, _ *rand.Rand) *imageio.RGB {
		return mapLin(m, func(_ int, v float64) float64 { return v * math.Pow(2, -ev) })
	}},
	{"warm", []float64{0.05, 0.1, 0.2, 0.35}, func(m *imageio.RGB, a float64, _ *rand.Rand) *imageio.RGB {
		return mapLin(m, func(c int, v float64) float64 { return v * []float64{1 + a, 1, 1 - a}[c] })
	}},
	{"cool", []float64{0.05, 0.1, 0.2, 0.35}, func(m *imageio.RGB, a float64, _ *rand.Rand) *imageio.RGB {
		return mapLin(m, func(c int, v float64) float64 { return v * []float64{1 - a, 1, 1 + a}[c] })
	}},
	{"green", []float64{0.05, 0.1, 0.2, 0.35}, func(m *imageio.RGB, a float64, _ *rand.Rand) *imageio.RGB {
		return mapLin(m, func(c int, v float64) float64 { return v * []float64{1 - a/2, 1 + a, 1 - a/2}[c] })
	}},
	{"flat", []float64{0.15, 0.3, 0.5, 0.7}, func(m *imageio.RGB, a float64, _ *rand.Rand) *imageio.RGB { // haze / low contrast
		out := imageio.NewRGB(m.W, m.H)
		for i, v := range m.Pix {
			out.Pix[i] = uint8(math.Round(float64(v)*(1-a) + 128*a))
		}
		return out
	}},
	{"jpeg", []float64{60, 30, 15, 8}, func(m *imageio.RGB, q float64, _ *rand.Rand) *imageio.RGB { return jpegRoundTrip(m, int(q)) }},
	{"blur+noise", []float64{1, 2, 4}, func(m *imageio.RGB, s float64, r *rand.Rand) *imageio.RGB {
		b := gaussBlur(m, s)
		for i, v := range b.Pix {
			b.Pix[i] = uint8(math.Max(0, math.Min(255, math.Round(float64(v)+r.NormFloat64()*8))))
		}
		return b
	}},
}

var metricHeader = []string{"sharpness", "sharpness_global", "brightness", "highlights", "shadows", "dynamic_range",
	"contrast", "cast_strength", "colorfulness", "noise", "focus_score", "exposure_score", "color_score", "noise_score", "technical", "naive_lap", "rating", "edge_cast", "focus_ratio", "blockiness", "lab_cast"}

func metricRow(m quality.Metrics, naive, rating, edge, blk float64) []string {
	return []string{ff(m.Sharpness), ff(m.SharpnessGlobal), ff(m.Brightness), ff(m.Highlights), ff(m.Shadows), ff(m.DynamicRange),
		ff(m.Contrast), ff(m.CastStrength), ff(m.Colorfulness), ff(m.Noise), ff(m.FocusScore), ff(m.ExposureScore),
		ff(m.ColorScore), ff(m.NoiseScore), ff(quality.Technical(rating, m, blk)), ff(naive), ff(rating), ff(edge), ff(m.FocusRatio), ff(blk), ff(m.LabCast())}
}

// naiveLaplacian is the textbook focus measure (no tiling, no noise
// correction) used as a baseline.
func naiveLaplacian(m *imageio.RGB) float64 {
	img := m
	if max(m.W, m.H) > quality.AnalysisSize {
		w, h := imageio.FitWithin(m.W, m.H, quality.AnalysisSize)
		img = imageio.Resize(m, w, h, imageio.Bilinear)
	}
	g := img.Gray()
	var s, s2 float64
	n := 0
	for y := 1; y < img.H-1; y++ {
		for x := 1; x < img.W-1; x++ {
			i := y*img.W + x
			v := float64(g[i-1] + g[i+1] + g[i-img.W] + g[i+img.W] - 4*g[i])
			s += v
			s2 += v * v
			n++
		}
	}
	mu := s / float64(n)
	return s2/float64(n) - mu*mu
}

func ladder(args []string) error {
	fs := flag.NewFlagSet("ladder", flag.ExitOnError)
	out := fs.String("out", "ladder.csv", "output")
	maxDim := fs.Int("max", 1600, "resize inputs so the long edge is at most this")
	only := fs.String("only", "", "comma-separated degradations to run (default all)")
	limit := fs.Int("n", 0, "use at most this many images")
	fs.Parse(args)
	sel := map[string]bool{}
	for _, d := range strings.Split(*only, ",") {
		if d != "" {
			sel[d] = true
		}
	}
	imgs := listImages(fs.Args())
	if *limit > 0 && len(imgs) > *limit {
		imgs = imgs[:*limit]
	}
	w, done := csvOut(*out, append([]string{"image", "degradation", "level"}, metricHeader...))
	defer done()
	var mu sync.Mutex
	parallel(imgs, 4, func(p string) {
		d, err := imageio.Load(p, *maxDim)
		if err != nil {
			return
		}
		r := rand.New(rand.NewSource(int64(len(p))))
		id := filepath.Base(p)
		rows := [][]string{append([]string{id, "none", "0"}, metricRow(quality.Analyze(d.Img), naiveLaplacian(d.Img), quality.Rate(d.Img), quality.GrayEdgeCast(d.Img), imageio.BlockinessRGB(d.Img))...)}
		for _, dg := range degradations {
			if len(sel) > 0 && !sel[dg.name] {
				continue
			}
			for _, lv := range dg.levels {
				img := dg.apply(d.Img, lv, r)
				rows = append(rows, append([]string{id, dg.name, ff(lv)}, metricRow(quality.Analyze(img), naiveLaplacian(img), quality.Rate(img), quality.GrayEdgeCast(img), imageio.BlockinessRGB(img))...))
			}
		}
		mu.Lock()
		w.WriteAll(rows)
		mu.Unlock()
	})
	return nil
}

func qualityCmd(args []string) error {
	fs := flag.NewFlagSet("quality", flag.ExitOnError)
	out := fs.String("out", "quality.csv", "output")
	fs.Parse(args)
	w, done := csvOut(*out, append([]string{"image"}, metricHeader...))
	defer done()
	var mu sync.Mutex
	parallel(listImages(fs.Args()), 4, func(p string) {
		d, err := imageio.Load(p, indexer.WorkSize)
		if err != nil {
			return
		}
		row := append([]string{filepath.Base(p)}, metricRow(quality.Analyze(d.Img), naiveLaplacian(d.Img), quality.Rate(d.Img), quality.GrayEdgeCast(d.Img), d.Blockiness)...)
		mu.Lock()
		w.Write(row)
		mu.Unlock()
	})
	return nil
}

// clipCmd writes the CLIP image embedding of every image (name, 512 floats).
func clipCmd(args []string) error {
	fs := flag.NewFlagSet("clip", flag.ExitOnError)
	models := fs.String("models", "", "models dir")
	out := fs.String("out", "clip.csv", "output")
	workers := fs.Int("workers", 4, "parallel images")
	fs.Parse(args)
	o := initORT()
	c, err := ml.NewCLIP(filepath.Join(*models, "clip"), o)
	if err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	var mu sync.Mutex
	parallel(listImages(fs.Args()), *workers, func(p string) {
		d, err := imageio.Load(p, indexer.WorkSize)
		if err != nil {
			return
		}
		e, err := c.EmbedImage(d.Img)
		if err != nil {
			return
		}
		row := []string{filepath.Base(p)}
		for _, v := range e {
			row = append(row, strconv.FormatFloat(float64(v), 'g', 5, 32))
		}
		mu.Lock()
		w.Write(row)
		mu.Unlock()
	})
	return nil
}

// synthCmd fills a catalogue with random photos and faces to measure how
// the UI and queries scale. Embeddings are random unit vectors.
func synthCmd(args []string) error {
	fs := flag.NewFlagSet("synth", flag.ExitOnError)
	data := fs.String("data", "", "data dir to create")
	nPhotos := fs.Int("photos", 1000000, "photos")
	facesPer := fs.Float64("faces", 1.5, "mean faces per photo")
	fs.Parse(args)
	os.MkdirAll(*data, 0o755)
	st, err := store.Open(indexer.Layout{Dir: *data}.DB()) // adopts a pre-rename photodex.db
	if err != nil {
		return err
	}
	defer st.Close()
	r := rand.New(rand.NewSource(1))
	vec := func() []byte {
		v := make([]float32, 512)
		var s float64
		for i := range v {
			v[i] = float32(r.NormFloat64())
			s += float64(v[i] * v[i])
		}
		for i := range v {
			v[i] /= float32(math.Sqrt(s))
		}
		return vindex.EncodeF16(v)
	}
	cams := []string{"Canon EOS 5D Mark IV", "Nikon D850", "Sony ILCE-7RM3", "Fujifilm X-T4", "iPhone 13"}
	tags := []string{"portrait", "crowd", "protest", "sports", "landscape", "street", "speech", "night"}
	var batch []*store.Result
	for i := 0; i < *nPhotos; i++ {
		nf := int(r.ExpFloat64() * *facesPer)
		p := store.Photo{
			Path: fmt.Sprintf("/archive/%04d/%02d/IMG_%07d.CR2", 2000+i%25, 1+i%12, i), Size: int64(20e6 + r.Intn(10e6)),
			MTime: int64(1e9 + i), Fingerprint: fmt.Sprintf("%032x", i), Format: "raw:cr2", Width: 6000, Height: 4000,
			TakenAt: int64(946684800 + r.Intn(25*365*86400)), CameraMake: "", CameraModel: cams[i%len(cams)],
			Sharpness: r.Float64() * 800, FocusScore: r.Float64(), ExposureScore: r.Float64(), ColorScore: r.Float64(),
			NoiseScore: r.Float64(), Technical: r.Float64() * 100, Overall: r.Float64() * 100, Brightness: r.Float64(),
			CastStrength: r.Float64() * 15, FaceCount: nf, CLIP: vec(), IndexedAt: 1, Version: store.SchemaVersion,
		}
		res := &store.Result{Photo: p, Tags: []store.Tag{{Tag: tags[r.Intn(len(tags))], Score: 0.5}}}
		for k := 0; k < nf; k++ {
			res.Faces = append(res.Faces, store.Face{Ordinal: k, X1: .1, Y1: .1, X2: .2, Y2: .2, Score: .8, WidthPx: 100, Embedding: vec()})
		}
		batch = append(batch, res)
		if len(batch) == 5000 {
			if err := st.Save(batch); err != nil {
				return err
			}
			batch = batch[:0]
			fmt.Fprintf(os.Stderr, "\r%d", i+1)
		}
	}
	if err := st.Save(batch); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	return nil
}

// ---- text search ----------------------------------------------------------

func textCmd(args []string) error {
	fs := flag.NewFlagSet("text", flag.ExitOnError)
	data := fs.String("data", "", "catalogue data dir")
	models := fs.String("models", "", "models dir")
	queries := fs.String("queries", "", "TSV: label<TAB>query")
	k := fs.Int("k", 100, "results per query")
	out := fs.String("out", "text.csv", "output")
	fs.Parse(args)
	o := initORT()
	c, err := ml.NewCLIP(filepath.Join(*models, "clip"), o)
	if err != nil {
		return err
	}
	cat, err := catalog.Open(*data, &indexer.Engines{CLIP: c})
	if err != nil {
		return err
	}
	defer cat.Close()
	b, err := os.ReadFile(*queries)
	if err != nil {
		return err
	}
	w, done := csvOut(*out, []string{"label", "query", "rank", "path", "score"})
	defer done()
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		label, q, _ := strings.Cut(l, "\t")
		hits, err := cat.SearchText(q, store.Filter{}, *k)
		if err != nil {
			return err
		}
		for i, h := range hits {
			w.Write([]string{label, q, strconv.Itoa(i + 1), h.Path, ff(float64(h.Score))})
		}
	}
	return nil
}

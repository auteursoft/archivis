// Command archivis indexes a photo archive for face search, natural-language
// search and objective quality ranking, and serves a local web UI over it.
package main

import (
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/web"
)

const usage = `Archivis — face search and quality ranking for large photo archives

Usage: archivis <command> [flags]

Commands:
  setup        download ONNX Runtime and the models (~800 MB, once)
  index        catalogue photos under one or more folders (incremental, resumable)
  serve        run the web interface (search, people, quality browsing)
  search       search from the terminal: text, --face IMAGE or --like IMAGE
  list         list photos matching quality/metadata filters (paths, csv or json)
  export       symlink or copy matching photos into a folder (e.g. for Lightroom)
  people       list | add NAME FACE_ID... | match | rename | merge | delete
  aesthetic    models | train | use ID[=W]... | import FILE | export ID
  retag        recompute category tags from stored embeddings
  prune        drop catalogue entries for files that no longer exist
  stats        catalogue summary
  errors       show files that failed to index
  inspect      show what Archivis reads from photo files (camera, lens, preview, rotation)
  users        web accounts: list | add-admin | invite | reset | password | role | disable | enable | signout

Global flags (all commands):
  --data DIR       data directory (default $ARCHIVIS_DATA or ~/.archivis)
  --provider P     cpu | coreml | cuda | directml | auto (default auto)
  --ort-lib PATH   path to the onnxruntime shared library

Run "archivis <command> -h" for command flags.
`

type globals struct {
	data     string
	provider string
	ortLib   string
	threads  int
}

func defaultData() string {
	if d := os.Getenv("ARCHIVIS_DATA"); d != "" {
		return d
	}
	if d := os.Getenv("PHOTODEX_DATA"); d != "" { // the name before Archivis
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".archivis")
}

// usesDefaultData reports whether this run relies on the default data
// directory: no --data flag and no data-directory variable.
func usesDefaultData(args []string) bool {
	if os.Getenv("ARCHIVIS_DATA") != "" || os.Getenv("PHOTODEX_DATA") != "" {
		return false
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if n := strings.TrimLeft(a, "-"); n != a && (n == "data" || strings.HasPrefix(n, "data=")) {
			return false
		}
	}
	return true
}

// adoptOldDataDir moves a catalogue made before the rename to Archivis
// (~/.photodex) to the new location, once. If the move fails it keeps
// using the old directory rather than starting an empty catalogue.
func adoptOldDataDir(old, dir string) string {
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	if _, err := os.Stat(old); err != nil {
		return dir
	}
	if err := os.Rename(old, dir); err != nil {
		fmt.Fprintf(os.Stderr, "note: using %s (could not rename it to %s: %v)\n", old, dir, err)
		return old
	}
	fmt.Fprintf(os.Stderr, "note: moved your catalogue from %s to %s\n", old, dir)
	return dir
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.data, "data", defaultData(), "data directory")
	fs.StringVar(&g.provider, "provider", "auto", "execution provider: cpu, coreml, cuda, directml, auto")
	fs.StringVar(&g.ortLib, "ort-lib", "", "onnxruntime shared library path")
	fs.IntVar(&g.threads, "threads", 0, "threads per model invocation (0 = automatic)")
}

func (g *globals) modelsDir() string { return filepath.Join(g.data, "models") }

func newFlagSet(name, help string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "archivis %s — %s\n\nFlags:\n", name, help)
		fs.PrintDefaults()
	}
	return fs
}

// parseInterleaved lets flags appear after positional arguments
// ("archivis index /photos --workers 8").
// parseFlags parses and then rejects non-finite numbers: Go's flag package
// accepts "NaN" and "Inf", and a NaN threshold makes every comparison false,
// silently disabling the check it controls.
func parseFlags(fs *flag.FlagSet, args []string) {
	fs.Parse(args)
	if err := finiteFlags(fs); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", fs.Name(), err)
		os.Exit(2)
	}
}

func finiteFlags(fs *flag.FlagSet) error {
	var bad error
	fs.Visit(func(f *flag.Flag) {
		if g, ok := f.Value.(flag.Getter); ok && bad == nil {
			if v, ok := g.Get().(float64); ok && (math.IsNaN(v) || math.IsInf(v, 0)) {
				bad = fmt.Errorf("-%s must be a finite number, not %v", f.Name, v)
			}
		}
	})
	return bad
}

func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		parseFlags(fs, args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type need struct{ faces, clip bool }

// engines loads the requested models.
func (g *globals) engines(n need, workers int) (*indexer.Engines, error) {
	lib := g.ortLib
	if lib == "" {
		if p := filepath.Join(g.data, "lib", ml.LibraryName()); fileExists(p) {
			lib = p
		}
	}
	threads := g.threads
	if threads <= 0 {
		threads = max(1, runtime.NumCPU()/max(1, workers))
	}
	opts := ml.Options{LibraryPath: lib, Provider: g.provider, IntraOpThreads: threads}
	if err := ml.Init(opts); err != nil {
		return nil, err
	}
	eng := &indexer.Engines{}
	var err error
	if n.faces {
		if eng.Faces, err = ml.NewFaceEngine(filepath.Join(g.modelsDir(), "buffalo_l"), opts); err != nil {
			return nil, fmt.Errorf("%w\n(run `archivis setup` first)", err)
		}
	}
	if n.clip {
		if eng.CLIP, err = ml.NewCLIP(filepath.Join(g.modelsDir(), "clip"), opts); err != nil {
			return nil, fmt.Errorf("%w\n(run `archivis setup` first)", err)
		}
		if eng.CLIP.HasText() {
			cats, err := indexer.LoadCategories(filepath.Join(g.data, "categories.txt"))
			if err != nil {
				return nil, err
			}
			if eng.Tagger, err = indexer.NewTagger(eng.CLIP, cats); err != nil {
				return nil, err
			}
		}
	}
	return eng, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	g := &globals{}
	cmd, args := splitCommand(os.Args[1:])
	cmds := map[string]func(*globals, []string) error{
		"setup": runSetup, "index": runIndex, "serve": runServe, "search": runSearch,
		"list": runList, "export": runExport, "people": runPeople, "retag": runRetag,
		"prune": runPrune, "stats": runStats, "errors": runErrors, "aesthetic": runAesthetic, "inspect": runInspect,
		"users": runUsers,
	}
	fn, ok := cmds[cmd]
	if cmd == "" {
		fmt.Print(usage)
		os.Exit(2)
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if usesDefaultData(args) {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".archivis")
		if got := adoptOldDataDir(filepath.Join(home, ".photodex"), dir); got != dir {
			os.Setenv("ARCHIVIS_DATA", got) // the move failed: keep using the old directory
		}
	}
	if err := fn(g, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// splitCommand finds the command word even when global flags come first
// ("archivis --data /x stats"), moving those flags after the command.
func splitCommand(argv []string) (string, []string) {
	valued := map[string]bool{"data": true, "provider": true, "ort-lib": true, "threads": true}
	var pre []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			return a, append(append([]string{}, argv[i+1:]...), pre...)
		}
		pre = append(pre, a)
		name := strings.TrimLeft(a, "-")
		if !strings.Contains(name, "=") && valued[name] && i+1 < len(argv) {
			i++
			pre = append(pre, argv[i])
		}
	}
	return "", pre
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "\nstopping after in-flight photos finish (Ctrl-C again to abort)…")
		cancel()
		<-ch
		os.Exit(130)
	}()
	return ctx, cancel
}

func runIndex(g *globals, args []string) error {
	fs := newFlagSet("index", "Catalogue photos under the given folders.")
	workers := fs.Int("workers", runtime.NumCPU(), "parallel photos in flight")
	force := fs.Bool("force", false, "re-analyse photos even if unchanged")
	retry := fs.Bool("retry-errors", false, "retry files that failed before")
	var excludes multiFlag
	fs.Var(&excludes, "exclude", "skip paths containing this text or dir names matching this glob (repeatable)")
	noFaces := fs.Bool("no-faces", false, "skip face detection")
	noMatch := fs.Bool("no-match", false, "don't auto-label people after indexing")
	minBytes := fs.Int64("min-bytes", 4096, "skip files smaller than this many bytes (icons, tiny thumbnails)")
	g.register(fs)
	roots := parseInterleaved(fs, args)
	if len(roots) == 0 {
		return fmt.Errorf("give at least one folder to index")
	}
	// A drive that isn't mounted shouldn't stop the others from being
	// indexed (e.g. a nightly job over several archive drives).
	var avail []string
	for _, r := range roots {
		if st, err := os.Stat(r); err != nil || !st.IsDir() {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: not a folder (drive not mounted?)\n", r)
			continue
		}
		avail = append(avail, r)
	}
	if len(avail) == 0 {
		return fmt.Errorf("none of the folders to index exist: %s", strings.Join(roots, ", "))
	}
	roots = avail
	if err := os.MkdirAll(g.data, 0o755); err != nil {
		return err
	}
	unlock, err := indexer.LockIndex(g.data)
	if err != nil {
		return err
	}
	defer unlock()
	eng, err := g.engines(need{faces: !*noFaces, clip: true}, *workers)
	if err != nil {
		return err
	}
	cat, err := catalog.Open(g.data, eng)
	if err != nil {
		return err
	}
	defer cat.Close()
	ctx, cancel := signalContext()
	defer cancel()
	ix := indexer.New(indexer.Config{
		Roots: roots, Exclude: excludes, Workers: *workers, Force: *force,
		RetryErrors: *retry, MinBytes: *minBytes, Progress: os.Stderr,
	}, cat.Store, eng, cat.Layout)
	fmt.Fprintf(os.Stderr, "indexing %s with %d workers (Ctrl-C to stop; rerun to resume)\n", strings.Join(roots, ", "), *workers)
	runErr := ix.Run(ctx)
	if runErr != nil && runErr != context.Canceled {
		return runErr
	}
	if !*noMatch && ctx.Err() == nil {
		// The photos are indexed either way; a failure here only means named
		// people were not matched, so say so and how to retry.
		if err := cat.Refresh(); err != nil {
			return fmt.Errorf("indexing finished, but loading the new faces for auto-labelling failed: %w (retry with `archivis people match`)", err)
		}
		n, err := cat.AutoMatch(catalog.DefaultMatch)
		if err != nil {
			return fmt.Errorf("indexing finished, but auto-labelling named people failed: %w (retry with `archivis people match`)", err)
		}
		if n > 0 {
			fmt.Fprintf(os.Stderr, "auto-labelled %d faces of named people\n", n)
		}
	}
	st, _ := cat.Store.Stats()
	fmt.Fprintf(os.Stderr, "catalogue: %d photos, %d faces, %d people\n", st.Photos, st.Faces, st.People)
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runServe(g *globals, args []string) error {
	fs := newFlagSet("serve", "Run the web interface.")
	addr := fs.String("addr", "127.0.0.1:8088", "listen address (use 0.0.0.0:8088 to expose on your network)")
	auth := fs.String("auth", cmp.Or(os.Getenv("ARCHIVIS_AUTH"), os.Getenv("PHOTODEX_AUTH")), "require HTTP basic auth, as user:password, until the first account exists (see archivis users; default $ARCHIVIS_AUTH, which keeps it out of the process list)")
	g.register(fs)
	parseFlags(fs, args)
	if u, p, ok := strings.Cut(*auth, ":"); *auth != "" && (!ok || u == "" || p == "") {
		return fmt.Errorf("--auth / ARCHIVIS_AUTH must be user:password")
	}
	eng, err := g.engines(need{faces: true, clip: true}, 1)
	if err != nil {
		return err
	}
	cat, err := catalog.Open(g.data, eng)
	if err != nil {
		return err
	}
	defer cat.Close()
	srv := web.New(cat)
	srv.Auth = *auth
	accounts, err := cat.Store.HasUsers()
	if err != nil {
		return err
	}
	if accounts && *auth != "" {
		fmt.Fprintln(os.Stderr, "note: accounts exist, so --auth / ARCHIVIS_AUTH is ignored; everyone signs in with their own account")
	}
	if !loopbackOnly(*addr) && *auth == "" && !accounts {
		fmt.Fprintf(os.Stderr, "warning: %s is reachable from other machines and --auth is not set; anyone on your network can browse your photos\n", *addr)
	}
	fmt.Fprintf(os.Stderr, "archivis: %d photos, %d faces loaded — open http://%s\n", cat.PhotoIndex().Len(), cat.FaceIndex().Len(), browseURL(*addr))
	return srv.ListenAndServe(*addr)
}

// explicitSort is the --sort order for search results, or "" (keep the
// similarity ranking) when --sort wasn't given: its default ("overall")
// suits browsing, not searching.
func explicitSort(fs *flag.FlagSet, f *store.Filter) string {
	set := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "sort" {
			set = true
		}
	})
	if !set {
		return ""
	}
	return f.Sort
}

// loopbackOnly reports whether a listen address accepts connections only
// from this machine. ":8088", "0.0.0.0:8088", "[::]:8088" and LAN addresses
// do not.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// browseURL is the address to open in a browser for a listen address.
func browseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// filterFlags registers the shared quality/metadata filters.
func filterFlags(fs *flag.FlagSet) *store.Filter {
	f := &store.Filter{}
	fs.Float64Var(&f.MinOverall, "min-overall", 0, "minimum overall score (0-100)")
	fs.Float64Var(&f.MinTechnical, "min-technical", 0, "minimum technical score (0-100)")
	fs.Float64Var(&f.MinFocus, "min-focus", 0, "minimum focus score (0-1)")
	fs.Float64Var(&f.MinExposure, "min-exposure", 0, "minimum exposure score (0-1)")
	fs.Float64Var(&f.MinAesthetic, "min-aesthetic", 0, "minimum aesthetic rating (0-10; most photos score 4-7.5)")
	fs.Float64Var(&f.MaxCast, "max-cast", 0, "maximum colour cast strength (e.g. 6)")
	fs.StringVar(&f.Camera, "camera", "", "camera make/model contains")
	fs.StringVar(&f.Lens, "lens", "", "lens contains")
	fs.StringVar(&f.PathContains, "path", "", "path contains")
	fs.StringVar(&f.Tag, "tag", "", "category tag (see `archivis stats`)")
	fs.StringVar(&f.Faces, "faces", "", "any | none | 1 | group")
	fs.StringVar(&f.Mono, "mono", "", "only | exclude black & white")
	fs.IntVar(&f.Year, "year", 0, "year taken")
	fs.StringVar(&f.Sort, "sort", "overall", "overall|technical|aesthetic|sharpness|newest|oldest|colorful|faces|worst|path")
	fs.IntVar(&f.Limit, "limit", 100, "maximum results (0 = all)")
	return f
}

func personFilter(cat *catalog.Catalog, name string, f *store.Filter) error {
	if name == "" {
		return nil
	}
	p, err := cat.Store.PersonByName(name)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("no person named %q", name)
	}
	f.PersonID = p.ID
	return nil
}

func runSearch(g *globals, args []string) error {
	fs := newFlagSet("search", `Search: archivis search "children playing in snow" | --face img.jpg | --like img.jpg`)
	faceImg := fs.String("face", "", "find photos of the person in this image (largest face)")
	likeImg := fs.String("like", "", "find photos that look like this image")
	minSim := fs.Float64("min-sim", 0.4, "minimum face similarity for --face")
	person := fs.String("person", "", "only photos of this named person")
	format := fs.String("format", "table", "table | paths | csv | json")
	f := filterFlags(fs)
	g.register(fs)
	text := strings.Join(parseInterleaved(fs, args), " ")
	if text == "" && *faceImg == "" && *likeImg == "" {
		return fmt.Errorf("give a text query, --face IMAGE or --like IMAGE")
	}
	eng, err := g.engines(need{faces: *faceImg != "", clip: true}, 1)
	if err != nil {
		return err
	}
	cat, err := catalog.Open(g.data, eng)
	if err != nil {
		return err
	}
	defer cat.Close()
	if err := personFilter(cat, *person, f); err != nil {
		return err
	}
	if f.Limit <= 0 {
		f.Limit = math.MaxInt32 // --limit 0: everything
	}
	switch {
	case *faceImg != "":
		q, err := cat.AnalyzeQueryFile(*faceImg)
		if err != nil {
			return err
		}
		if len(q.Analysis.Faces) == 0 {
			return fmt.Errorf("no face found in %s", *faceImg)
		}
		best := 0
		for i, fc := range q.Analysis.Faces {
			if fc.Width() > q.Analysis.Faces[best].Width() {
				best = i
			}
		}
		ph, err := cat.SearchFacePhotos(q.Analysis.Faces[best].Embedding, *f, f.Limit, float32(*minSim))
		if err != nil {
			return err
		}
		catalog.SortHits(ph, explicitSort(fs, f))
		return printPhotos(ph, *format)
	case *likeImg != "":
		q, err := cat.AnalyzeQueryFile(*likeImg)
		if err != nil {
			return err
		}
		hits, err := cat.SearchVector(q.Analysis.CLIP, *f, f.Limit)
		if err != nil {
			return err
		}
		catalog.SortHits(hits, explicitSort(fs, f))
		return printPhotos(hits, *format)
	default:
		hits, err := cat.SearchText(text, *f, f.Limit)
		if err != nil {
			return err
		}
		catalog.SortHits(hits, explicitSort(fs, f))
		return printPhotos(hits, *format)
	}
}

func runList(g *globals, args []string) error {
	fs := newFlagSet("list", "List photos by quality/metadata filters, e.g. --min-focus 0.7 --sort aesthetic")
	person := fs.String("person", "", "only photos of this named person")
	format := fs.String("format", "table", "table | paths | csv | json")
	f := filterFlags(fs)
	g.register(fs)
	parseFlags(fs, args)
	cat, err := catalog.Open(g.data, nil)
	if err != nil {
		return err
	}
	defer cat.Close()
	if err := personFilter(cat, *person, f); err != nil {
		return err
	}
	if f.Limit <= 0 {
		f.Limit = math.MaxInt32 // --limit 0: everything
	}
	ps, total, err := cat.Store.Browse(*f)
	if err != nil {
		return err
	}
	hits := make([]catalog.PhotoHit, len(ps))
	for i, p := range ps {
		hits[i] = catalog.PhotoHit{Photo: p}
	}
	// stderr, so `--format paths|csv|json` output stays clean for scripts
	if *format == "table" {
		fmt.Fprintf(os.Stderr, "%d of %d matching photos\n", len(ps), total)
	} else if total > len(ps)+f.Offset {
		fmt.Fprintf(os.Stderr, "note: %d photos match; listed %d (raise --limit, or 0 for all)\n", total, len(ps))
	}
	return printPhotos(hits, *format)
}

func runExport(g *globals, args []string) error {
	fs := newFlagSet("export", "Link or copy photos matching filters into a folder: archivis export --min-overall 75 --person Alice ./picks")
	person := fs.String("person", "", "only photos of this named person")
	query := fs.String("query", "", "natural-language query to rank by")
	cp := fs.Bool("copy", false, "copy files instead of symlinking")
	f := filterFlags(fs)
	g.register(fs)
	pos := parseInterleaved(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("give exactly one destination folder")
	}
	dest := pos[0]
	var eng *indexer.Engines
	var err error
	if *query != "" {
		if eng, err = g.engines(need{clip: true}, 1); err != nil {
			return err
		}
	}
	cat, err := catalog.Open(g.data, eng)
	if err != nil {
		return err
	}
	defer cat.Close()
	if err := personFilter(cat, *person, f); err != nil {
		return err
	}
	if f.Limit <= 0 {
		f.Limit = math.MaxInt32 // --limit 0: everything
	}
	var ps []store.Photo
	total := -1
	if *query != "" {
		hits, err := cat.SearchText(*query, *f, f.Limit)
		if err != nil {
			return err
		}
		catalog.SortHits(hits, explicitSort(fs, f))
		for _, h := range hits {
			ps = append(ps, h.Photo)
		}
	} else if ps, total, err = cat.Store.Browse(*f); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	n := 0
	for i, p := range ps {
		name := fmt.Sprintf("%04d_%s", i+1, filepath.Base(p.Path))
		ok, err := exportPhoto(p.Path, filepath.Join(dest, name), *cp)
		if err != nil {
			return err
		}
		if ok {
			n++
		}
	}
	fmt.Printf("exported %d photos to %s\n", n, dest)
	if total > len(ps) {
		fmt.Fprintf(os.Stderr, "note: %d photos match; only the first %d were exported (raise --limit, or 0 for all)\n", total, len(ps))
	}
	return nil
}

// exportPhoto links or copies src to target without ever writing through an
// existing name: a copy is written to a temporary file and renamed into
// place, which replaces a symlink rather than following it (a plain
// os.Create would truncate the original a previous export's link points
// to). A link from an earlier export is replaced by the copy; any other
// existing file is left alone and reported. It returns whether target now
// holds src.
func exportPhoto(src, target string, copyFile bool) (bool, error) {
	if st, err := os.Lstat(target); err == nil {
		link, _ := os.Readlink(target)
		isLink := st.Mode()&os.ModeSymlink != 0
		switch {
		case !copyFile && isLink && link == src:
			return true, nil // already exported
		case copyFile && isLink:
			// becomes a real copy below; the link itself is replaced
		default:
			fmt.Fprintf(os.Stderr, "skip: %s already exists (not overwriting)\n", target)
			return false, nil
		}
	}
	if !copyFile {
		return true, os.Symlink(src, target)
	}
	in, err := os.Open(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skip:", err)
		return false, nil
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".export-*")
	if err != nil {
		return false, err
	}
	_, err = io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), target)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	return true, nil
}

func printPhotos(hits []catalog.PhotoHit, format string) error {
	switch format {
	case "paths":
		for _, h := range hits {
			fmt.Println(h.Path)
		}
	case "csv":
		w := csv.NewWriter(os.Stdout)
		w.Write([]string{"path", "score", "overall", "technical", "aesthetic", "focus", "exposure", "color", "noise", "faces", "taken", "camera", "lens"})
		for _, h := range hits {
			w.Write([]string{h.Path, fmt.Sprintf("%.3f", h.Score), f1(h.Overall), f1(h.Technical), f2(h.Aesthetic.Float64),
				f2(h.FocusScore), f2(h.ExposureScore), f2(h.ColorScore), f2(h.NoiseScore), fmt.Sprint(h.FaceCount),
				takenStr(h.TakenAt), strings.TrimSpace(h.CameraMake + " " + h.CameraModel), h.Lens})
		}
		w.Flush()
	case "json":
		type row struct {
			Path      string   `json:"path"`
			Score     float32  `json:"score,omitempty"`
			Overall   float64  `json:"overall"`
			Technical float64  `json:"technical"`
			Aesthetic *float64 `json:"aesthetic,omitempty"`
			Focus     float64  `json:"focus"`
			Exposure  float64  `json:"exposure"`
			Color     float64  `json:"color"`
			Noise     float64  `json:"noise"`
			Faces     int      `json:"faces"`
			Taken     string   `json:"taken,omitempty"`
			Camera    string   `json:"camera,omitempty"`
		}
		var rows []row
		for _, h := range hits {
			r := row{Path: h.Path, Score: h.Score, Overall: h.Overall, Technical: h.Technical, Focus: h.FocusScore,
				Exposure: h.ExposureScore, Color: h.ColorScore, Noise: h.NoiseScore, Faces: h.FaceCount,
				Taken: takenStr(h.TakenAt), Camera: strings.TrimSpace(h.CameraMake + " " + h.CameraModel)}
			if h.Aesthetic.Valid {
				a := h.Aesthetic.Float64
				r.Aesthetic = &a
			}
			rows = append(rows, r)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	default:
		for _, h := range hits {
			score := ""
			if h.Score != 0 {
				score = fmt.Sprintf("%.3f  ", h.Score)
			}
			fmt.Printf("%soverall %5.1f  focus %.2f  exp %.2f  %s  %s\n", score, h.Overall, h.FocusScore, h.ExposureScore, takenStr(h.TakenAt), h.Path)
		}
	}
	return nil
}

func f1(v float64) string { return fmt.Sprintf("%.1f", v) }
func f2(v float64) string { return fmt.Sprintf("%.2f", v) }

func takenStr(t int64) string {
	if t <= 0 {
		return "----------"
	}
	return time.Unix(t, 0).UTC().Format("2006-01-02")
}

func runRetag(g *globals, args []string) error {
	fs := newFlagSet("retag", "Recompute category tags (edit DATA/categories.txt to customise categories).")
	g.register(fs)
	parseFlags(fs, args)
	eng, err := g.engines(need{clip: true}, 1)
	if err != nil {
		return err
	}
	if eng.Tagger == nil {
		return fmt.Errorf("CLIP text model missing")
	}
	cat, err := catalog.Open(g.data, eng)
	if err != nil {
		return err
	}
	defer cat.Close()
	tags := map[int64][]store.Tag{}
	n := 0
	var writeErr error
	_, err = cat.Store.EachEmbedding(false, 0, func(id int64, b []byte) {
		if writeErr != nil {
			return // a batch failed: stop writing, report it below
		}
		tags[id] = eng.Tagger.Tags(decodeF16(b))
		if len(tags) >= 5000 {
			if writeErr = cat.Store.ReplaceTags(tags); writeErr == nil {
				n += len(tags)
			}
			tags = map[int64][]store.Tag{}
		}
	})
	if err != nil {
		return err
	}
	if writeErr != nil {
		return fmt.Errorf("retagging stopped after %d photos: %w", n, writeErr)
	}
	if err := cat.Store.ReplaceTags(tags); err != nil {
		return err
	}
	n += len(tags)
	fmt.Printf("retagged %d photos\n", n)
	return nil
}

func runPrune(g *globals, args []string) error {
	fs := newFlagSet("prune", "Remove catalogue entries for deleted files under the given folders.\n"+
		"Folders whose files are all missing and that are themselves gone or empty look\n"+
		"like an offline drive and are kept unless --all-missing is given.")
	dry := fs.Bool("dry-run", false, "only list what would be removed")
	allMissing := fs.Bool("all-missing", false, "also prune folders whose files are all gone (you deleted them; the drive is not just offline)")
	g.register(fs)
	roots := parseInterleaved(fs, args)
	if len(roots) == 0 {
		return fmt.Errorf("give the folder(s) to prune (only entries under them are checked)")
	}
	st, err := store.Open(indexer.Layout{Dir: g.data}.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.Prune(roots, *dry, *allMissing)
	if err != nil {
		return err
	}
	for _, p := range res.Gone {
		fmt.Println(p)
	}
	verb := "removed"
	if *dry {
		verb = "would remove"
	}
	fmt.Fprintf(os.Stderr, "%s %d entries\n", verb, len(res.Gone))
	if len(res.Held) > 0 {
		fmt.Fprintf(os.Stderr, "kept %d folder(s) whose files are all missing and that are gone or empty — an offline drive looks the same:\n", len(res.Held))
		for _, d := range res.Held {
			fmt.Fprintf(os.Stderr, "  %s\n", d)
		}
		fmt.Fprintln(os.Stderr, "if you deleted them, rerun with --all-missing")
	}
	return nil
}

func runStats(g *globals, args []string) error {
	fs := newFlagSet("stats", "Catalogue summary.")
	g.register(fs)
	parseFlags(fs, args)
	st, err := store.Open(indexer.Layout{Dir: g.data}.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := st.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("photos   %d\nfaces    %d\npeople   %d (%d labelled faces)\nerrors   %d\n", s.Photos, s.Faces, s.People, s.Labeled, s.Errors)
	tags, _ := st.TagCounts()
	if len(tags) > 0 {
		fmt.Println("\ntags:")
		for _, t := range tags {
			fmt.Printf("  %-14s %d\n", t.Tag, int(t.Score))
		}
	}
	return nil
}

func runErrors(g *globals, args []string) error {
	fs := newFlagSet("errors", "List files that failed to index (retry with index --retry-errors).")
	limit := fs.Int("limit", 100, "how many")
	g.register(fs)
	parseFlags(fs, args)
	st, err := store.Open(indexer.Layout{Dir: g.data}.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	errs, err := st.Errors(*limit)
	if err != nil {
		return err
	}
	for _, e := range errs {
		fmt.Printf("%s\t%s\n", e[0], e[1])
	}
	return nil
}

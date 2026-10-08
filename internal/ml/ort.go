// Package ml wraps the ONNX models used by archivis: SCRFD face detection,
// ArcFace face recognition and CLIP image/text embeddings, plus the aesthetic
// head (a linear model on CLIP embeddings).
package ml

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// Options control how ONNX Runtime sessions are created.
type Options struct {
	// Path to the onnxruntime shared library. Empty = search common locations.
	LibraryPath string
	// Execution provider: "cpu", "coreml", "cuda", "directml" or "auto".
	Provider string
	// Threads used inside a single model invocation. With many parallel
	// workers, 1 is usually the best throughput on CPU.
	IntraOpThreads int
}

var (
	initOnce sync.Once
	initErr  error
)

// Init loads the ONNX Runtime shared library. Safe to call more than once.
func Init(opts Options) error {
	initOnce.Do(func() {
		lib := opts.LibraryPath
		if lib == "" {
			lib = FindLibrary()
		}
		if lib == "" {
			initErr = errors.New("onnxruntime shared library not found; run `archivis setup` or pass --ort-lib / set ONNXRUNTIME_LIB")
			return
		}
		ort.SetSharedLibraryPath(lib)
		if err := ort.InitializeEnvironment(); err != nil {
			initErr = fmt.Errorf("initialising onnxruntime from %s: %w", lib, err)
		}
	})
	return initErr
}

// LibraryName is the platform-specific file name of the ORT shared library.
func LibraryName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libonnxruntime.dylib"
	case "windows":
		return "onnxruntime.dll"
	default:
		return "libonnxruntime.so"
	}
}

// FindLibrary searches the usual places for the ORT shared library.
func FindLibrary() string {
	if p := os.Getenv("ONNXRUNTIME_LIB"); p != "" {
		return p
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "lib"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".archivis", "lib"))
	}
	dirs = append(dirs, "/opt/homebrew/lib", "/usr/local/lib", "/usr/lib", "/usr/lib/x86_64-linux-gnu", "/usr/lib/aarch64-linux-gnu")
	for _, d := range dirs {
		if p := LibraryIn(d); p != "" {
			return p
		}
	}
	return ""
}

// LibraryIn returns the ORT runtime library in dir, or "". It prefers the
// canonical name and otherwise accepts a versioned one (libonnxruntime.1.29.0.dylib,
// libonnxruntime.so.1.29.0). Only regular files count (symlinks are followed),
// so the macOS libonnxruntime.*.dylib.dSYM debug bundle -- a directory -- and
// provider plug-ins are never picked.
func LibraryIn(dir string) string {
	name := LibraryName()
	isLib := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && st.Mode().IsRegular()
	}
	if p := filepath.Join(dir, name); isLib(p) {
		return p
	}
	// Versioned names only: libonnxruntime.1.29.0.dylib (macOS) or
	// libonnxruntime.so.1.29.0 (Linux). Backups (libonnxruntime.so.bak),
	// provider plug-ins and debug bundles do not match.
	ext := filepath.Ext(name) // .dylib, .so, .dll
	stem := strings.TrimSuffix(name, ext)
	versioned := regexp.MustCompile(`^(` + regexp.QuoteMeta(stem) + `(\.\d+)+` + regexp.QuoteMeta(ext) +
		`|` + regexp.QuoteMeta(name) + `(\.\d+)+)$`)
	m, _ := filepath.Glob(filepath.Join(dir, stem+"*"))
	for _, c := range m {
		if versioned.MatchString(filepath.Base(c)) && isLib(c) {
			return c
		}
	}
	return ""
}

func newSessionOptions(o Options) (*ort.SessionOptions, error) {
	so, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	threads := o.IntraOpThreads
	if threads <= 0 {
		threads = 1
	}
	if err := so.SetIntraOpNumThreads(threads); err != nil {
		so.Destroy()
		return nil, err
	}
	so.SetInterOpNumThreads(1)
	prov := strings.ToLower(o.Provider)
	if prov == "auto" {
		switch runtime.GOOS {
		case "darwin":
			prov = "coreml"
		default:
			prov = "cpu"
		}
	}
	switch prov {
	case "", "cpu":
	case "coreml":
		if err := so.AppendExecutionProviderCoreMLV2(map[string]string{"MLComputeUnits": "ALL"}); err != nil {
			if err := so.AppendExecutionProviderCoreML(0); err != nil {
				fmt.Fprintf(os.Stderr, "warning: CoreML unavailable (%v); using CPU\n", err)
			}
		}
	case "cuda":
		cu, err := ort.NewCUDAProviderOptions()
		if err == nil {
			err = so.AppendExecutionProviderCUDA(cu)
			cu.Destroy()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: CUDA unavailable (%v); using CPU\n", err)
		}
	case "directml":
		if err := so.AppendExecutionProviderDirectML(0); err != nil {
			fmt.Fprintf(os.Stderr, "warning: DirectML unavailable (%v); using CPU\n", err)
		}
	default:
		so.Destroy()
		return nil, fmt.Errorf("unknown execution provider %q", o.Provider)
	}
	return so, nil
}

// session wraps a dynamic ORT session with its I/O metadata.
type session struct {
	s       *ort.DynamicAdvancedSession
	inputs  []ort.InputOutputInfo
	outputs []ort.InputOutputInfo
}

func openSession(path string, o Options) (*session, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("model %s: %w", path, err)
	}
	ins, outs, err := ort.GetInputOutputInfo(path)
	if err != nil {
		return nil, fmt.Errorf("reading model %s: %w", path, err)
	}
	so, err := newSessionOptions(o)
	if err != nil {
		return nil, err
	}
	defer so.Destroy()
	inNames := make([]string, len(ins))
	for i, x := range ins {
		inNames[i] = x.Name
	}
	outNames := make([]string, len(outs))
	for i, x := range outs {
		outNames[i] = x.Name
	}
	s, err := ort.NewDynamicAdvancedSession(path, inNames, outNames, so)
	if err != nil {
		return nil, fmt.Errorf("loading model %s: %w", path, err)
	}
	return &session{s: s, inputs: ins, outputs: outs}, nil
}

// run executes the session and returns every output as float32 data + shape.
func (s *session) run(inputs ...ort.Value) ([][]float32, []ort.Shape, error) {
	outs := make([]ort.Value, len(s.outputs))
	if err := s.s.Run(inputs, outs); err != nil {
		return nil, nil, err
	}
	data := make([][]float32, len(outs))
	shapes := make([]ort.Shape, len(outs))
	for i, v := range outs {
		if v == nil {
			continue
		}
		if t, ok := v.(*ort.Tensor[float32]); ok {
			data[i] = append([]float32(nil), t.GetData()...)
			shapes[i] = t.GetShape()
		}
		v.Destroy()
	}
	return data, shapes, nil
}

func (s *session) close() {
	if s != nil && s.s != nil {
		s.s.Destroy()
	}
}

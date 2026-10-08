package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/auteursoft/archivis/internal/ml"
)

// ortVersion must match the C API version compiled into onnxruntime_go.
const ortVersion = "1.29.0"

// ortSHA256 pins every ONNX Runtime archive setup may download: it is native
// code, so an archive that doesn't match is never unpacked. Bump with
// ortVersion (sha256sum of each release asset).
var ortSHA256 = map[string]string{
	"onnxruntime-osx-arm64-1.29.0.tgz":            "d0706fc34f315d8c88639d0a8c81f2e09e815f282cabed3493c06a054352cf92",
	"onnxruntime-linux-x64-1.29.0.tgz":            "c3fddc4f139a045b0c4902c57410f0694f1c2fdf9b6939fbe38b1aeae7cd14ba",
	"onnxruntime-linux-aarch64-1.29.0.tgz":        "e1799098ebc054b370f6176a450f158720f297818c613e5dc99b92e2ec82346f",
	"onnxruntime-win-x64-1.29.0.zip":              "c9b4b7086b529ad814f428c1bad028e20a25d7dc0699836775faace4ab5b78b2",
	"onnxruntime-linux-x64-gpu_cuda12-1.29.0.tgz": "4ca594a0da83927befbd73fe020d7f569be151d70bb4fe9741ad405f4882e2ad",
	"onnxruntime-win-x64-gpu_cuda12-1.29.0.zip":   "3526773ba1fef5849a3e9d18558d87bbbb75b0db3ddcde4f08694a746ac3169f",
}

type modelFile struct {
	dest   string // relative to models dir
	urls   []string
	sha256 string // required: every download is verified
	// fromZip extracts this member from a zip download instead of saving it.
	zipMember string
}

var modelFiles = []modelFile{
	{
		dest:      "buffalo_l/det_10g.onnx",
		urls:      []string{"https://github.com/deepinsight/insightface/releases/download/v0.7/buffalo_l.zip"},
		zipMember: "det_10g.onnx",
		sha256:    "5838f7fe053675b1c7a08b633df49e7af5495cee0493c7dcf6697200b85b5b91",
	},
	{
		dest:      "buffalo_l/w600k_r50.onnx",
		urls:      []string{"https://github.com/deepinsight/insightface/releases/download/v0.7/buffalo_l.zip"},
		zipMember: "w600k_r50.onnx",
		sha256:    "4c06341c33c2ca1f86781dab0e829f88ad5b64be9fba56e56bc9ebdefc619e43",
	},
	{
		dest: "clip/visual.onnx",
		urls: []string{
			"https://clip-as-service.s3.us-east-2.amazonaws.com/models-436c69702d61732d53657276696365/onnx/ViT-B-32/visual.onnx",
		},
		sha256: "06395063c0a5c28b1a8d4bd585261501a878c8f52d1216db6c4cbb651f7c13f1",
	},
	{
		dest: "clip/textual.onnx",
		urls: []string{
			"https://clip-as-service.s3.us-east-2.amazonaws.com/models-436c69702d61732d53657276696365/onnx/ViT-B-32/textual.onnx",
		},
		sha256: "0af04c287a3be2570eaef7a1ef896d81c1989602df67a8905941afed589e545e",
	},
}

// ortArchive names the release archive for a platform.
func ortArchive(goos, arch string, gpu bool) (string, error) {
	if gpu {
		switch goos + "/" + arch {
		case "linux/amd64":
			return "onnxruntime-linux-x64-gpu_cuda12-" + ortVersion + ".tgz", nil
		case "windows/amd64":
			return "onnxruntime-win-x64-gpu_cuda12-" + ortVersion + ".zip", nil
		}
		return "", fmt.Errorf("no prebuilt CUDA onnxruntime for %s/%s", goos, arch)
	}
	switch goos + "/" + arch {
	case "darwin/arm64":
		return "onnxruntime-osx-arm64-" + ortVersion + ".tgz", nil
	case "linux/amd64":
		return "onnxruntime-linux-x64-" + ortVersion + ".tgz", nil
	case "linux/arm64":
		return "onnxruntime-linux-aarch64-" + ortVersion + ".tgz", nil
	case "windows/amd64":
		return "onnxruntime-win-x64-" + ortVersion + ".zip", nil
	}
	return "", fmt.Errorf("no prebuilt onnxruntime for %s/%s; install ONNX Runtime %s yourself, run setup --skip-runtime, and pass --ort-lib", goos, arch, ortVersion)
}

func runSetup(g *globals, args []string) error {
	fs := newFlagSet("setup", "Download ONNX Runtime and the models into the data directory.")
	gpu := fs.Bool("cuda", false, "download the CUDA 12 build of ONNX Runtime (Linux/Windows x64)")
	skipORT := fs.Bool("skip-runtime", false, "don't download ONNX Runtime (use a system install)")
	g.register(fs)
	parseFlags(fs, args)

	if !*skipORT {
		if err := setupORT(g, *gpu); err != nil {
			return err
		}
	}
	zipCache := map[string]string{}
	defer func() {
		for _, p := range zipCache {
			os.Remove(p)
		}
	}()
	for _, m := range modelFiles {
		dest := filepath.Join(g.modelsDir(), m.dest)
		if ok, _ := verify(dest, m.sha256); ok {
			fmt.Printf("✓ %s\n", m.dest)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		var err error
		for _, u := range m.urls {
			if m.zipMember != "" {
				zp, ok := zipCache[u]
				if !ok {
					zp = dest + ".zip.part"
					if err = download(u, zp); err != nil {
						continue
					}
					zipCache[u] = zp
				}
				err = extractZipMember(zp, m.zipMember, dest)
			} else {
				err = download(u, dest)
			}
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", m.dest, err)
		}
		if ok, err := verify(dest, m.sha256); !ok {
			os.Remove(dest)
			return fmt.Errorf("%s: checksum mismatch (%v)", m.dest, err)
		}
		fmt.Printf("✓ %s\n", m.dest)
	}
	fmt.Printf("\nReady. Models in %s\nNext: archivis index /path/to/photos\n", g.modelsDir())
	return nil
}

func setupORT(g *globals, gpu bool) error {
	name, err := ortArchive(runtime.GOOS, runtime.GOARCH, gpu)
	if err != nil {
		return err
	}
	sum, ok := ortSHA256[name]
	if !ok {
		return fmt.Errorf("no pinned checksum for %s; install ONNX Runtime yourself and pass --ort-lib", name)
	}
	libDir := filepath.Join(g.data, "lib")
	want := ml.LibraryName()
	if ortInstalled(libDir, want) {
		fmt.Printf("✓ ONNX Runtime already in %s\n", libDir)
		return nil
	}
	url := "https://github.com/microsoft/onnxruntime/releases/download/v" + ortVersion + "/" + name
	tmp := filepath.Join(os.TempDir(), name)
	if err := download(url, tmp); err != nil {
		return fmt.Errorf("downloading ONNX Runtime: %w", err)
	}
	defer os.Remove(tmp)
	if ok, err := verify(tmp, sum); !ok {
		return fmt.Errorf("ONNX Runtime archive %s: checksum mismatch (%v); nothing was installed", name, err)
	}
	return installORT(tmp, name, libDir, want)
}

// ortInstalled reports whether the runtime library itself (not just a
// provider library left by an interrupted setup) is present and non-empty.
func ortInstalled(libDir, want string) bool {
	st, err := os.Stat(filepath.Join(libDir, want)) // follows the symlink
	return err == nil && st.Mode().IsRegular() && st.Size() > 1<<20
}

// isORTLib selects the shared libraries directly inside an archive's lib/
// directory. Nested files are skipped: the macOS .dSYM bundle holds a
// debug-only file with the same name as the real dylib.
func isORTLib(n string) bool {
	b := path.Base(n)
	return path.Base(path.Dir(n)) == "lib" && strings.Contains(b, "onnxruntime") &&
		(strings.Contains(b, ".so") || strings.HasSuffix(b, ".dylib") || strings.HasSuffix(b, ".dll"))
}

// installORT copies the shared libraries out of a verified archive.
func installORT(tmp, name, libDir, want string) error {
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return err
	}
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.OpenReader(tmp)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if isORTLib(f.Name) {
				rc, _ := f.Open()
				err := writeFile(filepath.Join(libDir, filepath.Base(f.Name)), rc)
				rc.Close()
				if err != nil {
					return err
				}
			}
		}
	} else {
		f, err := os.Open(tmp)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if !isORTLib(h.Name) {
				continue
			}
			dst := filepath.Join(libDir, filepath.Base(h.Name))
			switch h.Typeflag {
			case tar.TypeSymlink:
				os.Remove(dst)
				if err := os.Symlink(filepath.Base(h.Linkname), dst); err != nil {
					return err
				}
			case tar.TypeReg:
				if err := writeFile(dst, tr); err != nil {
					return err
				}
			}
		}
	}
	// Make sure the canonical name exists, pointing at the real library.
	if _, err := os.Stat(filepath.Join(libDir, want)); err != nil {
		if p := ml.LibraryIn(libDir); p != "" {
			os.Symlink(filepath.Base(p), filepath.Join(libDir, want))
		}
	}
	fmt.Printf("✓ ONNX Runtime %s in %s\n", ortVersion, libDir)
	return nil
}

func writeFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func download(url, dest string) error {
	fmt.Printf("↓ %s\n", url)
	client := &http.Client{Timeout: 60 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	part := dest + ".download"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	pr := &progressReader{r: resp.Body, total: resp.ContentLength, start: time.Now()}
	_, err = io.Copy(f, pr)
	fmt.Print("\r\033[K")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	// Git LFS pointer files are tiny text files: treat as failure.
	if st, _ := os.Stat(part); st != nil && st.Size() < 1024 {
		b, _ := os.ReadFile(part)
		if strings.Contains(string(b), "git-lfs") {
			os.Remove(part)
			return errors.New("got a Git LFS pointer instead of the file")
		}
	}
	return os.Rename(part, dest)
}

type progressReader struct {
	r     io.Reader
	total int64
	n     int64
	last  time.Time
	start time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()
		if p.total > 0 {
			fmt.Printf("\r  %5.1f%% of %d MB", float64(p.n)*100/float64(p.total), p.total>>20)
		} else {
			fmt.Printf("\r  %d MB", p.n>>20)
		}
	}
	return n, err
}

func extractZipMember(zipPath, member, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if filepath.Base(f.Name) == member {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFile(dest, rc)
		}
	}
	return fmt.Errorf("%s not found in archive", member)
}

func verify(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if want == "" {
		return false, errors.New("no pinned checksum")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return false, fmt.Errorf("sha256 %s, want %s", got, want)
	}
	return true, nil
}

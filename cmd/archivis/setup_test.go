package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The macOS archive ships lib/libonnxruntime.X.dylib.dSYM/.../DWARF/
// libonnxruntime.X.dylib after the real dylib; copying it would overwrite the
// library with debug symbols.
func TestORTLibFilter(t *testing.T) {
	for n, want := range map[string]bool{
		"./onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.1.29.0.dylib":                                                           true,
		"./onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.1.29.0.dylib.dSYM/Contents/Resources/DWARF/libonnxruntime.1.29.0.dylib": false,
		"onnxruntime-linux-x64-1.29.0/lib/libonnxruntime.so.1.29.0":                                                                true,
		"onnxruntime-linux-x64-1.29.0/lib/pkgconfig/libonnxruntime.pc":                                                             false,
		"onnxruntime-win-x64-1.29.0/lib/onnxruntime.dll":                                                                           true,
		"onnxruntime-win-x64-1.29.0/include/onnxruntime_c_api.h":                                                                   false,
	} {
		if isORTLib(n) != want {
			t.Errorf("%s: got %v", n, !want)
		}
	}
}

func TestORTChecksumsPinned(t *testing.T) {
	for _, pl := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}} {
		for _, gpu := range []bool{false, true} {
			name, err := ortArchive(pl[0], pl[1], gpu)
			if err != nil {
				continue // no prebuilt archive for this combination
			}
			if _, ok := ortSHA256[name]; !ok {
				t.Errorf("%v gpu=%v: no checksum for %s", pl, gpu, name)
			}
		}
	}
	p := filepath.Join(t.TempDir(), "a.tgz")
	os.WriteFile(p, []byte("tampered"), 0o644)
	if ok, _ := verify(p, ortSHA256["onnxruntime-linux-x64-1.29.0.tgz"]); ok {
		t.Fatal("tampered archive verified")
	}
}

func TestEveryModelPinned(t *testing.T) {
	for _, m := range modelFiles {
		if len(m.sha256) != 64 {
			t.Errorf("%s has no SHA-256", m.dest)
		}
	}
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("x"), 0o644)
	if ok, _ := verify(p, ""); ok {
		t.Fatal("a file without a pinned checksum verified")
	}
}

// An interrupted setup can leave only a provider library behind; that is not
// an installation.
func TestORTInstalledNeedsRuntimeLibrary(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "libonnxruntime_providers_shared.so"), make([]byte, 2<<20), 0o644)
	if ortInstalled(dir, "libonnxruntime.so") {
		t.Fatal("provider library alone counted as installed")
	}
	os.WriteFile(filepath.Join(dir, "libonnxruntime.so"), make([]byte, 100), 0o644)
	if ortInstalled(dir, "libonnxruntime.so") {
		t.Fatal("truncated runtime library counted as installed")
	}
	os.WriteFile(filepath.Join(dir, "libonnxruntime.so.1.29.0"), make([]byte, 2<<20), 0o644)
	os.Remove(filepath.Join(dir, "libonnxruntime.so"))
	os.Symlink("libonnxruntime.so.1.29.0", filepath.Join(dir, "libonnxruntime.so"))
	if !ortInstalled(dir, "libonnxruntime.so") {
		t.Fatal("complete install not recognised")
	}
}

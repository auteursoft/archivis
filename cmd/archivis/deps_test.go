package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manifest reads tools/deps/assets.txt: mirror name -> sha256, original URL.
func manifest(t *testing.T) map[string][2]string {
	t.Helper()
	f, err := os.Open("../../tools/deps/assets.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][2]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) != 3 || len(fs[0]) != 64 {
			t.Fatalf("assets.txt: bad line %q", line)
		}
		if _, dup := out[fs[1]]; dup {
			t.Fatalf("assets.txt: %s listed twice", fs[1])
		}
		out[fs[1]] = [2]string{fs[0], fs[2]}
	}
	return out
}

// Every file setup downloads is in the mirror manifest, from the same
// original, with the same checksum, and setup falls back to its mirror
// copy: adding a model or runtime without mirroring it fails here.
func TestDepsManifestMatchesSetup(t *testing.T) {
	m := manifest(t)
	for name, sum := range ortSHA256 {
		e, ok := m[name]
		if !ok || e[0] != sum {
			t.Errorf("ONNX Runtime %s: manifest has %v, setup pins %s", name, e, sum)
		}
		if urls := ortURLs(name); urls[0] != e[1] || urls[len(urls)-1] != mirrorBase+name {
			t.Errorf("ONNX Runtime %s: sources %v, manifest original %s", name, urls, e[1])
		}
	}
	for _, mf := range modelFiles {
		if len(mf.urls) < 2 {
			t.Errorf("%s: no mirror to fall back to: %v", mf.dest, mf.urls)
			continue
		}
		last := mf.urls[len(mf.urls)-1]
		name := strings.TrimPrefix(last, mirrorBase)
		e, ok := m[name]
		switch {
		case !strings.HasPrefix(last, mirrorBase) || !ok:
			t.Errorf("%s: last source %s is not a mirrored file", mf.dest, last)
		case e[1] != mf.urls[0]:
			t.Errorf("%s: original %s, manifest has %s", mf.dest, mf.urls[0], e[1])
		case mf.zipMember == "" && e[0] != mf.sha256:
			t.Errorf("%s: setup pins %s, manifest %s", mf.dest, mf.sha256, e[0])
		case mf.zipMember != "" && e[0] != mf.zipSHA256:
			t.Errorf("%s: setup pins the zip as %q, manifest %s", mf.dest, mf.zipSHA256, e[0])
		}
	}
}

// A source that is gone, or serves a different file, falls through to the
// next (the mirror), for plain files, zip members and ONNX Runtime.
func TestSetupFallsBackToMirror(t *testing.T) {
	good := []byte(strings.Repeat("model bytes ", 200))
	sum := sha256.Sum256(good)
	zipped := zipOf(t, map[string][]byte{"m.onnx": good})
	mux := http.NewServeMux()
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/changed", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("other ", 300))) })
	mux.HandleFunc("/changed.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipOf(t, map[string][]byte{"m.onnx": []byte(strings.Repeat("other ", 300))}))
	})
	// a different zip whose member still matches: not the pinned zip
	mux.HandleFunc("/repacked.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipOf(t, map[string][]byte{"m.onnx": good, "extra.txt": []byte("x")}))
	})
	mux.HandleFunc("/mirror/m.onnx", func(w http.ResponseWriter, r *http.Request) { w.Write(good) })
	mirrorZips := 0
	mux.HandleFunc("/mirror/b.zip", func(w http.ResponseWriter, r *http.Request) { mirrorZips++; w.Write(zipped) })
	zipSum := sha256.Sum256(zipped)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dir := t.TempDir()
	for _, c := range []struct {
		name string
		m    modelFile
	}{
		{"gone", modelFile{urls: []string{srv.URL + "/gone", srv.URL + "/mirror/m.onnx"}}},
		{"changed", modelFile{urls: []string{srv.URL + "/changed", srv.URL + "/mirror/m.onnx"}}},
		{"zip gone", modelFile{urls: []string{srv.URL + "/gone", srv.URL + "/mirror/b.zip"}, zipMember: "m.onnx"}},
		{"zip changed", modelFile{urls: []string{srv.URL + "/changed.zip", srv.URL + "/mirror/b.zip"}, zipMember: "m.onnx"}},
		{"zip repacked", modelFile{urls: []string{srv.URL + "/repacked.zip", srv.URL + "/mirror/b.zip"}, zipMember: "m.onnx"}},
	} {
		c.m.sha256 = hex.EncodeToString(sum[:])
		if c.m.zipMember != "" {
			c.m.zipSHA256 = hex.EncodeToString(zipSum[:])
		}
		before := mirrorZips
		dest := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_"))
		zc := map[string]string{}
		if err := fetchModel(c.m, dest, zc); err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if b, _ := os.ReadFile(dest); string(b) != string(good) {
			t.Errorf("%s: wrong file", c.name)
		}
		if c.m.zipMember != "" && mirrorZips != before+1 {
			t.Errorf("%s: the mirror's zip was not used", c.name)
		}
		for _, p := range zc {
			os.Remove(p)
		}
	}
	// every source failing is an error, and leaves nothing behind
	dest := filepath.Join(dir, "none")
	if err := fetchModel(modelFile{urls: []string{srv.URL + "/gone", srv.URL + "/changed"}, sha256: hex.EncodeToString(sum[:])}, dest, map[string]string{}); err == nil {
		t.Error("no good source, yet no error")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a mismatched file was left in place")
	}
	if err := fetchVerified([]string{srv.URL + "/changed", srv.URL + "/mirror/m.onnx"}, filepath.Join(dir, "ort"), hex.EncodeToString(sum[:])); err != nil {
		t.Errorf("fetchVerified: %v", err)
	}
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Two members of one archive whose original is down: the original is asked
// once, not once per member (each failure can be a long timeout), and both
// come from the mirror's archive, downloaded once.
func TestFailedArchiveSourceTriedOnce(t *testing.T) {
	a, b := []byte(strings.Repeat("first ", 200)), []byte(strings.Repeat("second ", 200))
	zipped := zipOf(t, map[string][]byte{"a.onnx": a, "b.onnx": b})
	zipSum := sha256.Sum256(zipped)
	downs, mirrors := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("/down.zip", func(w http.ResponseWriter, r *http.Request) { downs++; http.Error(w, "down", 503) })
	mux.HandleFunc("/mirror/x.zip", func(w http.ResponseWriter, r *http.Request) { mirrors++; w.Write(zipped) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dir := t.TempDir()
	zc := map[string]string{}
	for name, data := range map[string][]byte{"a.onnx": a, "b.onnx": b} {
		sum := sha256.Sum256(data)
		m := modelFile{urls: []string{srv.URL + "/down.zip", srv.URL + "/mirror/x.zip"}, zipMember: name,
			sha256: hex.EncodeToString(sum[:]), zipSHA256: hex.EncodeToString(zipSum[:])}
		if err := fetchModel(m, filepath.Join(dir, name), zc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if downs != 1 || mirrors != 1 {
		t.Fatalf("original asked %d times, mirror %d; want once each", downs, mirrors)
	}
	for _, p := range zc {
		if p != "" {
			os.Remove(p)
		}
	}
}

// A file already at dest that doesn't match its pin is replaced only by a
// verified download. If every source fails (one down, one serving another
// file), it is kept untouched, the error says so, and nothing is left
// beside it.
func TestSetupKeepsExistingFileWhenAllSourcesFail(t *testing.T) {
	good := []byte(strings.Repeat("model bytes ", 200))
	sum := sha256.Sum256(good)
	mux := http.NewServeMux()
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) })
	mux.HandleFunc("/other", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("other ", 300))) })
	mux.HandleFunc("/good", func(w http.ResponseWriter, r *http.Request) { w.Write(good) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dir := t.TempDir()
	dest := filepath.Join(dir, "visual.onnx")
	old := []byte("an older export, not the pinned one")
	if err := os.WriteFile(dest, old, 0o644); err != nil {
		t.Fatal(err)
	}
	m := modelFile{urls: []string{srv.URL + "/down", srv.URL + "/other"}, sha256: hex.EncodeToString(sum[:])}
	err := fetchModel(m, dest, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "kept the existing") {
		t.Fatalf("all sources failing: err = %v, want it to say the existing file was kept", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != string(old) {
		t.Fatalf("the existing file was changed: %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("left beside it: %v", names)
	}
	// a verified download replaces it
	m.urls = []string{srv.URL + "/other", srv.URL + "/good"}
	if err := fetchModel(m, dest, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != string(good) {
		t.Fatal("the verified download did not replace the old file")
	}
}

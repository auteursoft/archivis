package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var b bytes.Buffer
	if err := printVersion(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "archivis ") || !strings.Contains(out, runtime.Version()) || !strings.Contains(out, "file dated ") {
		t.Fatalf("version output:\n%s", out)
	}
}

func TestWantsVersion(t *testing.T) {
	for args, want := range map[string]bool{"--version": true, "-v": true, "--data /x --version": true, "--data /x": false, "": false} {
		if got := wantsVersion(strings.Fields(args)); got != want {
			t.Errorf("%q: %v", args, got)
		}
	}
}

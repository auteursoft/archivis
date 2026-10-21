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

package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

// versionInfo describes this binary: the git commit it was built from (Go
// records it when building in a git checkout), whether the checkout had
// uncommitted changes, and when the binary file was written.
func versionInfo() string {
	commit, when, dirty := "unknown", "", false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				commit = s.Value
				if len(commit) > 12 {
					commit = commit[:12]
				}
			case "vcs.time":
				if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
					when = t.UTC().Format("2006-01-02 15:04 UTC")
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	v := "archivis " + commit
	if dirty {
		v += " (with uncommitted changes)"
	}
	if when != "" {
		v += ", committed " + when
	}
	return v
}

func runVersion(g *globals, args []string) error {
	return printVersion(os.Stdout)
}

func printVersion(w io.Writer) error {
	fmt.Fprintln(w, versionInfo())
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			// the file's date, set when it was built or installed (install
			// sets it); the commit above is what identifies the build
			fmt.Fprintf(w, "binary %s, file dated %s\n", exe, st.ModTime().Format("2006-01-02 15:04:05 MST"))
		}
	}
	fmt.Fprintf(w, "%s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

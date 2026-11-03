package main

import (
	"flag"
	"testing"

	"github.com/auteursoft/archivis/internal/ml"
)

// --clip-provider and --face-provider move that model alone to another
// provider; without them both follow --provider.
func TestModelProviders(t *testing.T) {
	for _, c := range []struct {
		args        []string
		faces, clip string
	}{
		{nil, "auto", "auto"},
		{[]string{"--provider", "coreml-gpu"}, "coreml-gpu", "coreml-gpu"},
		{[]string{"--clip-provider", "cpu"}, "auto", "cpu"},
		{[]string{"--provider", "coreml-gpu", "--clip-provider", "cpu"}, "coreml-gpu", "cpu"},
		{[]string{"--face-provider", "coreml-gpu", "--clip-provider", "cpu"}, "coreml-gpu", "cpu"},
	} {
		var g globals
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		g.register(fs)
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		faces, clip := g.modelOptions(ml.Options{Provider: g.provider, IntraOpThreads: 3})
		if faces.Provider != c.faces || clip.Provider != c.clip {
			t.Errorf("%v: faces %q, clip %q; want %q, %q", c.args, faces.Provider, clip.Provider, c.faces, c.clip)
		}
		if faces.IntraOpThreads != 3 || clip.IntraOpThreads != 3 {
			t.Errorf("%v: the other options were not kept", c.args)
		}
	}
}

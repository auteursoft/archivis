package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/auteursoft/archivis/internal/store"
)

func TestSettingsCommand(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run := func(args ...string) (string, error) {
		var b bytes.Buffer
		err := settingsCmd(st, args, &b)
		return b.String(), err
	}
	if out, err := run(); err != nil || out != "burst_gap = 3\nscene_gap = 10\nsimilar_clip = 93\nstack_bursts = on\nstack_hamming = 10\n" {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err := run("set", "burst_gap", "5"); err != nil || out != "burst_gap = 5\n" {
		t.Fatalf("set: %q %v", out, err)
	}
	if out, _ := run("get", "burst_gap"); out != "5\n" {
		t.Fatalf("get: %q", out)
	}
	if out, err := run("set", "similar_clip", "90"); err != nil || out != "similar_clip = 90\n" || st.GetSettings().SimilarCLIP != 90 {
		t.Fatalf("set similar_clip: %q %v", out, err)
	}
	if _, err := run("set", "similar_clip", "70"); err == nil || st.GetSettings().SimilarCLIP != 90 {
		t.Fatalf("similar_clip 70 accepted: %v", err)
	}
	for _, bad := range [][]string{{"set", "burst_gap", "0"}, {"set", "burst_gap", "x"}, {"get", "nope"}, {"set", "nope", "1"}, {"frob"}} {
		if _, err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if got := st.GetSettings().BurstGap; got != 5 {
		t.Fatalf("a refused change altered the gap to %d", got)
	}
	if out, err := run("set", "stack_bursts", "off"); err != nil || out != "stack_bursts = off\n" || st.GetSettings().StackBursts {
		t.Fatalf("turning stacking off: %q %v", out, err)
	}
	if _, err := run("set", "stack_hamming", "17"); err == nil {
		t.Error("stack_hamming 17 accepted")
	}
}

package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSettings(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.GetSettings(); got != DefaultSettings {
		t.Fatalf("fresh catalogue: %+v, want defaults %+v", got, DefaultSettings)
	}
	v := st.GetSettings()
	if err := v.Set("burst_gap", "5"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSettings(v); err != nil {
		t.Fatal(err)
	}
	if got := st.GetSettings(); got.BurstGap != 5 {
		t.Fatalf("round trip: %+v", got)
	}
	if names := strings.Join(SettingNames(), " "); names != "burst_gap scene_gap stack_bursts stack_hamming" {
		t.Fatalf("settings offered: %v", names)
	}
	// stacking settings round-trip too
	v.StackBursts, v.StackHamming, v.SceneGap = false, 9, 0
	if err := st.SetSettings(v); err != nil {
		t.Fatal(err)
	}
	if got := st.GetSettings(); got.StackBursts || got.StackHamming != 9 || got.SceneGap != 0 {
		t.Fatalf("stacking round trip: %+v", got)
	}

	// out of range or malformed values are refused and change nothing
	for _, bad := range [][2]string{{"burst_gap", "0"}, {"burst_gap", "61"}, {"burst_gap", "three"},
		{"stack_hamming", "17"}, {"stack_hamming", "-1"}, {"scene_gap", "121"}, {"scene_gap", "-1"}, {"stack_bursts", "maybe"}, {"colour", "red"}} {
		w := v
		if err := w.Set(bad[0], bad[1]); err == nil {
			t.Errorf("%s=%s accepted", bad[0], bad[1])
		}
		if w != v {
			t.Errorf("%s=%s changed the settings to %+v", bad[0], bad[1], w)
		}
	}
	if err := st.SetSettings(Settings{BurstGap: 0}); err == nil {
		t.Error("SetSettings accepted an invalid gap")
	}
	// a damaged stored value falls back to the default
	st.SetMeta("setting_burst_gap", "lots")
	if got := st.GetSettings().BurstGap; got != DefaultSettings.BurstGap {
		t.Fatalf("damaged value read as %d", got)
	}
}

// Settings saved before scene stacking hold the old default similarity (6,
// for frames of one burst): they read as the new default until saved
// again; any other value, or a 6 saved since, is kept.
func TestOldDefaultSimilarityUpgrades(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for k, v := range map[string]string{"setting_burst_gap": "4", "setting_stack_bursts": "on", "setting_stack_hamming": "6"} {
		st.SetMeta(k, v)
	}
	got := st.GetSettings()
	if got.StackHamming != DefaultSettings.StackHamming || got.BurstGap != 4 || got.SceneGap != DefaultSettings.SceneGap {
		t.Fatalf("old settings read as %+v", got)
	}
	st.SetMeta("setting_stack_hamming", "8")
	if got := st.GetSettings().StackHamming; got != 8 {
		t.Fatalf("a chosen similarity read as %d", got)
	}
	v := st.GetSettings()
	v.StackHamming = 6
	if err := st.SetSettings(v); err != nil {
		t.Fatal(err)
	}
	if got := st.GetSettings().StackHamming; got != 6 {
		t.Fatalf("6 saved with scenes read as %d", got)
	}
}

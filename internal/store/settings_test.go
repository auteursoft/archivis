package store

import (
	"path/filepath"
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
	if names := SettingNames(); len(names) != 1 || names[0] != "burst_gap" {
		t.Fatalf("settings offered: %v", names)
	}

	// out of range or malformed values are refused and change nothing
	for _, bad := range [][2]string{{"burst_gap", "0"}, {"burst_gap", "61"}, {"burst_gap", "three"},
		{"stack_hamming", "17"}, {"stack_hamming", "-1"}, {"stack_bursts", "maybe"}, {"colour", "red"}} {
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

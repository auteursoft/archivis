package store

import (
	"fmt"
	"strconv"
	"strings"
)

// Settings are catalogue-wide options an admin can change (in the web
// interface or with `archivis settings`). They live in the meta table.
type Settings struct {
	// StackBursts collapses near-identical frames of a burst into one card
	// on the Photos page.
	StackBursts bool
	// BurstGap is the most seconds between frames of one burst.
	BurstGap int
	// StackHamming is how many bits two frames' perceptual hashes may
	// differ by and still count as the same picture.
	StackHamming int
}

// DefaultSettings are used for any setting never changed.
var DefaultSettings = Settings{StackBursts: true, BurstGap: 3, StackHamming: 6}

// Setting limits.
const (
	MinBurstGap, MaxBurstGap         = 1, 60
	MinStackHamming, MaxStackHamming = 0, 16
)

// settingKeys are the settings' names, as `archivis settings` and the meta
// table use them.
var settingKeys = []string{"burst_gap", "stack_bursts", "stack_hamming"}

// SettingNames lists the settings' names.
func SettingNames() []string { return append([]string(nil), settingKeys...) }

// GetSettings reads the settings, with defaults for any never set or
// unreadable.
func (s *Store) GetSettings() Settings {
	out := DefaultSettings
	for _, k := range settingKeys {
		if v := s.GetMeta("setting_" + k); v != "" {
			_ = out.Set(k, v) // a bad stored value keeps the default
		}
	}
	return out
}

// SetSettings validates and saves all the settings.
func (s *Store) SetSettings(v Settings) error {
	if err := v.Validate(); err != nil {
		return err
	}
	for _, k := range settingKeys {
		if err := s.SetMeta("setting_"+k, v.Get(k)); err != nil {
			return err
		}
	}
	return nil
}

// Validate reports whether every setting is within its limits.
func (v Settings) Validate() error {
	if v.BurstGap < MinBurstGap || v.BurstGap > MaxBurstGap {
		return fmt.Errorf("burst gap must be %d-%d seconds", MinBurstGap, MaxBurstGap)
	}
	if v.StackHamming < MinStackHamming || v.StackHamming > MaxStackHamming {
		return fmt.Errorf("stack similarity must be %d-%d bits", MinStackHamming, MaxStackHamming)
	}
	return nil
}

// Get returns one setting as text.
func (v Settings) Get(name string) string {
	switch name {
	case "stack_bursts":
		return map[bool]string{true: "on", false: "off"}[v.StackBursts]
	case "burst_gap":
		return strconv.Itoa(v.BurstGap)
	case "stack_hamming":
		return strconv.Itoa(v.StackHamming)
	}
	return ""
}

// Set changes one setting from text (on/off/true/false/1/0 for switches),
// checking its limits.
func (v *Settings) Set(name, value string) error {
	value = strings.TrimSpace(value)
	switch name {
	case "stack_bursts":
		switch strings.ToLower(value) {
		case "on", "true", "1", "yes":
			v.StackBursts = true
		case "off", "false", "0", "no":
			v.StackBursts = false
		default:
			return fmt.Errorf("stack_bursts must be on or off")
		}
		return nil
	case "burst_gap", "stack_hamming":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be a whole number", name)
		}
		next := *v
		if name == "burst_gap" {
			next.BurstGap = n
		} else {
			next.StackHamming = n
		}
		if err := next.Validate(); err != nil {
			return err
		}
		*v = next
		return nil
	}
	return fmt.Errorf("unknown setting %q (settings: %s)", name, strings.Join(settingKeys, ", "))
}

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/auteursoft/archivis/internal/catalog"
	"github.com/auteursoft/archivis/internal/store"
)

const settingsHelp = `Show or change catalogue settings (also on the web interface's Settings
page, for admins).
  settings                 list every setting and its value
  settings get NAME
  settings set NAME VALUE
Settings:
  burst_gap   most seconds between frames of one burst (1-60, default 3)`

func runSettings(g *globals, args []string) error {
	fs := newFlagSet("settings", settingsHelp)
	g.register(fs)
	pos := parseInterleaved(fs, args)
	cat, err := catalog.Open(g.data, nil)
	if err != nil {
		return err
	}
	defer cat.Close()
	return settingsCmd(cat.Store, pos, os.Stdout)
}

func settingsCmd(st *store.Store, pos []string, out io.Writer) error {
	known := func(name string) error {
		for _, n := range store.SettingNames() {
			if n == name {
				return nil
			}
		}
		return fmt.Errorf("unknown setting %q (settings: %s)", name, strings.Join(store.SettingNames(), ", "))
	}
	v := st.GetSettings()
	switch {
	case len(pos) == 0 || (len(pos) == 1 && pos[0] == "list"):
		for _, n := range store.SettingNames() {
			fmt.Fprintf(out, "%s = %s\n", n, v.Get(n))
		}
		return nil
	case len(pos) == 2 && pos[0] == "get":
		if err := known(pos[1]); err != nil {
			return err
		}
		fmt.Fprintln(out, v.Get(pos[1]))
		return nil
	case len(pos) == 3 && pos[0] == "set":
		if err := known(pos[1]); err != nil {
			return err
		}
		if err := v.Set(pos[1], pos[2]); err != nil {
			return err
		}
		if err := st.SetSettings(v); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s = %s\n", pos[1], v.Get(pos[1]))
		return nil
	}
	return fmt.Errorf("usage: archivis settings [list | get NAME | set NAME VALUE]")
}

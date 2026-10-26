package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/store"
)

func runRoots(g *globals, args []string) error {
	fs := newFlagSet("roots", `The indexed folders Albums starts from (each index run records the folders it indexes):
  roots list             list them
  roots add FOLDER...    add folders indexed before Albums existed
  roots remove FOLDER    stop showing a folder (its photos stay catalogued)
  roots name FOLDER NAME show a folder as NAME in Albums ("" for its own name;
                         FOLDER "iphone" names the iPhone album)`)
	g.register(fs)
	pos := parseInterleaved(fs, args)
	if len(pos) == 0 {
		pos = []string{"list"}
	}
	st, err := store.Open(indexer.Layout{Dir: g.data}.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	switch pos[0] {
	case "list":
		roots, err := st.Roots()
		if err != nil {
			return err
		}
		names, err := st.AlbumNames()
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			fmt.Fprintln(os.Stderr, "no folders recorded yet: the next index run records its folders, or run `archivis roots add FOLDER`")
		}
		for _, r := range roots {
			if n := names[r]; n != "" {
				fmt.Printf("%s  (shown as %q)\n", r, n)
			} else {
				fmt.Println(r)
			}
		}
		if n := names[store.IPhoneAlbum]; n != "" {
			fmt.Printf("iPhone album shown as %q\n", n)
		}
	case "add":
		if len(pos) < 2 {
			return fmt.Errorf("usage: roots add FOLDER...")
		}
		return st.RecordRoots(pos[1:])
	case "remove":
		if len(pos) != 2 {
			return fmt.Errorf("usage: roots remove FOLDER")
		}
		ok, err := st.RemoveRoot(pos[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s is not a recorded folder (see `archivis roots list`)", pos[1])
		}
	case "name":
		if len(pos) != 3 {
			return fmt.Errorf(`usage: roots name FOLDER "Display name" (or "" to clear)`)
		}
		album := pos[1]
		if strings.EqualFold(album, store.IPhoneAlbum) {
			album = store.IPhoneAlbum
		}
		ok, err := st.SetAlbumName(album, pos[2])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s is not a recorded folder (see `archivis roots list`)", pos[1])
		}
	default:
		return fmt.Errorf("unknown roots command %q (list | add | remove | name)", pos[0])
	}
	return nil
}

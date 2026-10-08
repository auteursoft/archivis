package main

import (
	"fmt"
	"strconv"

	"archivis/internal/catalog"
	"archivis/internal/vindex"
)

var decodeF16 = vindex.DecodeF16

func runPeople(g *globals, args []string) error {
	fs := newFlagSet("people", `Manage named people:
  people list
  people add NAME FACE_ID...      label faces (IDs are shown in the web UI)
  people match [--threshold 0.45] auto-label faces matching confirmed ones
  people rename OLD NEW           (merges if NEW exists)
  people merge INTO FROM
  people delete NAME`)
	threshold := fs.Float64("threshold", float64(catalog.DefaultMatch.Threshold), "minimum similarity for match")
	margin := fs.Float64("margin", float64(catalog.DefaultMatch.Margin), "required lead over the next person for match")
	g.register(fs)
	pos := parseInterleaved(fs, args)
	if len(pos) == 0 {
		pos = []string{"list"}
	}
	cat, err := catalog.Open(g.data, nil)
	if err != nil {
		return err
	}
	defer cat.Close()
	st := cat.Store
	lookup := func(name string) (int64, error) {
		p, err := st.PersonByName(name)
		if err != nil {
			return 0, err
		}
		if p == nil {
			return 0, fmt.Errorf("no person named %q", name)
		}
		return p.ID, nil
	}
	switch pos[0] {
	case "list":
		if len(pos) != 1 {
			return fmt.Errorf("usage: people list")
		}
		ps, err := st.People()
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			fmt.Println("no people yet — name faces in the web UI (archivis serve) or with `people add`")
		}
		for _, p := range ps {
			fmt.Printf("%-30s %6d photos (%d confirmed)  %d faces\n", p.Name, p.Photos, p.ManualPhotos, p.Faces)
		}
	case "add":
		if len(pos) < 3 {
			return fmt.Errorf("usage: people add NAME FACE_ID [FACE_ID ...]")
		}
		// parse everything first; LabelFaces then checks the ids exist and
		// changes nothing (creates no person) if any do not
		var faces []int64
		for _, s := range pos[2:] {
			f, err := strconv.ParseInt(s, 10, 64)
			if err != nil || f <= 0 {
				return fmt.Errorf("bad face id %q", s)
			}
			faces = append(faces, f)
		}
		if _, err := st.LabelFaces(pos[1], faces); err != nil {
			return err
		}
		fmt.Printf("labelled %d faces as %s\n", len(faces), pos[1])
	case "match":
		if len(pos) != 1 {
			return fmt.Errorf("usage: people match (it matches every named person; extra arguments are not supported)")
		}
		n, err := cat.AutoMatch(catalog.MatchOptions{Threshold: float32(*threshold), Margin: float32(*margin)})
		if err != nil {
			return err
		}
		fmt.Printf("auto-labelled %d faces\n", n)
	case "rename":
		if len(pos) != 3 {
			return fmt.Errorf("usage: people rename OLD NEW")
		}
		id, err := lookup(pos[1])
		if err != nil {
			return err
		}
		return st.RenamePerson(id, pos[2])
	case "merge":
		if len(pos) != 3 {
			return fmt.Errorf("usage: people merge INTO FROM")
		}
		into, err := lookup(pos[1])
		if err != nil {
			return err
		}
		from, err := lookup(pos[2])
		if err != nil {
			return err
		}
		return st.MergePeople(into, from)
	case "delete":
		if len(pos) != 2 {
			return fmt.Errorf("usage: people delete NAME")
		}
		id, err := lookup(pos[1])
		if err != nil {
			return err
		}
		return st.DeletePerson(id)
	default:
		return fmt.Errorf("unknown people subcommand %q", pos[0])
	}
	return nil
}

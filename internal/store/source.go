package store

import "strings"

// Where a photo came from, as far as its file tells (photos.source).
// SourceCamera: its metadata names the camera. Otherwise it is a saved
// image (sent, downloaded or exported, its metadata stripped), or, when it
// also looks like one, a meme or screenshot. The indexer works it out
// (indexer.Sources); 0 is not yet.
const (
	SourceUnknown = 0
	SourceCamera  = 1
	SourceSaved   = 2
	SourceMeme    = 3
)

// SourceInput is what working out a photo's source needs.
type SourceInput struct {
	ID     int64
	Camera bool   // its metadata names a camera
	CLIP   []byte // float16 image embedding (nil: none)
}

// SourceInputs returns up to n photos after id `after`, in id order: those
// whose source is not worked out yet, or every photo with all.
func (s *Store) SourceInputs(after int64, all bool, n int) ([]SourceInput, error) {
	cond := `p.source = 0 AND `
	if all {
		cond = ``
	}
	rows, err := s.DB.Query(`SELECT p.id, COALESCE(p.camera_make, '') != '' OR COALESCE(p.camera_model, '') != '', c.clip
		FROM photos p LEFT JOIN photo_clip c ON c.photo_id = p.id WHERE `+cond+`p.id > ? ORDER BY p.id LIMIT ?`, after, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceInput
	for rows.Next() {
		var in SourceInput
		if err := rows.Scan(&in.ID, &in.Camera, &in.CLIP); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// SetSources records photos' sources, in one short transaction. It does
// not change their seq: nothing else about them changed.
func (s *Store) SetSources(src map[int64]int) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`UPDATE photos SET source = ? WHERE id = ? AND source != ?`)
	if err != nil {
		return err
	}
	defer st.Close()
	for id, v := range src {
		if _, err := st.Exec(v, id, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MemeCount counts the memes and screenshots, all of them or those below
// the given folders, through the photos_memes index.
func (s *Store) MemeCount(dirs []string) (int, error) {
	q, args := `SELECT COUNT(*) FROM photos INDEXED BY photos_memes WHERE source = 3`, []any{}
	if len(dirs) > 0 {
		var or []string
		for _, d := range dirs {
			for _, r := range dirRanges(d) {
				or = append(or, "(path >= ? AND path < ?)")
				args = append(args, r[0], r[1])
			}
		}
		q += " AND (" + strings.Join(or, " OR ") + ")"
	}
	var n int
	err := s.DB.QueryRow(q, args...).Scan(&n)
	return n, err
}

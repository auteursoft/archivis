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
	Seq    int64  // the photo's seq when read: its source is set only if unchanged since
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
	rows, err := s.DB.Query(`SELECT p.id, p.seq, COALESCE(p.camera_make, '') != '' OR COALESCE(p.camera_model, '') != '', c.clip
		FROM photos p LEFT JOIN photo_clip c ON c.photo_id = p.id WHERE `+cond+`p.id > ? ORDER BY p.id LIMIT ?`, after, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceInput
	for rows.Next() {
		var in SourceInput
		if err := rows.Scan(&in.ID, &in.Seq, &in.Camera, &in.CLIP); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// SourceSet is a photo's worked-out source, from what it was at Seq.
type SourceSet struct {
	ID, Seq int64
	Source  int
}

// SetSources records photos' sources, in one short transaction: each only
// if the photo is unchanged since it was read (a photo re-indexed meanwhile
// already has its source from that analysis). It does not change their
// seq, as nothing else about them changed; a change marks the folders for
// working out again, as their covers leave memes out.
func (s *Store) SetSources(src []SourceSet) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`UPDATE photos SET source = ? WHERE id = ? AND seq = ? AND source != ?`)
	if err != nil {
		return err
	}
	defer st.Close()
	changed := int64(0)
	for _, x := range src {
		res, err := st.Exec(x.Source, x.ID, x.Seq, x.Source)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		changed += n
	}
	if changed > 0 {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value)
			VALUES ('folders_dirty', COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'folders_dirty'), 0) + 1)`); err != nil {
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

package store

import (
	"database/sql"
	"errors"
)

// MUSIQInput is what scoring an indexed photo with MUSIQ needs: its file,
// and the stored scores its technical and overall scores are worked out
// from (exposure and colour penalties, JPEG blockiness, aesthetics), as at
// change number Seq.
type MUSIQInput struct {
	ID, Seq                   int64
	Path                      string
	Size, MTime               int64 // the file as indexed (scoring checks it is still that file)
	ExposureScore, ColorScore float64
	Blockiness                float64
	Aesthetic                 sql.NullFloat64
}

// UnscoredMUSIQ returns up to n photos MUSIQ has not scored yet, with ids
// above after, in id order.
func (s *Store) UnscoredMUSIQ(after int64, n int) ([]MUSIQInput, error) {
	rows, err := s.DB.Query(`SELECT id, seq, path, size, mtime, COALESCE(exposure_score, 0), COALESCE(color_score, 0), blockiness, aesthetic
		FROM photos WHERE musiq IS NULL AND id > ? ORDER BY id LIMIT ?`, after, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MUSIQInput
	for rows.Next() {
		var p MUSIQInput
		if err := rows.Scan(&p.ID, &p.Seq, &p.Path, &p.Size, &p.MTime, &p.ExposureScore, &p.ColorScore, &p.Blockiness, &p.Aesthetic); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MUSIQCounts reports how many photos MUSIQ has scored and how many it has
// not.
func (s *Store) MUSIQCounts() (scored, unscored int, err error) {
	err = s.DB.QueryRow(`SELECT COUNT(musiq), COUNT(*) - COUNT(musiq) FROM photos`).Scan(&scored, &unscored)
	return scored, unscored, err
}

// MUSIQUpdate is a photo's MUSIQ score and the technical and overall scores
// it gives, worked out from the photo as it was at change number Seq.
type MUSIQUpdate struct {
	ID, Seq                   int64
	MUSIQ, Technical, Overall float64
}

// SetMUSIQ writes MUSIQ scores, skipping photos that changed since they
// were worked out (the indexer has scored those afresh), and reports how
// many it wrote. With overall set, each photo's overall score is worked out
// from its aesthetic score as it is when written (in the same transaction):
// the aesthetic blend may have been refreshed meanwhile, which does not
// change seq. Without, u.Overall is written. Groups and folders, which rank
// by these scores, are marked for working out again.
func (s *Store) SetMUSIQ(updates []MUSIQUpdate, overall OverallFunc) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`UPDATE photos SET musiq = ?, technical = ?, overall = ? WHERE id = ? AND seq = ?`)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	aes, err := tx.Prepare(`SELECT aesthetic FROM photos WHERE id = ? AND seq = ?`)
	if err != nil {
		return 0, err
	}
	defer aes.Close()
	n := 0
	for _, u := range updates {
		o := u.Overall
		if overall != nil {
			var a sql.NullFloat64
			switch err := aes.QueryRow(u.ID, u.Seq).Scan(&a); {
			case errors.Is(err, sql.ErrNoRows):
				continue // changed since: the indexer scored it afresh
			case err != nil:
				return 0, err
			}
			o = overall(u.Technical, a)
		}
		r, err := st.Exec(u.MUSIQ, u.Technical, o, u.ID, u.Seq)
		if err != nil {
			return 0, err
		}
		k, _ := r.RowsAffected()
		n += int(k)
	}
	if n > 0 {
		if err := markGroupsDirty(tx); err != nil { // groups and folders rank by these scores
			return 0, err
		}
	}
	return n, tx.Commit()
}

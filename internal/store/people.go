package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrNoPerson is returned when an operation names a person that does not exist.
var ErrNoPerson = errors.New("no such person")

// Person is a named identity.
type Person struct {
	ID           int64
	Name         string
	Photos       int // photos they are labelled in (manual + auto)
	ManualPhotos int // photos where a label of theirs is confirmed
	Faces        int // labelled faces; can exceed Photos (repeat detections, reflections)
	Manual       int // manually confirmed faces
	CoverFace    int64
}

// personCounts selects a person's photo and face counts; join faces as f.
const personCounts = `COUNT(DISTINCT f.photo_id),
			COUNT(DISTINCT CASE WHEN f.person_source = 'manual' THEN f.photo_id END),
			COUNT(f.id),
			COALESCE(SUM(f.person_source = 'manual'), 0)`

// People lists everyone with photo and face counts, most-photographed first.
func (s *Store) People() ([]Person, error) {
	rows, err := s.DB.Query(`
		SELECT p.id, p.name, ` + personCounts + `,
			COALESCE((SELECT f2.id FROM faces f2 WHERE f2.person_id = p.id
				ORDER BY (f2.person_source = 'manual') DESC, f2.width_px * f2.score * (1 - ABS(f2.yaw)) DESC LIMIT 1), 0)
		FROM people p LEFT JOIN faces f ON f.person_id = p.id
		GROUP BY p.id ORDER BY COUNT(DISTINCT f.photo_id) DESC, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name, &p.Photos, &p.ManualPhotos, &p.Faces, &p.Manual, &p.CoverFace); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PersonByName finds a person (case-insensitive).
func (s *Store) PersonByName(name string) (*Person, error) {
	var p Person
	err := s.DB.QueryRow(`SELECT id, name FROM people WHERE name = ?`, strings.TrimSpace(name)).Scan(&p.ID, &p.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// Person loads a person by id.
func (s *Store) Person(id int64) (*Person, error) {
	var p Person
	err := s.DB.QueryRow(`SELECT p.id, p.name, `+personCounts+`
		FROM people p LEFT JOIN faces f ON f.person_id = p.id
		WHERE p.id = ? GROUP BY p.id`, id).Scan(&p.ID, &p.Name, &p.Photos, &p.ManualPhotos, &p.Faces, &p.Manual)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// EnsurePerson returns the person with this name, creating them if needed.
func (s *Store) EnsurePerson(name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("empty name")
	}
	if p, err := s.PersonByName(name); err != nil || p != nil {
		if p != nil {
			return p.ID, nil
		}
		return 0, err
	}
	res, err := s.DB.Exec(`INSERT INTO people (name, created_at) VALUES (?, ?)`, name, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ErrNoFace is returned when a face id does not exist.
var ErrNoFace = errors.New("no such face")

// LabelFaces manually labels faces as the person called name, creating the
// person if needed, all in one transaction: if the name is empty or any face
// id does not exist, nothing is changed (in particular no empty person is
// left behind). It returns the person's id.
func (s *Store) LabelFaces(name string, faceIDs []int64) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("empty name")
	}
	if len(faceIDs) == 0 {
		return 0, errors.New("no faces given")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	uniq := map[int64]bool{}
	for _, id := range faceIDs {
		uniq[id] = true
	}
	ids := make([]int64, 0, len(uniq))
	for id := range uniq {
		ids = append(ids, id)
	}
	found := map[int64]bool{}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		rows, err := tx.Query(`SELECT id FROM faces WHERE id IN (`+placeholders(len(chunk))+`)`, int64sToAny(chunk)...)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			found[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}
	var missing []string
	for _, id := range faceIDs {
		if !found[id] {
			missing = append(missing, strconv.FormatInt(id, 10))
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%w: %s", ErrNoFace, strings.Join(missing, ", "))
	}
	var pid int64
	err = tx.QueryRow(`SELECT id FROM people WHERE name = ?`, name).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.Exec(`INSERT INTO people (name, created_at) VALUES (?, ?)`, name, time.Now().Unix())
		if err != nil {
			return 0, err
		}
		pid, _ = res.LastInsertId()
	} else if err != nil {
		return 0, err
	}
	st, err := tx.Prepare(`UPDATE faces SET person_id = ?, person_source = 'manual', person_sim = NULL WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	for _, id := range ids {
		if _, err := st.Exec(pid, id); err != nil {
			return 0, err
		}
	}
	return pid, tx.Commit()
}

// AssignFaces labels faces as a person. source is "manual" or "auto"; auto
// labels never overwrite manual ones.
func (s *Store) AssignFaces(personID int64, faceIDs []int64, source string, sims map[int64]float32) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := `UPDATE faces SET person_id = ?, person_source = ?, person_sim = ? WHERE id = ?`
	if source != "manual" {
		q += ` AND (person_source IS NULL OR person_source != 'manual')`
	}
	st, err := tx.Prepare(q)
	if err != nil {
		return err
	}
	for _, id := range faceIDs {
		var sim any
		if v, ok := sims[id]; ok {
			sim = float64(v)
		}
		if _, err := st.Exec(personID, source, sim, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnassignFaces clears labels from faces.
func (s *Store) UnassignFaces(faceIDs []int64) error {
	if len(faceIDs) == 0 {
		return nil
	}
	_, err := s.DB.Exec(`UPDATE faces SET person_id = NULL, person_source = NULL, person_sim = NULL WHERE id IN (`+placeholders(len(faceIDs))+`)`, int64sToAny(faceIDs)...)
	return err
}

// ClearAutoLabels removes automatic labels (optionally for one person).
func (s *Store) ClearAutoLabels(personID int64) error {
	q := `UPDATE faces SET person_id = NULL, person_source = NULL, person_sim = NULL WHERE person_source = 'auto'`
	var args []any
	if personID > 0 {
		q += ` AND person_id = ?`
		args = append(args, personID)
	}
	_, err := s.DB.Exec(q, args...)
	return err
}

// RenamePerson renames; if the new name exists the two people are merged.
func (s *Store) RenamePerson(id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("empty name")
	}
	other, err := s.PersonByName(name)
	if err != nil {
		return err
	}
	if other != nil && other.ID != id {
		return s.MergePeople(other.ID, id)
	}
	res, err := s.DB.Exec(`UPDATE people SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoPerson
	}
	return nil
}

// MergePeople moves all faces of from into into and deletes from.
func (s *Store) MergePeople(into, from int64) error {
	if into == from {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Both must exist: merging into a missing ID would orphan from's faces,
	// and merging from one would report a merge that never happened.
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM people WHERE id IN (?, ?)`, into, from).Scan(&n); err != nil {
		return err
	}
	if n != 2 {
		return ErrNoPerson
	}
	if _, err := tx.Exec(`UPDATE faces SET person_id = ? WHERE person_id = ?`, into, from); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM people WHERE id = ?`, from); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePerson removes a person; their faces become unlabelled.
func (s *Store) DeletePerson(id int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE faces SET person_id = NULL, person_source = NULL, person_sim = NULL WHERE person_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM people WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoPerson
	}
	return tx.Commit()
}

// LabeledFaces returns faces of a person; manualOnly restricts to confirmed.
func (s *Store) LabeledFaces(personID int64, manualOnly bool) ([]Face, error) {
	w := `WHERE person_id = ?`
	if manualOnly {
		w += ` AND person_source = 'manual'`
	}
	return s.queryFaces(w+` ORDER BY id`, personID)
}

// AllManualLabels returns (faceID -> personID) for every confirmed face.
func (s *Store) AllManualLabels() (map[int64]int64, error) {
	rows, err := s.DB.Query(`SELECT id, person_id FROM faces WHERE person_source = 'manual' AND person_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var f, p int64
		rows.Scan(&f, &p)
		out[f] = p
	}
	return out, rows.Err()
}

// UnlabeledOrAutoFaceIDs returns faces that automatic matching may (re)label.
func (s *Store) UnlabeledOrAutoFaceIDs() (map[int64]bool, error) {
	rows, err := s.DB.Query(`SELECT id FROM faces WHERE person_source IS NULL OR person_source != 'manual'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out[id] = true
	}
	return out, rows.Err()
}

// preserveLabels copies manual labels from an old version of a photo onto
// re-detected faces with overlapping boxes (IoU > 0.5).
func preserveLabels(tx *sql.Tx, path string, faces []Face) error {
	rows, err := tx.Query(`SELECT f.x1, f.y1, f.x2, f.y2, f.person_id FROM faces f JOIN photos p ON p.id = f.photo_id
		WHERE p.path = ? AND f.person_source = 'manual'`, path)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var o Face
		var pid int64
		if err := rows.Scan(&o.X1, &o.Y1, &o.X2, &o.Y2, &pid); err != nil {
			return err
		}
		best, bestIoU := -1, 0.5
		for i := range faces {
			if v := iou(&o, &faces[i]); v > bestIoU {
				best, bestIoU = i, v
			}
		}
		if best >= 0 {
			faces[best].PersonID = sql.NullInt64{Int64: pid, Valid: true}
			faces[best].PersonSource = sql.NullString{String: "manual", Valid: true}
		}
	}
	return rows.Err()
}

func iou(a, b *Face) float64 {
	w := min(a.X2, b.X2) - max(a.X1, b.X1)
	h := min(a.Y2, b.Y2) - max(a.Y1, b.Y1)
	if w <= 0 || h <= 0 {
		return 0
	}
	in := w * h
	return in / ((a.X2-a.X1)*(a.Y2-a.Y1) + (b.X2-b.X1)*(b.Y2-b.Y1) - in)
}

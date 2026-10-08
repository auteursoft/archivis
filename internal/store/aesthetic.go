package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// AestheticModel is one aesthetic model known to the catalogue. Models are
// never deleted: scores they produced stay attributed to them, so a
// catalogue keeps the history of how its photos were judged and can blend
// several models.
type AestheticModel struct {
	ID        string // name-<hash of the weights>: different weights, different ID
	Name      string
	Kind      string // builtin | user | legacy (scores only, weights unknown)
	Parent    string // model it was trained from, if any
	Model     []byte // the model as JSON (empty for legacy models)
	Active    bool   // contributes to photos.aesthetic
	Weight    float64
	Metrics   string // JSON: how it was evaluated when it was made
	CreatedAt int64
	Scored    int64 // photos it has scored
}

// AddAestheticModel registers a model. It reports whether it was new; an
// existing model is left unchanged.
func (s *Store) AddAestheticModel(m AestheticModel) (bool, error) {
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().Unix()
	}
	res, err := s.DB.Exec(`INSERT OR IGNORE INTO aesthetic_models (id, name, kind, parent, model, active, weight, metrics, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, m.ID, m.Name, m.Kind, nullStr(m.Parent), m.Model, m.Active, m.Weight, nullStr(m.Metrics), m.CreatedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// AestheticModels lists the registered models, oldest first, with how many
// photos each has scored.
func (s *Store) AestheticModels() ([]AestheticModel, error) {
	rows, err := s.DB.Query(`SELECT m.id, m.name, m.kind, COALESCE(m.parent, ''), m.model, m.active, m.weight,
		COALESCE(m.metrics, ''), m.created_at, (SELECT COUNT(*) FROM aesthetic_scores s WHERE s.model = m.id)
		FROM aesthetic_models m ORDER BY m.created_at, m.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AestheticModel
	for rows.Next() {
		var m AestheticModel
		if err := rows.Scan(&m.ID, &m.Name, &m.Kind, &m.Parent, &m.Model, &m.Active, &m.Weight, &m.Metrics, &m.CreatedAt, &m.Scored); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetAestheticWeights makes exactly the given models active, with the given
// blend weights; every other model becomes inactive.
func (s *Store) SetAestheticWeights(weights map[string]float64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE aesthetic_models SET active = 0, weight = 0`); err != nil {
		return err
	}
	for id, w := range weights {
		if !(w > 0) || math.IsInf(w, 0) { // also refuses NaN
			return fmt.Errorf("aesthetic model %s: weight must be positive", id)
		}
		res, err := tx.Exec(`UPDATE aesthetic_models SET active = 1, weight = ? WHERE id = ?`, w, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no aesthetic model %s", id)
		}
	}
	return tx.Commit()
}

// AestheticScores returns every model's score for a photo.
func (s *Store) AestheticScores(photoID int64) (map[string]float64, error) {
	rows, err := s.DB.Query(`SELECT model, score FROM aesthetic_scores WHERE photo_id = ?`, photoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var m string
		var v float64
		if err := rows.Scan(&m, &v); err != nil {
			return nil, err
		}
		out[m] = v
	}
	return out, rows.Err()
}

// AddAestheticScores records one model's scores (photo id -> score),
// replacing earlier scores by the same model.
func (s *Store) AddAestheticScores(model string, scores map[int64]float64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT OR REPLACE INTO aesthetic_scores (photo_id, model, score) VALUES (?,?,?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, id := range sortedIDs(scores) { // in key order: far faster B-tree inserts
		if _, err := st.Exec(id, model, scores[id]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AdoptLegacyAesthetic attributes the scores in photos.aesthetic, written
// before models were tracked, to m, and registers m if there were any. It
// is one transaction, so it can simply be rerun after an interruption.
func (s *Store) AdoptLegacyAesthetic(m AestheticModel) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT OR IGNORE INTO aesthetic_scores (photo_id, model, score)
		SELECT id, ?, aesthetic FROM photos WHERE aesthetic IS NOT NULL`, m.ID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO aesthetic_models (id, name, kind, model, active, weight, created_at)
			VALUES (?,?,?,?,?,?,?)`, m.ID, m.Name, m.Kind, m.Model, m.Active, m.Weight, time.Now().Unix()); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

// EachEmbeddingUnscored streams the CLIP embedding, and the change number,
// of every photo that model has not scored yet.
func (s *Store) EachEmbeddingUnscored(model string, fn func(id, seq int64, blob []byte)) error {
	rows, err := s.DB.Query(`SELECT c.photo_id, p.seq, c.clip FROM photo_clip c JOIN photos p ON p.id = c.photo_id
		WHERE NOT EXISTS (SELECT 1 FROM aesthetic_scores s WHERE s.photo_id = c.photo_id AND s.model = ?)`, model)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, seq int64
		var b []byte
		if err := rows.Scan(&id, &seq, &b); err != nil {
			return err
		}
		fn(id, seq, b)
	}
	return rows.Err()
}

// SeqScore is a score computed from a photo as it was at change number Seq.
type SeqScore struct {
	ID, Seq int64
	Score   float64
}

// AddFreshAestheticScores records one model's scores, in photo id order,
// skipping photos that changed since their score was computed (the indexer
// re-saved them meanwhile) and photos the model has scored since.
func (s *Store) AddFreshAestheticScores(model string, scores []SeqScore) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT OR IGNORE INTO aesthetic_scores (photo_id, model, score)
		SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM photos WHERE id = ? AND seq = ?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, v := range scores {
		if _, err := st.Exec(v.ID, model, v.Score, v.ID, v.Seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PhotoScores is one photo's scores, as streamed by EachPhotoScores.
type PhotoScores struct {
	ID, Seq   int64
	Technical float64
	Aesthetic sql.NullFloat64
	Overall   float64
	Models    map[string]float64 // the requested models' scores
}

// EachPhotoScores streams every photo's technical, aesthetic and overall
// scores and change number, with the scores of the given models, in photo
// id order.
func (s *Store) EachPhotoScores(models []string, fn func(PhotoScores)) error {
	q := `SELECT p.id, p.seq, p.technical, p.aesthetic, p.overall, s.model, s.score FROM photos p
		LEFT JOIN aesthetic_scores s ON s.photo_id = p.id AND s.model IN (''` + strings.Repeat(",?", len(models)) + `)
		ORDER BY p.id`
	args := make([]any, len(models))
	for i, m := range models {
		args[i] = m
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cur := PhotoScores{ID: -1}
	for rows.Next() {
		var p PhotoScores
		var m sql.NullString
		var v sql.NullFloat64
		if err := rows.Scan(&p.ID, &p.Seq, &p.Technical, &p.Aesthetic, &p.Overall, &m, &v); err != nil {
			return err
		}
		if p.ID != cur.ID {
			if cur.ID >= 0 {
				fn(cur)
			}
			cur, cur.Models = p, map[string]float64{}
		}
		if m.Valid && v.Valid {
			cur.Models[m.String] = v.Float64
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if cur.ID >= 0 {
		fn(cur)
	}
	return nil
}

// BlendUpdate is a photo's new aesthetic (NULL when invalid) and overall
// score, computed from the photo as it was at change number Seq.
type BlendUpdate struct {
	ID, Seq   int64
	Aesthetic sql.NullFloat64
	Overall   float64
}

// SetBlendedScores writes blended scores, in photo id order, skipping photos
// that changed since they were computed: the indexer has already scored
// those afresh.
func (s *Store) SetBlendedScores(updates []BlendUpdate) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`UPDATE photos SET aesthetic = ?, overall = ? WHERE id = ? AND seq = ?`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, u := range updates {
		if _, err := st.Exec(u.Aesthetic, u.Overall, u.ID, u.Seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Feedback is one person's judgement of a photo's aesthetic score: their
// own rating (0-10), a verdict on the score they were shown, or both.
type Feedback struct {
	ID        int64
	PhotoID   int64
	Rater     string
	Rating    sql.NullFloat64
	Verdict   string  // "" | agree | high (score too high) | low (too low)
	Shown     float64 // the aesthetic score on screen at the time
	Models    string  // the active models and weights behind it
	CreatedAt int64
}

// Verdicts accepted in feedback.
var Verdicts = map[string]bool{"agree": true, "high": true, "low": true}

// AddFeedback records a judgement.
func (s *Store) AddFeedback(f Feedback) (int64, error) {
	if f.Verdict != "" && !Verdicts[f.Verdict] {
		return 0, fmt.Errorf("unknown verdict %q", f.Verdict)
	}
	if f.Rating.Valid && (f.Rating.Float64 < 0 || f.Rating.Float64 > 10) {
		return 0, fmt.Errorf("rating must be between 0 and 10")
	}
	if !f.Rating.Valid && f.Verdict == "" {
		return 0, fmt.Errorf("feedback needs a rating or a verdict")
	}
	if f.CreatedAt == 0 {
		f.CreatedAt = time.Now().Unix()
	}
	res, err := s.DB.Exec(`INSERT INTO aesthetic_feedback (photo_id, rater, rating, verdict, shown, models, created_at)
		SELECT id, ?, ?, ?, ?, ?, ? FROM photos WHERE id = ?`,
		f.Rater, f.Rating, nullStr(f.Verdict), f.Shown, nullStr(f.Models), f.CreatedAt, f.PhotoID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNoPhoto
	}
	return res.LastInsertId()
}

// ErrNoPhoto is returned when a photo id does not exist.
var ErrNoPhoto = errors.New("no such photo")

// LatestFeedback returns each rater's judgement of each photo that still
// has a CLIP embedding, with that embedding: their latest rating, or if
// they never rated it, their latest verdict (a rating says more than a
// verdict, so a later "about right" does not override it). Rater "" means
// all raters.
func (s *Store) LatestFeedback(rater string) ([]Feedback, [][]byte, error) {
	rows, err := s.DB.Query(`SELECT f.id, f.photo_id, f.rater, f.rating, COALESCE(f.verdict, ''), COALESCE(f.shown, 0),
		COALESCE(f.models, ''), f.created_at, c.clip
		FROM aesthetic_feedback f JOIN photo_clip c ON c.photo_id = f.photo_id
		WHERE (? = '' OR f.rater = ?) AND f.id = COALESCE(
			(SELECT MAX(g.id) FROM aesthetic_feedback g
				WHERE g.photo_id = f.photo_id AND g.rater = f.rater AND g.rating IS NOT NULL),
			(SELECT MAX(g.id) FROM aesthetic_feedback g
				WHERE g.photo_id = f.photo_id AND g.rater = f.rater))
		ORDER BY f.id`, rater, rater)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Feedback
	var embs [][]byte
	for rows.Next() {
		var f Feedback
		var b []byte
		if err := rows.Scan(&f.ID, &f.PhotoID, &f.Rater, &f.Rating, &f.Verdict, &f.Shown, &f.Models, &f.CreatedAt, &b); err != nil {
			return nil, nil, err
		}
		out = append(out, f)
		embs = append(embs, b)
	}
	return out, embs, rows.Err()
}

// PhotoFeedback returns a rater's latest judgement of a photo, if any.
func (s *Store) PhotoFeedback(photoID int64, rater string) (*Feedback, error) {
	var f Feedback
	err := s.DB.QueryRow(`SELECT id, photo_id, rater, rating, COALESCE(verdict, ''), COALESCE(shown, 0), COALESCE(models, ''), created_at
		FROM aesthetic_feedback WHERE photo_id = ? AND rater = ? ORDER BY id DESC LIMIT 1`, photoID, rater).
		Scan(&f.ID, &f.PhotoID, &f.Rater, &f.Rating, &f.Verdict, &f.Shown, &f.Models, &f.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func sortedIDs[V any](m map[int64]V) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// FeedbackSummary counts judged photos.
type FeedbackSummary struct {
	Photos      int // distinct photos judged by anyone
	NoEmbedding int // of those, photos without a CLIP embedding
	Raters      []RaterCount
}

// RaterCount is how many photos one rater has judged.
type RaterCount struct {
	Rater  string
	Photos int
}

// FeedbackSummary counts judged photos, overall and per rater (by name).
func (s *Store) FeedbackSummary() (FeedbackSummary, error) {
	var out FeedbackSummary
	err := s.DB.QueryRow(`SELECT COUNT(DISTINCT f.photo_id),
		COUNT(DISTINCT CASE WHEN c.photo_id IS NULL THEN f.photo_id END)
		FROM aesthetic_feedback f LEFT JOIN photo_clip c ON c.photo_id = f.photo_id`).Scan(&out.Photos, &out.NoEmbedding)
	if err != nil {
		return out, err
	}
	rows, err := s.DB.Query(`SELECT rater, COUNT(DISTINCT photo_id) FROM aesthetic_feedback GROUP BY rater ORDER BY rater`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r RaterCount
		if err := rows.Scan(&r.Rater, &r.Photos); err != nil {
			return out, err
		}
		out.Raters = append(out.Raters, r)
	}
	return out, rows.Err()
}

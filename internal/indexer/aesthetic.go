package indexer

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

// AestheticModel is an active aesthetic model with its share of the blend.
type AestheticModel struct {
	*ml.Aesthetic
	Weight float64
}

// Score rates an embedding with every model: the per-model scores and their
// weighted blend (invalid when there are no models).
func ScoreAesthetic(models []AestheticModel, emb []float32) (map[string]float64, sql.NullFloat64) {
	if len(models) == 0 {
		return nil, sql.NullFloat64{}
	}
	scores := make(map[string]float64, len(models))
	for _, m := range models {
		scores[m.ID] = float64(m.Score(emb))
	}
	return scores, BlendAesthetic(models, scores)
}

// BlendAesthetic is the weighted mean of the active models' scores that are
// present; invalid when none is.
func BlendAesthetic(models []AestheticModel, scores map[string]float64) sql.NullFloat64 {
	var sum, wsum float64
	for _, m := range models {
		if v, ok := scores[m.ID]; ok {
			sum += m.Weight * v
			wsum += m.Weight
		}
	}
	if wsum <= 0 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: sum / wsum, Valid: true}
}

// DescribeBlend is a compact record of the models behind a blended score,
// e.g. "eva-ridge-828e586c6724:1".
func DescribeBlend(models []AestheticModel) string {
	parts := make([]string, len(models))
	for i, m := range models {
		parts[i] = fmt.Sprintf("%s:%.3g", m.ID, m.Weight)
	}
	return strings.Join(parts, ",")
}

// ActiveAesthetics loads the catalogue's active models. Legacy models (no
// weights) cannot score new photos and are skipped.
func ActiveAesthetics(st *store.Store) ([]AestheticModel, error) {
	all, err := st.AestheticModels()
	if err != nil {
		return nil, err
	}
	var out []AestheticModel
	for _, m := range all {
		if !m.Active || len(m.Model) == 0 {
			continue
		}
		a, err := ml.ParseAesthetic(m.Model)
		if err != nil {
			return nil, fmt.Errorf("aesthetic model %s: %w", m.ID, err)
		}
		if a.ID != m.ID {
			return nil, fmt.Errorf("aesthetic model %s: stored weights hash to %s", m.ID, a.ID)
		}
		out = append(out, AestheticModel{a, m.Weight})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// legacyAesthetic maps the model version recorded by catalogues from before
// models were tracked (meta aesthetic_model) to the model that produced
// their scores.
var legacyAesthetic = map[string]string{"eva-ridge-1": "eva-ridge-828e586c6724"}

// LegacyLAION holds aesthetic scores from the LAION predictor used before
// the EVA model, kept for the record but not blended.
const LegacyLAION = "laion-legacy"

// SyncAesthetics prepares a catalogue's aesthetic models when it is opened:
//
//   - scores written before models were tracked are attributed to the model
//     that produced them (nothing is recomputed or thrown away);
//   - the built-in model is registered; it becomes active when nothing else
//     is, and takes over the weight of an earlier built-in model;
//   - photos that an active model has not scored yet (new models, older
//     photos) are scored from their stored CLIP embeddings. Existing scores
//     are never recomputed: a model's ID changes with its weights, so a
//     stored score always belongs to the exact model that produced it;
//   - when anything changed, the blended aesthetic and overall scores are
//     recomputed from the stored per-model scores.
//
// It returns the active models.
func SyncAesthetics(st *store.Store, builtin *ml.Aesthetic) ([]AestheticModel, error) {
	changed, err := adoptLegacyScores(st, builtin)
	if err != nil {
		return nil, err
	}
	added, err := st.AddAestheticModel(store.AestheticModel{ID: builtin.ID, Name: builtin.Name, Kind: "builtin", Model: builtin.JSON()})
	if err != nil {
		return nil, err
	}
	if added {
		changed = true
		if err := activateBuiltin(st, builtin.ID); err != nil {
			return nil, err
		}
	}
	active, err := ActiveAesthetics(st)
	if err != nil {
		return nil, err
	}
	for _, m := range active {
		n, err := ScoreUnscored(st, m.Aesthetic)
		if err != nil {
			return nil, err
		}
		changed = changed || n > 0
	}
	if changed {
		if _, err := RefreshBlend(st, active); err != nil {
			return nil, err
		}
	}
	return active, nil
}

func adoptLegacyScores(st *store.Store, builtin *ml.Aesthetic) (bool, error) {
	const key, done = "aesthetic_model", "aesthetic_registry"
	if st.GetMeta(done) != "" {
		return false, nil
	}
	m := store.AestheticModel{ID: LegacyLAION, Name: LegacyLAION, Kind: "legacy"}
	if id, ok := legacyAesthetic[st.GetMeta(key)]; ok {
		m = store.AestheticModel{ID: id, Name: id[:strings.LastIndex(id, "-")], Kind: "legacy", Active: true, Weight: 1}
		if id == builtin.ID {
			m.Kind, m.Model = "builtin", builtin.JSON()
		}
	}
	n, err := st.AdoptLegacyAesthetic(m)
	if err != nil {
		return false, fmt.Errorf("adopting earlier aesthetic scores: %w", err)
	}
	return n > 0, st.SetMeta(done, "1")
}

// activateBuiltin gives a newly registered built-in model the weight of the
// built-in models it supersedes, or makes it the only active model when
// none is active.
func activateBuiltin(st *store.Store, id string) error {
	all, err := st.AestheticModels()
	if err != nil {
		return err
	}
	weights := map[string]float64{}
	inherited := 0.0
	for _, m := range all {
		if !m.Active || m.ID == id {
			continue
		}
		if m.Kind == "builtin" || (m.Kind == "legacy" && len(m.Model) == 0) {
			inherited += m.Weight // superseded
			continue
		}
		weights[m.ID] = m.Weight
	}
	if inherited > 0 || len(weights) == 0 {
		weights[id] = max(inherited, 1)
	}
	return st.SetAestheticWeights(weights)
}

// ScoreUnscored scores, from stored CLIP embeddings, every photo that model
// has not scored yet. It returns how many it scored.
func ScoreUnscored(st *store.Store, a *ml.Aesthetic) (int, error) {
	var all []store.SeqScore // in photo id order, which keeps the inserts fast
	err := st.EachEmbeddingUnscored(a.ID, func(id, seq int64, b []byte) {
		all = append(all, store.SeqScore{ID: id, Seq: seq, Score: float64(a.Score(vindex.DecodeF16(b)))})
	})
	if err != nil {
		return 0, err
	}
	// Written after the scan: SQLite allows one writer, and the scan's read
	// transaction would otherwise see its own writes. Photos re-saved by the
	// indexer in between are skipped (their embedding may have changed).
	for start := 0; start < len(all); start += 50000 {
		if err := st.AddFreshAestheticScores(a.ID, all[start:min(start+50000, len(all))]); err != nil {
			return start, err
		}
	}
	return len(all), nil
}

// RefreshBlend recomputes every photo's aesthetic score as the blend of the
// active models' stored scores, and its overall score. It returns how many
// photos changed. A photo the indexer re-saves meanwhile keeps the scores
// the indexer gave it.
func RefreshBlend(st *store.Store, active []AestheticModel) (int, error) {
	ids := make([]string, len(active))
	for i, m := range active {
		ids[i] = m.ID
	}
	var all []store.BlendUpdate // in photo id order
	err := st.EachPhotoScores(ids, func(p store.PhotoScores) {
		a := BlendAesthetic(active, p.Models)
		if !a.Valid && p.Aesthetic.Valid {
			// No active model has scored this photo (no CLIP embedding):
			// keep what it has rather than erase it.
			return
		}
		o := Overall(p.Technical, a)
		if a.Valid == p.Aesthetic.Valid && (!a.Valid || abs(a.Float64-p.Aesthetic.Float64) < 1e-9) && o == p.Overall {
			return
		}
		all = append(all, store.BlendUpdate{ID: p.ID, Seq: p.Seq, Aesthetic: a, Overall: o})
	})
	if err != nil {
		return 0, err
	}
	for start := 0; start < len(all); start += 50000 {
		if err := st.SetBlendedScores(all[start:min(start+50000, len(all))]); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

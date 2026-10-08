package indexer

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"sort"
	"strings"

	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

// Sample is one training example for an aesthetic model.
type Sample struct {
	X      []float32 // normalised CLIP embedding
	Y      float64   // target rating, 0-10
	C      float64   // weight
	Rating bool      // Y is a rating given by a person (not derived from a verdict)
}

// VerdictStep is how far a "too high" / "too low" verdict moves the target
// from the score shown, and VerdictWeight how much such a target counts
// relative to a rating. A verdict says which way the score is wrong but not
// by how much, so it is a weaker, half-weight signal.
const (
	VerdictStep   = 1.0
	VerdictWeight = 0.5
)

// FeedbackSamples turns stored feedback into training samples: a rating is
// its own target; "agree" targets the score shown, "too high"/"too low" the
// score shown minus/plus VerdictStep.
func FeedbackSamples(fb []store.Feedback, embs [][]byte) []Sample {
	var out []Sample
	for i, f := range fb {
		s := Sample{X: vindex.DecodeF16(embs[i]), C: 1}
		switch {
		case f.Rating.Valid:
			s.Y, s.Rating = f.Rating.Float64, true
		case f.Verdict == "agree":
			s.Y, s.C = f.Shown, VerdictWeight
		case f.Verdict == "high":
			s.Y, s.C = f.Shown-VerdictStep, VerdictWeight
		case f.Verdict == "low":
			s.Y, s.C = f.Shown+VerdictStep, VerdictWeight
		default:
			continue
		}
		out = append(out, s)
	}
	return out
}

// TrainOptions configure TrainAesthetic.
type TrainOptions struct {
	Name       string // name of the new model (default "user")
	MinSamples int    // refuse to train on fewer (default 20)
	Folds      int    // cross-validation folds (default 5)
	Seed       int64
}

// TrainResult describes a trained model and how it did on held-out
// feedback, compared with the current blend (the prior it started from).
type TrainResult struct {
	Model               *ml.Aesthetic
	Parent              string // the blend it was trained from
	Samples, Ratings    int
	Lambda              float64
	PriorRMSE, RMSE     float64 // weighted, on held-out samples
	PriorRho, Rho       float64 // Spearman on held-out ratings (NaN with < 5)
	Mix                 float64 // the new model's share of the blend that predicted held-out samples best
	Better              bool    // the blend with the new model beats the current one
	Weights             map[string]float64
	BlendRMSE, BlendRho float64 // held-out quality of that blend
}

// Lambdas tried when training; larger stays closer to the prior.
var Lambdas = []float64{0.03, 0.1, 0.3, 1, 3, 10, 30, 100, 300, 1000}

var nameClean = regexp.MustCompile(`[^a-z0-9.-]+`)

// ModelName makes a valid model name from free text ("Ann Lee" -> "ann-lee").
func ModelName(s string) string {
	s = strings.Trim(nameClean.ReplaceAllString(strings.ToLower(s), "-"), "-.")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-.")
	}
	return s
}

// PriorModel is the blend of the active models as one linear model: the
// blend of linear models is linear, so this is exactly what users see.
func PriorModel(active []AestheticModel) (*ml.Aesthetic, error) {
	if len(active) == 0 {
		return nil, fmt.Errorf("no active aesthetic model to start from")
	}
	w := make([]float32, ml.EmbedDim)
	var b, sum float64
	for _, m := range active {
		sum += m.Weight
	}
	for _, m := range active {
		f := m.Weight / sum
		for j := range w {
			w[j] += float32(f) * m.W[j]
		}
		b += f * float64(m.B)
	}
	return ml.NewAesthetic("blend", w, float32(b))
}

// TrainAesthetic fits a new model to samples, starting from the blend of the
// active models: ridge regression pulled toward the blend's weights, with
// the pull (λ) chosen by k-fold cross-validation. It then reports, on
// held-out samples, how the new model and the best blend of it with the
// current models compare with the current blend. Nothing is stored.
func TrainAesthetic(samples []Sample, active []AestheticModel, opt TrainOptions) (*TrainResult, error) {
	opt.Name = ModelName(opt.Name)
	if opt.Name == "" {
		opt.Name = "user"
	}
	if opt.MinSamples <= 0 {
		opt.MinSamples = 20
	}
	if opt.Folds <= 1 {
		opt.Folds = 5
	}
	prior, err := PriorModel(active)
	if err != nil {
		return nil, err
	}
	n := len(samples)
	if n < max(opt.MinSamples, 2) { // cross-validation needs two
		return nil, fmt.Errorf("%d judgements so far; at least %d are needed to train", n, max(opt.MinSamples, 2))
	}
	folds := min(opt.Folds, n)
	fold := rand.New(rand.NewSource(opt.Seed + 1)).Perm(n)
	for i := range fold {
		fold[i] %= folds
	}
	x := make([][]float32, n)
	y, c := make([]float64, n), make([]float64, n)
	res := &TrainResult{Parent: DescribeBlend(active), Samples: n}
	for i, s := range samples {
		x[i], y[i], c[i] = s.X, s.Y, s.C
		if s.Rating {
			res.Ratings++
		}
	}
	priorPred := make([]float64, n)
	for i := range x {
		priorPred[i] = float64(prior.Score(x[i]))
	}
	// Out-of-fold predictions for each λ. The pull toward the prior is the
	// strongest whose error is within one standard error of the best (the
	// "one-standard-error rule"): with few judgements the error estimates
	// are noisy, and the smallest raw error is often just luck.
	preds := make([][]float64, len(Lambdas))
	// ranking part of the predictions (without the bias): each fold's model
	// has its own bias, which would scramble a ranking pooled across folds
	ranks := make([][]float64, len(Lambdas))
	errs := make([]float64, len(Lambdas))
	bestI := 0
	for li, lambda := range Lambdas {
		pred, rank := make([]float64, n), make([]float64, n)
		for f := 0; f < folds; f++ {
			var tx [][]float32
			var ty, tc []float64
			for i := range x {
				if fold[i] != f {
					tx, ty, tc = append(tx, x[i]), append(ty, y[i]), append(tc, c[i])
				}
			}
			w, b, err := ml.FitRidgeToward(tx, ty, tc, prior.W, prior.B, lambda)
			if err != nil {
				return nil, err
			}
			for i := range x {
				if fold[i] == f {
					rank[i] = float64(ml.Dot(w, x[i]))
					pred[i] = rank[i] + float64(b)
				}
			}
		}
		preds[li], ranks[li], errs[li] = pred, rank, wmse(pred, y, c)
		if errs[li] < errs[bestI] {
			bestI = li
		}
	}
	se := wmseSE(preds[bestI], y, c)
	chosen := bestI
	for li := len(Lambdas) - 1; li > bestI; li-- {
		if errs[li] <= errs[bestI]+se {
			chosen = li
			break
		}
	}
	bestPred := preds[chosen]
	res.Lambda = Lambdas[chosen]
	res.RMSE, res.PriorRMSE = math.Sqrt(errs[chosen]), wrmse(priorPred, y, c)
	priorRank := make([]float64, n)
	for i := range x {
		priorRank[i] = float64(ml.Dot(prior.W, x[i]))
	}
	res.Rho, res.PriorRho = ratingRho(ranks[chosen], samples), ratingRho(priorRank, samples)
	// the new model's share of the blend, chosen on held-out predictions
	res.BlendRMSE = res.PriorRMSE
	blendPred := priorPred
	for k := 1; k <= 10; k++ {
		a := float64(k) / 10
		mixed := make([]float64, n)
		for i := range mixed {
			mixed[i] = a*bestPred[i] + (1-a)*priorPred[i]
		}
		if e := wrmse(mixed, y, c); e < res.BlendRMSE-1e-12 {
			res.Mix, res.BlendRMSE, blendPred = a, e, mixed
		}
	}
	blendRank := make([]float64, n)
	for i := range blendRank {
		blendRank[i] = res.Mix*ranks[chosen][i] + (1-res.Mix)*priorRank[i]
	}
	res.BlendRho = ratingRho(blendRank, samples)
	// Better: the held-out gain over the current blend exceeds its standard
	// error (paired, per judgement) and is at least 1%.
	gain, gainSE := pairedGain(priorPred, blendPred, y, c)
	res.Better = res.Mix > 0 && gain > gainSE && res.BlendRMSE < 0.99*res.PriorRMSE
	w, b, err := ml.FitRidgeToward(x, y, c, prior.W, prior.B, res.Lambda)
	if err != nil {
		return nil, err
	}
	if res.Model, err = ml.NewAesthetic(opt.Name, w, b); err != nil {
		return nil, err
	}
	res.Weights = map[string]float64{}
	if res.Mix < 1 {
		var sum float64
		for _, m := range active {
			sum += m.Weight
		}
		for _, m := range active {
			res.Weights[m.ID] = (1 - res.Mix) * m.Weight / sum
		}
	}
	// += : a retrain can reproduce an active model exactly (same ID)
	res.Weights[res.Model.ID] += max(res.Mix, 0.1)
	return res, nil
}

// SaveTrained registers a trained model (inactive) with its evaluation; with
// activate it also joins the blend with the proposed weights, scores every
// photo from its stored embedding and updates the blended scores.
func SaveTrained(st *store.Store, res *TrainResult, activate bool) error {
	metrics, _ := json.Marshal(map[string]any{
		"samples": res.Samples, "ratings": res.Ratings, "lambda": res.Lambda,
		"prior_rmse": round3(res.PriorRMSE), "rmse": round3(res.RMSE),
		"prior_spearman": round3(res.PriorRho), "spearman": round3(res.Rho),
		"mix": res.Mix, "blend_rmse": round3(res.BlendRMSE),
	})
	if _, err := st.AddAestheticModel(store.AestheticModel{ID: res.Model.ID, Name: res.Model.Name, Kind: "user",
		Parent: res.Parent, Model: res.Model.JSON(), Metrics: string(metrics)}); err != nil {
		return err
	}
	if !activate {
		return nil
	}
	return Activate(st, res.Weights)
}

// Activate sets the blend weights, scores photos that a newly active model
// has not scored yet, and updates blended scores.
func Activate(st *store.Store, weights map[string]float64) error {
	all, err := st.AestheticModels()
	if err != nil {
		return err
	}
	usable := map[string]bool{}
	for _, m := range all {
		usable[m.ID] = len(m.Model) > 0
	}
	for id := range weights {
		if !usable[id] {
			return fmt.Errorf("aesthetic model %s cannot score photos (unknown, or scores only)", id)
		}
	}
	if len(weights) == 0 {
		return fmt.Errorf("at least one aesthetic model must stay active")
	}
	if err := st.SetAestheticWeights(weights); err != nil {
		return err
	}
	active, err := ActiveAesthetics(st)
	if err != nil {
		return err
	}
	for _, m := range active {
		if _, err := ScoreUnscored(st, m.Aesthetic); err != nil {
			return err
		}
	}
	_, err = RefreshBlend(st, active)
	return err
}

func round3(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return math.Round(v*1000) / 1000
}

func wmse(pred, y, c []float64) float64 {
	var s, w float64
	for i := range pred {
		d := pred[i] - y[i]
		s += c[i] * d * d
		w += c[i]
	}
	return s / w
}

// wmseSE is the standard error of wmse.
func wmseSE(pred, y, c []float64) float64 {
	e := make([]float64, len(pred))
	for i := range pred {
		e[i] = (pred[i] - y[i]) * (pred[i] - y[i])
	}
	return wmeanSE(e, c)
}

// pairedGain is the weighted mean reduction in squared error from a to b,
// and its standard error.
func pairedGain(a, b, y, c []float64) (float64, float64) {
	d := make([]float64, len(a))
	var s, w float64
	for i := range a {
		d[i] = (a[i]-y[i])*(a[i]-y[i]) - (b[i]-y[i])*(b[i]-y[i])
		s += c[i] * d[i]
		w += c[i]
	}
	return s / w, wmeanSE(d, c)
}

// wmeanSE is the standard error of a weighted mean.
func wmeanSE(v, c []float64) float64 {
	var s, w, w2 float64
	for i := range v {
		s += c[i] * v[i]
		w += c[i]
		w2 += c[i] * c[i]
	}
	m := s / w
	var ss float64
	for i := range v {
		ss += c[i] * c[i] * (v[i] - m) * (v[i] - m)
	}
	if w2 >= w*w {
		return math.Inf(1)
	}
	return math.Sqrt(ss) / w * math.Sqrt(float64(len(v))/float64(max(len(v)-1, 1)))
}

func wrmse(pred, y, c []float64) float64 {
	var s, w float64
	for i := range pred {
		d := pred[i] - y[i]
		s += c[i] * d * d
		w += c[i]
	}
	return math.Sqrt(s / w)
}

// ratingRho is the Spearman correlation between predictions and the samples
// that are ratings (NaN with fewer than 5).
func ratingRho(pred []float64, samples []Sample) float64 {
	var p, y []float64
	for i, s := range samples {
		if s.Rating {
			p, y = append(p, pred[i]), append(y, s.Y)
		}
	}
	if len(p) < 5 {
		return math.NaN()
	}
	return Spearman(p, y)
}

// Spearman is the rank correlation of a and b (ties get their mean rank).
func Spearman(a, b []float64) float64 {
	ra, rb := ranks(a), ranks(b)
	var ma, mb float64
	for i := range ra {
		ma += ra[i]
		mb += rb[i]
	}
	ma /= float64(len(ra))
	mb /= float64(len(rb))
	var sab, saa, sbb float64
	for i := range ra {
		da, db := ra[i]-ma, rb[i]-mb
		sab += da * db
		saa += da * da
		sbb += db * db
	}
	if saa == 0 || sbb == 0 {
		return math.NaN()
	}
	return sab / math.Sqrt(saa*sbb)
}

func ranks(v []float64) []float64 {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return v[idx[i]] < v[idx[j]] })
	r := make([]float64, len(v))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		for k := i; k <= j; k++ {
			r[idx[k]] = float64(i+j)/2 + 1
		}
		i = j + 1
	}
	return r
}

package indexer

import (
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/auteursoft/archivis/internal/ml"
)

func randUnit(rng *rand.Rand) []float32 {
	v := make([]float32, ml.EmbedDim)
	for j := range v {
		v[j] = float32(rng.NormFloat64())
	}
	return ml.Normalize(v)
}

// A person whose taste differs from the built-in model in a consistent way
// gets a model that predicts their held-out ratings better; ratings that
// are only noise around the current scores do not produce a "better" model.
func TestTrainAesthetic(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	builtin := ml.DefaultAesthetic()
	active := []AestheticModel{{builtin, 1}}
	taste := randUnit(rng) // what this person likes, beyond the built-in model
	mk := func(n int, personal bool) []Sample {
		var out []Sample
		for i := 0; i < n; i++ {
			x := randUnit(rng)
			y := float64(builtin.Score(x)) + 0.3*rng.NormFloat64()
			if personal {
				y += 25 * float64(ml.Dot(taste, x)) // ~±1 point on random photos
			}
			s := Sample{X: x, Y: y, C: 1, Rating: true}
			if i%3 == 0 { // some feedback is a verdict instead
				shown := float64(builtin.Score(x))
				s.Rating, s.C = false, VerdictWeight
				s.Y = shown
				if y < shown-0.5 {
					s.Y = shown - VerdictStep
				} else if y > shown+0.5 {
					s.Y = shown + VerdictStep
				}
			}
			out = append(out, s)
		}
		return out
	}
	res, err := TrainAesthetic(mk(300, true), active, TrainOptions{Name: "Ann Lee"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("personal: λ %.2g, RMSE %.3f -> %.3f, ρ %.3f -> %.3f, mix %.1f", res.Lambda, res.PriorRMSE, res.RMSE, res.PriorRho, res.Rho, res.Mix)
	if !res.Better || res.RMSE >= res.PriorRMSE || res.Model.Name != "ann-lee" || res.Weights[res.Model.ID] <= 0 {
		t.Fatalf("consistent taste not learnt: %+v", res)
	}
	// fresh photos the model never saw
	test := mk(300, true)
	var pe, ne float64
	for _, s := range test {
		if s.Rating {
			pe += math.Pow(float64(builtin.Score(s.X))-s.Y, 2)
			ne += math.Pow(float64(res.Model.Score(s.X))-s.Y, 2)
		}
	}
	if ne >= pe {
		t.Fatalf("worse on new photos: %.2f vs %.2f", ne, pe)
	}

	res, err = TrainAesthetic(mk(300, false), active, TrainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("noise: λ %.2g, RMSE %.3f -> %.3f, mix %.1f", res.Lambda, res.PriorRMSE, res.RMSE, res.Mix)
	if res.Better {
		t.Fatalf("noise produced a better model: %+v", res)
	}
	if _, err := TrainAesthetic(mk(5, true), active, TrainOptions{}); err == nil {
		t.Fatal("trained on 5 judgements")
	}
}

func TestSpearman(t *testing.T) {
	if r := Spearman([]float64{1, 2, 3, 4}, []float64{10, 20, 30, 40}); math.Abs(r-1) > 1e-12 {
		t.Fatal(r)
	}
	if r := Spearman([]float64{1, 2, 3, 4}, []float64{4, 3, 2, 1}); math.Abs(r+1) > 1e-12 {
		t.Fatal(r)
	}
	if r := Spearman([]float64{1, 1, 2}, []float64{1, 2, 3}); math.Abs(r-0.8660254) > 1e-6 {
		t.Fatal(r)
	}
}

// One judgement cannot be cross-validated: a clear error, whatever --min.
func TestTrainNeedsTwoJudgements(t *testing.T) {
	b := ml.DefaultAesthetic()
	one := []Sample{{X: randUnit(rand.New(rand.NewSource(1))), Y: 5, C: 1, Rating: true}}
	_, err := TrainAesthetic(one, []AestheticModel{{b, 1}}, TrainOptions{MinSamples: 1})
	if err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("error %v", err)
	}
}

// A retrain that reproduces an active model exactly (same weights, same
// name, so the same ID) adds to its weight rather than replacing it.
func TestRetrainSameModelKeepsWeight(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	b := ml.DefaultAesthetic()
	user, _ := ml.NewAesthetic("user", b.W, b.B)
	var samples []Sample
	for i := 0; i < 30; i++ {
		x := randUnit(rng)
		samples = append(samples, Sample{X: x, Y: float64(user.Score(x)), C: 1, Rating: true}) // nothing new to learn
	}
	res, err := TrainAesthetic(samples, []AestheticModel{{user, 1}}, TrainOptions{Name: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model.ID != user.ID {
		t.Skipf("fit moved the weights (%s); nothing to check", res.Model.ID)
	}
	if len(res.Weights) != 1 || math.Abs(res.Weights[user.ID]-(1-res.Mix)-max(res.Mix, 0.1)) > 1e-12 {
		t.Fatalf("weights %v (mix %.1f)", res.Weights, res.Mix)
	}
}

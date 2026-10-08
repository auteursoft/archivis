package ml

import (
	"math"
	"math/rand"
	"testing"
)

// Both solver forms reach the optimum; with plenty of data the fit recovers
// the truth, and a huge λ returns the prior.
func TestFitRidgeToward(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const d = 8
	truth := make([]float32, d)
	for j := range truth {
		truth[j] = float32(rng.NormFloat64())
	}
	sample := func(n int) ([][]float32, []float64, []float64) {
		x := make([][]float32, n)
		y, c := make([]float64, n), make([]float64, n)
		for i := range x {
			x[i] = make([]float32, d)
			for j := range x[i] {
				x[i][j] = float32(rng.NormFloat64())
			}
			y[i] = float64(Dot(truth, x[i])) + 2 + 0.01*rng.NormFloat64()
			c[i] = 0.5 + rng.Float64()
		}
		return x, y, c
	}
	w0 := make([]float32, d)
	x, y, c := sample(5) // dual (n < d+1)
	wd, bd, err := FitRidgeToward(x, y, c, w0, 1, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	// At the optimum the objective's gradient is zero, whichever form
	// (dual for few samples, primal for many) solved it.
	grad := func(w []float32, b float32, x [][]float32, y, c []float64, lambda float64) float64 {
		g := make([]float64, d+1)
		for i := range x {
			e := float64(Dot(w, x[i])) + float64(b) - y[i]
			for j := 0; j < d; j++ {
				g[j] += 2 * c[i] * e * float64(x[i][j])
			}
			g[d] += 2 * c[i] * e
		}
		for j := 0; j < d; j++ {
			g[j] += 2 * lambda * float64(w[j]-w0[j])
		}
		g[d] += 2 * lambda / (BiasFreedom * BiasFreedom) * float64(b-1)
		m := 0.0
		for _, v := range g {
			m = math.Max(m, math.Abs(v))
		}
		return m
	}
	if g := grad(wd, bd, x, y, c, 0.3); g > 1e-3 {
		t.Fatalf("dual solution not optimal: gradient %g", g)
	}
	x, y, c = sample(200) // primal
	wp, bp, err := FitRidgeToward(x, y, c, w0, 1, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if g := grad(wp, bp, x, y, c, 0.3); g > 1e-2 {
		t.Fatalf("primal solution not optimal: gradient %g", g)
	}
	if math.Abs(float64(bp)-2) > 0.05 || math.Abs(float64(wp[3]-truth[3])) > 0.05 {
		t.Fatalf("did not recover the truth: b %.3f w3 %.3f vs %.3f", bp, wp[3], truth[3])
	}
	wh, bh, _ := FitRidgeToward(x, y, c, w0, 1, 1e12)
	if math.Abs(float64(bh)-1) > 1e-3 || math.Abs(float64(wh[0])) > 1e-3 {
		t.Fatalf("huge λ moved away from the prior: b %.4f", bh)
	}
	if _, _, err := FitRidgeToward(x, y, c, w0, 1, 0); err == nil {
		t.Fatal("λ = 0 accepted")
	}
	// non-positive weights are refused in both forms (primal: n > d)
	for _, n := range []int{5, 200} {
		xs, ys, cs := sample(n)
		for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1)} {
			cs[0] = bad
			if _, _, err := FitRidgeToward(xs, ys, cs, w0, 1, 0.3); err == nil {
				t.Errorf("n=%d: sample weight %v accepted", n, bad)
			}
		}
	}
}

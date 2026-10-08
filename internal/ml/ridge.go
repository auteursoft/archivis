package ml

import (
	"errors"
	"math"
)

// FitRidgeToward fits a linear model θ = (w, b) to samples x_i with targets
// y_i and weights c_i, pulled toward a prior θ0:
//
//	minimise  Σ c_i (w·x_i + b − y_i)² + λ ‖w − w0‖² + (λ/BiasFreedom²) (b − b0)²
//
// With few samples the weights stay close to the prior; with many they
// follow the data. The bias is penalised far less: a person who rates
// everything a point lower shifts the bias cheaply without disturbing the
// weights, which decide how photos rank. λ must be positive. It solves for
// δ = θ − θ0 in whichever of the primal (d+1 unknowns) or dual (one per
// sample) forms is smaller.
func FitRidgeToward(x [][]float32, y, c []float64, w0 []float32, b0 float32, lambda float64) ([]float32, float32, error) {
	n := len(x)
	if n == 0 || len(y) != n || len(c) != n || !(lambda > 0) {
		return nil, 0, errors.New("ridge: bad input")
	}
	for _, ci := range c {
		if !(ci > 0) || math.IsInf(ci, 0) { // both solver forms need this
			return nil, 0, errors.New("ridge: sample weights must be positive and finite")
		}
	}
	d := len(w0) + 1
	aug := func(i, j int) float64 { // x̃_i[j], with a trailing BiasFreedom for the bias
		if j == d-1 {
			return BiasFreedom
		}
		return float64(x[i][j])
	}
	// residuals of the prior
	r := make([]float64, n)
	for i := range x {
		if len(x[i]) != d-1 {
			return nil, 0, errors.New("ridge: dimension mismatch")
		}
		r[i] = y[i] - (float64(Dot(w0, x[i])) + float64(b0))
	}
	delta := make([]float64, d)
	if n < d {
		// dual: δ = X̃ᵀ α, (X̃X̃ᵀ + λ C⁻¹) α = r
		k := make([]float64, n*n)
		for i := 0; i < n; i++ {
			for j := 0; j <= i; j++ {
				v := float64(Dot(x[i], x[j])) + BiasFreedom*BiasFreedom
				k[i*n+j], k[j*n+i] = v, v
			}
			k[i*n+i] += lambda / c[i]
		}
		alpha, err := cholSolve(k, r, n)
		if err != nil {
			return nil, 0, err
		}
		for i := 0; i < n; i++ {
			for j := 0; j < d; j++ {
				delta[j] += alpha[i] * aug(i, j)
			}
		}
	} else {
		// primal: (X̃ᵀ C X̃ + λ I) δ = X̃ᵀ C r
		a := make([]float64, d*d)
		rhs := make([]float64, d)
		row := make([]float64, d)
		for i := 0; i < n; i++ {
			for j := 0; j < d; j++ {
				row[j] = aug(i, j)
			}
			for j := 0; j < d; j++ {
				cj := c[i] * row[j]
				rhs[j] += cj * r[i]
				for l := 0; l <= j; l++ {
					a[j*d+l] += cj * row[l]
				}
			}
		}
		for j := 0; j < d; j++ {
			for l := 0; l < j; l++ {
				a[l*d+j] = a[j*d+l]
			}
			a[j*d+j] += lambda
		}
		var err error
		if delta, err = cholSolve(a, rhs, d); err != nil {
			return nil, 0, err
		}
	}
	w := make([]float32, d-1)
	for j := range w {
		w[j] = w0[j] + float32(delta[j])
	}
	return w, b0 + float32(BiasFreedom*delta[d-1]), nil
}

// BiasFreedom scales the bias feature: the bias is penalised BiasFreedom²
// times less than each weight.
const BiasFreedom = 10.0

// cholSolve solves A z = b for symmetric positive-definite A (n×n, row
// major; overwritten).
func cholSolve(a, b []float64, n int) ([]float64, error) {
	for j := 0; j < n; j++ {
		s := a[j*n+j]
		for k := 0; k < j; k++ {
			s -= a[j*n+k] * a[j*n+k]
		}
		if s <= 0 || math.IsNaN(s) {
			return nil, errors.New("ridge: matrix not positive definite")
		}
		ljj := math.Sqrt(s)
		a[j*n+j] = ljj
		for i := j + 1; i < n; i++ {
			s := a[i*n+j]
			for k := 0; k < j; k++ {
				s -= a[i*n+k] * a[j*n+k]
			}
			a[i*n+j] = s / ljj
		}
	}
	z := make([]float64, n)
	for i := 0; i < n; i++ { // L y = b
		s := b[i]
		for k := 0; k < i; k++ {
			s -= a[i*n+k] * z[k]
		}
		z[i] = s / a[i*n+i]
	}
	for i := n - 1; i >= 0; i-- { // Lᵀ z = y
		s := z[i]
		for k := i + 1; k < n; k++ {
			s -= a[k*n+i] * z[k]
		}
		z[i] = s / a[i*n+i]
	}
	return z, nil
}

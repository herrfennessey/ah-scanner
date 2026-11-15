// Package stats holds the small amount of statistics the reports rely on: shrinking sparse
// rates toward a prior (empirical Bayes) and Beta credible intervals.
package stats

import (
	"math"

	"gonum.org/v1/gonum/stat/distuv"
)

// Shrink pulls a rate observed over n trials toward the prior mean p0 with prior strength m
// (m pseudo-trials). With n ≫ m the observation wins; with n ≪ m the prior does.
func Shrink(rate, n, p0, m float64) float64 {
	if n+m <= 0 {
		return p0
	}
	return (rate*n + p0*m) / (n + m)
}

// EBStrength estimates the prior strength m for a family of rates (e.g. one per time block) by
// the method of moments: the more the groups truly differ beyond binomial noise, the weaker the
// prior. Returned m is clamped to [1, 50]; groups with no trials are ignored.
func EBStrength(rates, ns []float64, p0 float64) float64 {
	var sumN, sumW, sumNoise float64
	k := 0
	for i := range rates {
		if ns[i] <= 0 {
			continue
		}
		k++
		sumN += ns[i]
	}
	if k < 2 || p0 <= 0 || p0 >= 1 {
		return 10
	}
	// Weighted variance of the observed rates around p0, minus the expected binomial noise.
	for i := range rates {
		if ns[i] <= 0 {
			continue
		}
		w := ns[i] / sumN
		d := rates[i] - p0
		sumW += w * d * d
		sumNoise += w * p0 * (1 - p0) / ns[i]
	}
	tau2 := sumW - sumNoise
	if tau2 <= 0 {
		return 50 // no detectable difference between groups: pool hard
	}
	m := p0*(1-p0)/tau2 - 1
	return math.Max(1, math.Min(50, m))
}

// Interval returns a central credible interval at the given level (e.g. 0.8) for a rate whose
// inferred value lies between loRate and hiRate, from n effective trials. The lower end uses the
// floor rate and the upper end the ceiling rate, so the interval covers both the inference
// ambiguity (did it sell?) and the sampling noise (few auctions). Jeffreys prior.
func Interval(loRate, hiRate, n, level float64) (float64, float64) {
	if n <= 0 {
		return 0, 1
	}
	tail := (1 - level) / 2
	lo := distuv.Beta{Alpha: clampRate(loRate)*n + 0.5, Beta: (1-clampRate(loRate))*n + 0.5}.Quantile(tail)
	hi := distuv.Beta{Alpha: clampRate(hiRate)*n + 0.5, Beta: (1-clampRate(hiRate))*n + 0.5}.Quantile(1 - tail)
	return lo, hi
}

func clampRate(r float64) float64 { return math.Max(0, math.Min(1, r)) }

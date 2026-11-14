// Package stats holds the small amount of statistics the advisor relies on: shrinking sparse
// rates toward a prior (empirical Bayes), Beta credible intervals, and a time-of-day demand
// profile for a non-homogeneous Poisson arrival model.
package stats

import (
	"math"
	"time"

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

// Profile is the share of a day's demand falling in each local hour (sums to 1).
type Profile [24]float64

// Flat is a profile with demand spread evenly over the day.
func Flat() Profile {
	var p Profile
	for i := range p {
		p[i] = 1.0 / 24
	}
	return p
}

// ProfileFrom turns per-hour sales counts into a profile, shrunk toward prior with strength m
// (in the same units as counts). Sparse items lean on the prior; busy items speak for themselves.
func ProfileFrom(counts [24]float64, prior Profile, m float64) Profile {
	var total float64
	for _, c := range counts {
		total += c
	}
	var p Profile
	for h := range p {
		p[h] = (counts[h] + m*prior[h]) / (total + m)
	}
	return p
}

// Lambda is the expected number of buyers' units over [start, start+hours] for an item selling
// perDay units a day, following the profile in loc (a non-homogeneous Poisson rate).
func (p Profile) Lambda(perDay float64, start time.Time, hours float64, loc *time.Location) float64 {
	if perDay <= 0 || hours <= 0 {
		return 0
	}
	t := start.In(loc)
	var lambda float64
	remaining := hours
	for remaining > 1e-9 {
		h := t.Hour()
		into := float64(t.Minute())/60 + float64(t.Second())/3600
		step := math.Min(1-into, remaining)
		lambda += perDay * p[h] * step
		remaining -= step
		t = t.Add(time.Duration(step * float64(time.Hour)))
	}
	return lambda
}

// HoursUntil is how long until the expected demand from start reaches units (capped at 60 days).
func (p Profile) HoursUntil(perDay float64, start time.Time, units float64, loc *time.Location) float64 {
	if units <= 0 {
		return 0
	}
	if perDay <= 0 {
		return math.Inf(1)
	}
	t := start.In(loc)
	var got, hours float64
	for hours < 24*60 {
		h := t.Hour()
		into := float64(t.Minute())/60 + float64(t.Second())/3600
		step := 1 - into
		rate := perDay * p[h] // units per hour
		if rate > 0 && got+rate*step >= units {
			return hours + (units-got)/rate
		}
		got += rate * step
		hours += step
		t = t.Add(time.Duration(step * float64(time.Hour)))
	}
	return math.Inf(1)
}

package stats

import (
	"math"
	"testing"
	"time"
)

func TestShrink(t *testing.T) {
	// One lucky auction (1/1) in a cell whose item sells 30% overall: stays near 30%.
	if got := Shrink(1, 1, 0.3, 10); math.Abs(got-(1+3)/11.0) > 1e-12 {
		t.Fatal(got)
	}
	// Plenty of data: the observation wins.
	if got := Shrink(0.8, 1000, 0.3, 10); got < 0.79 {
		t.Fatal(got)
	}
}

func TestEBStrength(t *testing.T) {
	// Identical rates: nothing to learn per group, pool hard.
	if m := EBStrength([]float64{0.3, 0.3, 0.3}, []float64{100, 100, 100}, 0.3); m != 50 {
		t.Fatalf("identical groups: m=%v", m)
	}
	// Very different rates with lots of data: weak prior.
	if m := EBStrength([]float64{0.05, 0.9, 0.1, 0.85}, []float64{500, 500, 500, 500}, 0.475); m > 5 {
		t.Fatalf("distinct groups: m=%v", m)
	}
}

func TestInterval(t *testing.T) {
	lo, hi := Interval(0.5, 0.5, 4, 0.8)
	lo2, hi2 := Interval(0.5, 0.5, 400, 0.8)
	if !(lo < lo2 && hi > hi2 && lo2 < 0.5 && hi2 > 0.5) {
		t.Fatalf("more data should narrow: n=4 [%v,%v] n=400 [%v,%v]", lo, hi, lo2, hi2)
	}
	// Inference ambiguity widens even with lots of data.
	lo3, hi3 := Interval(0.2, 0.7, 1000, 0.8)
	if lo3 > 0.22 || hi3 < 0.68 {
		t.Fatalf("[%v,%v]", lo3, hi3)
	}
}

func TestProfileLambda(t *testing.T) {
	loc := time.UTC
	flat := Flat()
	start := time.Date(2026, 11, 14, 10, 30, 0, 0, loc)
	if got := flat.Lambda(24, start, 6, loc); math.Abs(got-6) > 1e-9 {
		t.Fatalf("flat: %v", got)
	}
	// All demand at 20:00: an evening listing sees it, a morning one doesn't.
	var counts [24]float64
	counts[20] = 100
	p := ProfileFrom(counts, flat, 0)
	if got := p.Lambda(10, start, 6, loc); got != 0 {
		t.Fatalf("morning window: %v", got)
	}
	if got := p.Lambda(10, start, 11, loc); math.Abs(got-10) > 1e-9 { // 10:30 → 21:30 covers all of 20:00
		t.Fatalf("window covering 20:00: %v", got)
	}
	if h := p.HoursUntil(10, start, 5, loc); math.Abs(h-10) > 1e-9 { // 10:30 → 20:30
		t.Fatalf("hours until: %v", h)
	}
}

func TestProfileShrinks(t *testing.T) {
	var counts [24]float64
	counts[3] = 1 // one sale at 3 am
	p := ProfileFrom(counts, Flat(), 24)
	if p[3] > 0.1 {
		t.Fatalf("one sale shouldn't dominate: %v", p[3])
	}
	var sum float64
	for _, v := range p {
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatal(sum)
	}
}

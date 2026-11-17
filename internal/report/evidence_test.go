package report

import (
	"math"
	"testing"
)

// sold builds a sales ladder: price, units, sales.
func sold(l ...[3]int64) []SaleLevel {
	var out []SaleLevel
	for _, x := range l {
		out = addLevel(out, x[0], x[1], x[2])
	}
	return out
}

func TestStaleFairCappedByWhatBuyersPay(t *testing.T) {
	// The Formula: Lesser Mana Oil case: market value says 2.48g, but for a day every sale has
	// been at ~1.85g. Buying the 1.84g ones to relist under 2.42g is betting on last week.
	it := FlipItem{Item: 20754, Name: "Formula: Lesser Mana Oil", Houses: map[string]*FlipStat{
		"horde": {Fair: 24800, Min: 18400, Sales: 100, SalesPerDay: 40, RobustPerDay: 40,
			Book: []FlipLevel{{18400, 4}, {24200, 3}, {24400, 1}}},
	}}
	if f := plan([]FlipItem{it}, 40); len(f) != 1 {
		t.Fatalf("without evidence the stale market value makes it look good: %+v", f)
	}
	h := it.Houses["horde"]
	h.Evidence, h.Clearing = true, 18500
	h.Sold12 = sold([3]int64{18500, 20, 20})
	h.Sold48 = sold([3]int64{18500, 45, 45}, [3]int64{24800, 4, 4})
	if f := plan([]FlipItem{it}, 40); len(f) != 0 {
		t.Fatalf("capped at what buyers pay, there's no gap: %+v", f)
	}
}

func TestResaleNeedsRecentBuyersAtThePrice(t *testing.T) {
	// The Elixir of Nature Power case: 3g for days, then dumped to 0.88g. Nobody has paid near
	// 3g since: buying the dump to relist at 3g is no flip, however good the history looks.
	it := FlipItem{Item: 250343, Name: "Elixir of Nature Power", Houses: map[string]*FlipStat{
		"horde": {Fair: 30000, Min: 8700, Sales: 30, SalesPerDay: 10, RobustPerDay: 80, Evidence: true,
			Book:   []FlipLevel{{8700, 10}, {30000, 5}},
			Sold12: sold([3]int64{8800, 20, 3}),
			Sold48: sold([3]int64{8800, 30, 5}, [3]int64{29990, 40, 4})},
	}}
	if f := plan([]FlipItem{it}, 40); len(f) != 0 {
		t.Fatalf("no recent buyers at 3g: %+v", f)
	}
	var fn *FlipFunnel
	_, funnel, _ := PlanFlips(&FlipInputs{Houses: []FlipHouse{horde}, Items: []FlipItem{it}}, FlipOptions{Gold: g(40)})
	fn = funnel["horde → horde"]
	if fn.Unproven != 1 {
		t.Fatalf("funnel %+v", fn)
	}
	// Buyers back at 3g in the last 12 hours: now it is one.
	it.Houses["horde"].Sold12 = sold([3]int64{29990, 6, 3})
	if f := plan([]FlipItem{it}, 40); len(f) != 1 || f[0].RecentAbove != 3 {
		t.Fatalf("proven: %+v", f)
	}
}

func TestSlidingMarketLeftToActiveTraders(t *testing.T) {
	// The Elixir of Strength case: 60c to 12c in three days, the asking price falling ~40% a day.
	it := FlipItem{Item: 250349, Name: "Elixir of Strength", Houses: map[string]*FlipStat{
		"horde": {Fair: 1300, Min: 600, Sales: 60, SalesPerDay: 12, RobustPerDay: 200, Evidence: true, Trend: -0.4,
			Book:   []FlipLevel{{600, 10}, {1300, 50}},
			Sold12: sold([3]int64{1300, 20, 5}),
			Sold48: sold([3]int64{1300, 200, 20})},
	}}
	for _, r := range []Style{StyleCasual, StyleAway} {
		if f := planRisk([]FlipItem{it}, 40, r); len(f) != 0 {
			t.Fatalf("%s: %+v", r, f)
		}
	}
	// Active players can react, but the resale is priced where the slide will be: the ceiling
	// drops by 40% × 12 h window / 2.
	f := planRisk([]FlipItem{it}, 40, StyleActive)
	if len(f) != 1 || f[0].SellAt > int64(1300*(1-0.4*0.25)) {
		t.Fatalf("active: %+v", f)
	}
}

func TestBuySizedToBuyersAtThePrice(t *testing.T) {
	// 400 a day sell, but only 20 units in two days went at or above our price: a regular
	// trader's day of stock is 10, not 400.
	it := FlipItem{Item: 14, Name: "Mostly Cheap Buyers", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 300, Min: 100, Book: []FlipLevel{{100, 500}}},
		"horde": {Fair: 300, Min: 300, Sales: 500, SalesPerDay: 80, RobustPerDay: 400, Evidence: true, Clearing: 300,
			Sold12: sold([3]int64{300, 10, 5}),
			Sold48: sold([3]int64{300, 20, 10}, [3]int64{150, 700, 60})},
	}}
	f := plan([]FlipItem{it}, 40)
	if len(f) != 1 || f[0].BuyUnits != 10 {
		t.Fatalf("%+v", f)
	}
}

func TestFrontChanceBlendsWithTheHouse(t *testing.T) {
	house := &FrontStat{N: 1000, Sold: [4]float64{350, 450, 500, 550}}
	// No listings of its own: the house's rate for the interval.
	if p := frontChance(&FlipStat{}, house, 5, 0.3); math.Abs(p-0.45) > 1e-9 {
		t.Fatalf("house only: %v", p)
	}
	// 40 of its own, 15% sold within an hour: mostly its own rate.
	s := &FlipStat{Front: &FrontStat{N: 40, Sold: [4]float64{6, 8, 10, 12}}}
	if p := frontChance(s, house, 1, 0.3); math.Abs(p-(6+0.35*10)/50) > 1e-9 {
		t.Fatalf("blended: %v", p)
	}
	// Nothing measured anywhere: the fallback.
	if p := frontChance(&FlipStat{}, nil, 12, 0.3); p != 0.3 {
		t.Fatalf("fallback: %v", p)
	}
}

func TestPriceStepsRoundDown(t *testing.T) {
	for _, p := range []int64{1, 99, 100, 1000, 18500, 29990, 1234567} {
		b := priceStep(p)
		if b > p || float64(b) < float64(p)/bucketStep-1 {
			t.Fatalf("%d → %d", p, b)
		}
	}
	l := sold([3]int64{1000, 5, 1}, [3]int64{1010, 5, 1}, [3]int64{2000, 3, 2})
	if u, n := atOrAbove(l, 1000); u != 13 || n != 4 {
		t.Fatalf("everything is in 1000's step or above: %d %d (%+v)", u, n, l)
	}
	if u, n := atOrAbove(l, 1500); u != 3 || n != 2 {
		t.Fatalf("only the 2000 sales: %d %d", u, n)
	}
}

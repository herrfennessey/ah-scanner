package report

import "testing"

var (
	horde = FlipHouse{ID: 113, Faction: "horde", Cut: 0.05, Tracked: true}
	bw    = FlipHouse{ID: 116, Faction: "blackwater", Cut: 0.15, Tracked: true}
	ally  = FlipHouse{ID: 110, Faction: "alliance", Cut: 0.05, Tracked: true, ViewOnly: true}
)

func g(gold float64) int64 { return int64(gold * 10000) }

func plan(items []FlipItem, gold float64) []Flip {
	f, _ := PlanFlips(&FlipInputs{Houses: []FlipHouse{horde, bw, ally}, Items: items}, FlipOptions{Gold: g(gold)})
	return f
}

func TestRelistGimmeOnly(t *testing.T) {
	// Two bracers dumped at 2g; the rest of the market sits at 9g; market value 9g; sells 6/day.
	it := FlipItem{Item: 1, Name: "Magister's Bracers", Houses: map[string]*FlipStat{
		"horde": {Fair: g(9), Min: g(2), Sales: 40, SalesPerDay: 6, RobustPerDay: 6,
			Book: []FlipLevel{{g(2), 2}, {g(9), 30}, {g(9.5), 20}}},
	}}
	f := plan([]FlipItem{it}, 40)
	if len(f) != 1 || f[0].Kind != "relist" || f[0].BuyUnits != 2 || f[0].SellAt != g(9)-1 {
		t.Fatalf("want: buy the 2 cheap ones, relist under 9g; got %+v", f)
	}
}

func TestRelistNeverBuysOutTheMarket(t *testing.T) {
	// 30 bracers at 6g, market value 9g: buying them all costs 180g. With 40g, one flip may
	// use 20g; and only ~a day of sales (6) may be bought.
	it := FlipItem{Item: 1, Name: "Magister's Bracers", Houses: map[string]*FlipStat{
		"horde": {Fair: g(9), Min: g(6), Sales: 40, SalesPerDay: 6, RobustPerDay: 6,
			Book: []FlipLevel{{g(6), 30}, {g(9), 30}}},
	}}
	f := plan([]FlipItem{it}, 40)
	for _, x := range f {
		if x.BuyCost > g(20) || x.BuyUnits > 6 {
			t.Fatalf("over budget or over a day of sales: %+v", x)
		}
	}
	// Buying part of a level leaves the rest at 6g as competition: resale would be under 6g,
	// so it's no flip at all.
	if len(f) != 0 {
		t.Fatalf("partly buying a level isn't a gimme: %+v", f)
	}
}

func TestBudgetScalesTheFlip(t *testing.T) {
	it := FlipItem{Item: 2, Name: "Heavy Hide", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 1300, Min: 900, Book: []FlipLevel{{900, 500}}},
		"horde":      {Fair: 1300, Min: 1250, Sales: 200, SalesPerDay: 60, RobustPerDay: 400},
	}}
	small, big := plan([]FlipItem{it}, 5), plan([]FlipItem{it}, 200)
	if len(small) != 1 || len(big) != 1 || small[0].BuyUnits >= big[0].BuyUnits || small[0].BuyCost > g(2.5) {
		t.Fatalf("small %+v\nbig %+v", small, big)
	}
}

func TestAllianceRouteNeedsFastAllianceSales(t *testing.T) {
	mk := func(allySalesPerDay float64, allyPrice int64) FlipItem {
		return FlipItem{Item: 3, Name: "Elemental Air", Houses: map[string]*FlipStat{
			"horde":      {Fair: 2200, Min: 1500, Book: []FlipLevel{{1500, 20}}},
			"blackwater": {Fair: 1500, Min: 0, Sales: 1, SalesPerDay: 0.5, RobustPerDay: 2},
			"alliance":   {Fair: allyPrice, Min: allyPrice, Sales: 60, SalesPerDay: allySalesPerDay, RobustPerDay: 80},
		}}
	}
	// Alliance pays 31s and buys 20 times a day: sell on Blackwater just under 29.45s.
	f := plan([]FlipItem{mk(20, 3100)}, 40)
	if len(f) != 1 || f[0].Kind != "alliance" || f[0].SellAt != 2945 {
		t.Fatalf("fast Alliance demand should pass: %+v", f)
	}
	// Same prices, but Alliance barely buys it: no flip, whatever Blackwater's price.
	if f := plan([]FlipItem{mk(1, 3100)}, 40); len(f) != 0 {
		t.Fatalf("slow Alliance demand should block: %+v", f)
	}
	// Alliance cheaper at home than our buy price allows: no flip.
	if f := plan([]FlipItem{mk(20, 1600)}, 40); len(f) != 0 {
		t.Fatalf("cheap Alliance should block: %+v", f)
	}
}

func TestOneSaleIsNotEvidence(t *testing.T) {
	// The Jagged Arrow case: one 6,600-arrow sale on Blackwater.
	it := FlipItem{Item: 4, Name: "Jagged Arrow", Houses: map[string]*FlipStat{
		"horde":      {Fair: 6, Min: 6, Book: []FlipLevel{{6, 20000}}},
		"blackwater": {Fair: 60, Min: 60, Sales: 1, SalesPerDay: 1, SoldPerDay: 6600, RobustPerDay: 0},
	}}
	if f := plan([]FlipItem{it}, 40); len(f) != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestSmallSureFlipSurvivesBiggerBudget(t *testing.T) {
	it := FlipItem{Item: 5, Name: "Firefin Snapper", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 40, Min: 16, Book: []FlipLevel{{16, 471}}},
		"horde":      {Fair: 60, Min: 60, Sales: 200, SalesPerDay: 31, RobustPerDay: 600},
	}}
	for _, gold := range []float64{40, 200, 1000} {
		if f := plan([]FlipItem{it}, gold); len(f) != 1 {
			t.Fatalf("%.0fg: a sure ~1g flip should still show: %+v", gold, f)
		}
	}
}

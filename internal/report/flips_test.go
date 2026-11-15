package report

import "testing"

var (
	horde = FlipHouse{ID: 113, Faction: "horde", Cut: 0.05, Tracked: true}
	bw    = FlipHouse{ID: 116, Faction: "blackwater", Cut: 0.15, Tracked: true}
	ally  = FlipHouse{ID: 110, Faction: "alliance", Cut: 0.05, Tracked: true, ViewOnly: true}
)

func g(gold float64) int64 { return int64(gold * 10000) }

func plan(items []FlipItem, gold float64) []Flip { return planRisk(items, gold, RiskLow) }

func planRisk(items []FlipItem, gold float64, risk Risk) []Flip {
	f, _, _ := PlanFlips(&FlipInputs{Houses: []FlipHouse{horde, bw, ally}, Items: items}, FlipOptions{Gold: g(gold), Risk: risk})
	return f
}

func TestRelistGimmeOnly(t *testing.T) {
	// Two dumped at 2g; the rest of the market sits at 9g; market value 9g; sells 300/day,
	// so the 52 units near 9g clear in about four hours.
	it := FlipItem{Item: 1, Name: "Mageweave Cloth", Houses: map[string]*FlipStat{
		"horde": {Fair: g(9), Min: g(2), Sales: 400, SalesPerDay: 60, RobustPerDay: 300,
			Book: []FlipLevel{{g(2), 2}, {g(9), 30}, {g(9.5), 20}}},
	}}
	f := plan([]FlipItem{it}, 40)
	if len(f) != 1 || f[0].Kind != "relist" || f[0].BuyUnits != 2 || f[0].SellAt != g(9)-1 {
		t.Fatalf("want: buy the 2 cheap ones, relist under 9g; got %+v", f)
	}
}

func TestRelistNeverBuysOutTheMarket(t *testing.T) {
	// 30 bracers at 6g, market value 9g: buying them all costs 180g. Even at high risk one
	// flip may use 24g of 40g, and only ~1.5 days of sales (9) may be bought.
	it := FlipItem{Item: 1, Name: "Magister's Bracers", Houses: map[string]*FlipStat{
		"horde": {Fair: g(9), Min: g(6), Sales: 40, SalesPerDay: 6, RobustPerDay: 6,
			Book: []FlipLevel{{g(6), 30}, {g(9), 30}}},
	}}
	f := planRisk([]FlipItem{it}, 40, RiskHigh)
	for _, x := range f {
		if x.BuyCost > g(24) || x.BuyUnits > 9 {
			t.Fatalf("over budget or over a day of sales: %+v", x)
		}
	}
	// Buying part of a level leaves the rest at 6g as competition: resale would be under 6g,
	// so it's no flip at all.
	if len(f) != 0 {
		t.Fatalf("partly buying a level isn't a gimme: %+v", f)
	}
	// With gold to buy all 30, the 60 units then near 9g are ~10 days of sales: still no flip.
	if f := planRisk([]FlipItem{it}, 1000, RiskHigh); len(f) != 0 {
		t.Fatalf("bought out the market: %+v", f)
	}
}

func TestRelistBandJudgedAtTheResalePrice(t *testing.T) {
	// Dumped at 2g, next at 5g, a wall at 9g (market value). Resale is just under 5g, where only
	// 12 units compete; the 500 at 9g are beyond the band and must not block the flip.
	it := FlipItem{Item: 10, Name: "Wall", Houses: map[string]*FlipStat{
		"horde": {Fair: g(9), Min: g(2), Sales: 400, SalesPerDay: 60, RobustPerDay: 60,
			Book: []FlipLevel{{g(2), 2}, {g(5), 10}, {g(9), 500}}},
	}}
	if f := plan([]FlipItem{it}, 40); len(f) != 1 || f[0].SellAt != g(5)-1 || f[0].Competing != 10 {
		t.Fatalf("%+v", f)
	}
}

func TestCompetitionOnPriceOnlyHouse(t *testing.T) {
	// No book on a price_only sell house: the listed count near the price is the competition.
	s := &FlipStat{Min: 1000, Listed: 40}
	if n := nearSupply(s, 999, 0); n != 40 {
		t.Fatalf("got %d", n)
	}
	if n := nearSupply(s, 500, 0); n != 0 {
		t.Fatalf("got %d", n)
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
			t.Fatalf("%.0fg: a sure, quick ~60s flip should still show: %+v", gold, f)
		}
	}
}

func TestRelistRespectsListingsBeyondCachedBook(t *testing.T) {
	// Codex's case: 40 one-unit levels at 1,000–1,039c are cached, a 41st sits at 1,040c,
	// market value 10,000c. Buying all 40 must not price the relist at market value.
	var book []FlipLevel
	for p := int64(1000); p < 1040; p++ {
		book = append(book, FlipLevel{P: p, Q: 1})
	}
	it := FlipItem{Item: 6, Name: "Cached Book", Houses: map[string]*FlipStat{
		"horde": {Fair: 10000, Min: 1000, Sales: 100, SalesPerDay: 40, RobustPerDay: 400, Book: book, Beyond: 1040},
	}}
	if f := plan([]FlipItem{it}, 40); len(f) != 0 {
		t.Fatalf("the 1,040c listing beyond the cache is the real competition: %+v", f)
	}
	// Without a level beyond the cache (the book really ends), the market-value ceiling applies.
	it.Houses["horde"].Beyond = 0
	if f := plan([]FlipItem{it}, 40); len(f) != 1 || f[0].SellAt != 10000 {
		t.Fatalf("complete book: %+v", f)
	}
}

func TestSlowRecipeOnlyAtHighRisk(t *testing.T) {
	// The cooking recipe case: 22 listed at 1.77g, ~6 sell a day (8 separate sales). A cheap
	// one on Blackwater looks like a 1g+ profit, but ours would wait behind ~3.5 days of supply.
	var book []FlipLevel
	book = append(book, FlipLevel{P: 17700, Q: 22})
	it := FlipItem{Item: 5482, Name: "Recipe: Kaldorei Spider Kabob", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 9000, Min: 3500, Book: []FlipLevel{{3500, 3}}},
		"horde":      {Fair: 17700, Min: 17700, Listed: 22, Sales: 60, SalesPerDay: 8, RobustPerDay: 6, Book: book},
	}}
	for _, r := range []Risk{RiskLow, RiskMedium, RiskHigh} {
		if f := planRisk([]FlipItem{it}, 40, r); len(f) != 0 {
			t.Fatalf("%s: a recipe with days of supply ahead of it is no flip: %+v", r, f)
		}
	}
	// With only 4 competing at the price, high risk takes it; low still won't (8 sales a day).
	it.Houses["horde"].Book = []FlipLevel{{17700, 4}}
	if f := planRisk([]FlipItem{it}, 40, RiskHigh); len(f) != 1 || f[0].Competing != 4 {
		t.Fatalf("high: %+v", f)
	}
	if f := planRisk([]FlipItem{it}, 40, RiskLow); len(f) != 0 {
		t.Fatalf("low: %+v", f)
	}
}

func TestCompetitionShrinksTheBuy(t *testing.T) {
	// Horde sells 400 a day (100 per 6 h); 70 are already listed just above our price, so a
	// low-risk flip may only add 30.
	it := FlipItem{Item: 7, Name: "Heavy Leather", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 300, Min: 100, Book: []FlipLevel{{100, 500}}},
		"horde":      {Fair: 300, Min: 250, Sales: 500, SalesPerDay: 80, RobustPerDay: 400, Book: []FlipLevel{{250, 70}, {400, 900}}},
	}}
	f := plan([]FlipItem{it}, 40)
	if len(f) != 1 || f[0].BuyUnits != 30 || f[0].Competing != 70 || f[0].DaysToSell != 0.25 {
		t.Fatalf("%+v", f)
	}
}

func TestRankedByProfitPerDay(t *testing.T) {
	fast := FlipItem{Item: 8, Name: "Fast", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 300, Min: 100, Book: []FlipLevel{{100, 50}}},
		"horde":      {Fair: 300, Min: 300, Sales: 500, SalesPerDay: 80, RobustPerDay: 2000},
	}}
	slow := FlipItem{Item: 9, Name: "Slow", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 30000, Min: 10000, Book: []FlipLevel{{10000, 1}}},
		"horde":      {Fair: 30000, Min: 30000, Sales: 50, SalesPerDay: 5, RobustPerDay: 5},
	}}
	f := planRisk([]FlipItem{slow, fast}, 40, RiskHigh)
	if len(f) != 2 || f[0].Name != "Fast" || f[0].Profit >= f[1].Profit {
		t.Fatalf("the quick small flip should rank first: %+v", f)
	}
}

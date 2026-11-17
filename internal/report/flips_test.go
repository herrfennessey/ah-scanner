package report

import (
	"math"
	"testing"
)

var (
	horde = FlipHouse{ID: 113, Faction: "horde", Cut: 0.05, Tracked: true}
	bw    = FlipHouse{ID: 116, Faction: "blackwater", Cut: 0.15, Tracked: true}
	ally  = FlipHouse{ID: 110, Faction: "alliance", Cut: 0.05, Tracked: true, ViewOnly: true}
)

func g(gold float64) int64 { return int64(gold * 10000) }

func plan(items []FlipItem, gold float64) []Flip { return planRisk(items, gold, StyleCasual) }

func planRisk(items []FlipItem, gold float64, style Style) []Flip {
	f, _, _ := PlanFlips(&FlipInputs{Houses: []FlipHouse{horde, bw, ally}, Items: items}, FlipOptions{Gold: g(gold), Style: style})
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
	f := planRisk([]FlipItem{it}, 40, StyleActive)
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
	if f := planRisk([]FlipItem{it}, 1000, StyleActive); len(f) != 0 {
		t.Fatalf("bought out the market: %+v", f)
	}
}

func TestRelistBandJudgedAtTheResalePrice(t *testing.T) {
	// Dumped at 2g, next at 5g (about market value), a wall at 9g. Resale is just under 5g, where
	// only 12 units compete; the 500 at 9g are beyond the band and must not block the flip.
	it := FlipItem{Item: 10, Name: "Wall", Houses: map[string]*FlipStat{
		"horde": {Fair: g(5.5), Min: g(2), Sales: 400, SalesPerDay: 60, RobustPerDay: 60,
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
	// Alliance pays 31s and buys 20 times a day: sell on Blackwater just under 29.45s. Selling on
	// Blackwater needs a player who comes back (not away).
	if f := planRisk([]FlipItem{mk(20, 3100)}, 40, StyleAway); len(f) != 0 {
		t.Fatalf("away sells on Horde only: %+v", f)
	}
	f := planRisk([]FlipItem{mk(20, 3100)}, 40, StyleCasual)
	if len(f) != 1 || f[0].Kind != "alliance" || f[0].SellAt != 2945 {
		t.Fatalf("fast Alliance demand should pass: %+v", f)
	}
	// Same prices, but Alliance barely buys it: no flip, whatever Blackwater's price.
	if f := planRisk([]FlipItem{mk(1, 3100)}, 40, StyleCasual); len(f) != 0 {
		t.Fatalf("slow Alliance demand should block: %+v", f)
	}
	// Alliance cheaper at home than our buy price allows: no flip.
	if f := planRisk([]FlipItem{mk(20, 1600)}, 40, StyleCasual); len(f) != 0 {
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

func TestSlowRecipeNeedsADay(t *testing.T) {
	// The cooking recipe case: 22 listed at 1.77g, ~6 sell a day (8 separate sales). A cheap
	// one on Blackwater looks like a 1g+ profit, but ours would wait behind ~3.5 days of supply.
	var book []FlipLevel
	book = append(book, FlipLevel{P: 17700, Q: 22})
	it := FlipItem{Item: 5482, Name: "Recipe: Kaldorei Spider Kabob", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 9000, Min: 3500, Book: []FlipLevel{{3500, 3}}},
		"horde":      {Fair: 17700, Min: 17700, Listed: 22, Sales: 60, SalesPerDay: 8, RobustPerDay: 6, Book: book},
	}}
	for _, r := range Styles {
		if f := planRisk([]FlipItem{it}, 40, r); len(f) != 0 {
			t.Fatalf("%s: a recipe with days of supply ahead of it is no flip: %+v", r, f)
		}
	}
	// With only 4 competing at the price, a day of buyers takes ours too: every style posts a
	// 24h listing that can wait for them.
	it.Houses["horde"].Book = []FlipLevel{{17700, 4}}
	for _, r := range Styles {
		if f := planRisk([]FlipItem{it}, 40, r); len(f) != 1 || f[0].Competing != 4 {
			t.Fatalf("%s: %+v", r, f)
		}
	}
}

func TestCompetitionShrinksTheBuy(t *testing.T) {
	// Horde sells 400 a day; 70 are already listed just above our price, so a flip that should
	// be gone within the day may only add 330.
	it := FlipItem{Item: 7, Name: "Heavy Leather", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 300, Min: 100, Book: []FlipLevel{{100, 500}}},
		"horde":      {Fair: 260, Min: 250, Sales: 500, SalesPerDay: 80, RobustPerDay: 400, Book: []FlipLevel{{250, 70}, {400, 900}}},
	}}
	f := planRisk([]FlipItem{it}, 40, StyleActive)
	if len(f) != 1 || f[0].BuyUnits != 330 || f[0].Competing != 70 || f[0].DaysToSell != 1 {
		t.Fatalf("%+v", f)
	}
}

func TestRankedByExpectedProfit(t *testing.T) {
	fast := FlipItem{Item: 8, Name: "Fast", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 300, Min: 100, Book: []FlipLevel{{100, 50}}},
		"horde": {Fair: 300, Min: 300, Sales: 500, SalesPerDay: 80, RobustPerDay: 2000,
			Front: &FrontStat{N: 50, Sold: [4]float64{45, 49, 50, 50}}},
	}}
	slow := FlipItem{Item: 9, Name: "Slow", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 30000, Min: 10000, Book: []FlipLevel{{10000, 1}}},
		"horde": {Fair: 30000, Min: 30000, Sales: 50, SalesPerDay: 6, RobustPerDay: 5,
			Front: &FrontStat{N: 50, Sold: [4]float64{2, 10, 20, 30}}},
	}}
	// Fast sells within the hour for a little; Slow takes most of a day for more. Gold sitting
	// idle isn't the limit, so the bigger expected profit comes first.
	f := planRisk([]FlipItem{fast, slow}, 40, StyleActive)
	if len(f) != 2 || f[0].Name != "Slow" || f[0].Expected <= f[1].Expected || f[0].ProfitPerDay >= f[1].ProfitPerDay {
		t.Fatalf("the bigger expected profit should rank first: %+v", f)
	}
}

func TestRelistPriceFoundBeforeSupplyCheck(t *testing.T) {
	// Codex's case: 8g, low risk, 40 sales a day; one unit at 2g, one at 4g and 100 at 9g (market
	// value 4.2g, so 4g isn't a slide). Buy the 2g unit and relist just under 4g: only the 4g
	// unit competes.
	it := FlipItem{Item: 10, Name: "Codex Case", Houses: map[string]*FlipStat{
		"horde": {Fair: g(4.2), Min: g(2), Sales: 100, SalesPerDay: 40, RobustPerDay: 40,
			Book: []FlipLevel{{g(2), 1}, {g(4), 1}, {g(9), 100}}},
	}}
	f := plan([]FlipItem{it}, 8)
	if len(f) != 1 || f[0].BuyUnits != 1 || f[0].SellAt != g(4)-1 || f[0].Competing != 1 {
		t.Fatalf("%+v", f)
	}
}

func TestBlackwaterCappedByHomePrices(t *testing.T) {
	// The Dreamfoil case: Blackwater's market value (37s) is left over from yesterday; 21 sit at
	// 20.95s, but Horde has 2,600 at 15.80s and Alliance 485 at 16.65s. Nobody pays 37s.
	it := FlipItem{Item: 13463, Name: "Dreamfoil", Houses: map[string]*FlipStat{
		"blackwater": {Fair: 3719, Min: 2095, Listed: 38, Sales: 30, SalesPerDay: 35, RobustPerDay: 190,
			Book: []FlipLevel{{2095, 21}, {5619, 17}}},
		"horde":    {Fair: 1910, Min: 1580, Listed: 2592, Sales: 255, SalesPerDay: 295, RobustPerDay: 3780, Book: []FlipLevel{{1580, 24}, {1600, 583}}},
		"alliance": {Fair: 1697, Min: 1665, Listed: 485, Sales: 18, SalesPerDay: 52, RobustPerDay: 545},
	}}
	for _, r := range []Style{StyleAway, StyleCasual, StyleActive} {
		for _, f := range planRisk([]FlipItem{it}, 40, r) {
			if f.SellHouse == "blackwater" && f.SellAt >= 1665 {
				t.Fatalf("%s: sells on Blackwater above both home prices: %+v", r, f)
			}
		}
	}
	// With none listed on the Alliance side, Alliance buyers have no cheaper option: no cap.
	it.Houses["alliance"].Min = 0
	if c := blackwaterHomeCap(it, true); c != 0 {
		t.Fatalf("cap %d", c)
	}
}

func TestFallingMarketOnlyAtHighRisk(t *testing.T) {
	// The Giant Egg case: Horde slid from 17s to 8s and Alliance from 25s to 20s (market value
	// 24.11s). Reselling on Blackwater at 19s, just under a falling Alliance price, is riding a
	// slide: not a low or medium risk flip.
	it := FlipItem{Item: 12207, Name: "Giant Egg", Houses: map[string]*FlipStat{
		"horde":      {Fair: 1398, Min: 795, Sales: 145, SalesPerDay: 60, RobustPerDay: 1000, Book: []FlipLevel{{795, 56}, {895, 149}}},
		"blackwater": {Fair: 2499, Min: 2395, Sales: 88, SalesPerDay: 60, RobustPerDay: 190, Book: []FlipLevel{{2395, 82}}},
		"alliance":   {Fair: 2411, Min: 2000, Sales: 21, SalesPerDay: 20, RobustPerDay: 100},
	}}
	for _, r := range []Style{StyleAway, StyleCasual} {
		if f := planRisk([]FlipItem{it}, 40, r); len(f) != 0 {
			t.Fatalf("%s: %+v", r, f)
		}
	}
	hasAlliance := func(f []Flip) bool {
		for _, x := range f {
			if x.Kind == "alliance" {
				return true
			}
		}
		return false
	}
	if f := planRisk([]FlipItem{it}, 40, StyleActive); !hasAlliance(f) {
		t.Fatalf("high risk accepts the slide: %+v", f)
	}
	// Steady Alliance price (market value 21s): the same flip is fine at medium risk.
	it.Houses["alliance"].Fair = 2100
	if f := planRisk([]FlipItem{it}, 40, StyleCasual); !hasAlliance(f) {
		t.Fatalf("steady: %+v", f)
	}
}

func TestHighDepositBlackwaterSkipped(t *testing.T) {
	// The Vibrant Plume case: 8s on Horde, ~17s on Blackwater and ~18.7s Alliance, but it vendors
	// for 8s25c, so a Blackwater listing costs ~8s25c of deposit per unit (5× the faction
	// house's 1s65c). Undercut once and the deposit wipes out the margin.
	bwDep := FlipHouse{ID: 116, Faction: "blackwater", Cut: 0.15, Tracked: true, DepositMult: 5}
	it := FlipItem{Item: 5117, Name: "Vibrant Plume", Deposit: 165, Houses: map[string]*FlipStat{
		"horde":      {Fair: 877, Min: 804, Sales: 400, SalesPerDay: 180, RobustPerDay: 1500, Book: []FlipLevel{{804, 200}}},
		"blackwater": {Fair: 1700, Min: 1700, Sales: 100, SalesPerDay: 60, RobustPerDay: 400},
		"alliance":   {Fair: 1874, Min: 1874, Sales: 59, SalesPerDay: 60, RobustPerDay: 400},
	}}
	in := &FlipInputs{Houses: []FlipHouse{horde, bwDep, ally}, Items: []FlipItem{it}}
	for _, r := range []Style{StyleAway, StyleCasual, StyleActive} {
		f, _, _ := PlanFlips(in, FlipOptions{Gold: g(40), Style: r})
		for _, x := range f {
			if x.SellHouse == "blackwater" {
				t.Fatalf("%s: %+v", r, x)
			}
		}
	}
	// The same flip with a faction-house deposit would pass: the deposit is what stops it.
	bwDep.DepositMult = 1
	in.Houses[1] = bwDep
	if f, _, _ := PlanFlips(in, FlipOptions{Gold: g(40), Style: StyleActive}); len(f) == 0 {
		t.Fatal("with a 1x deposit the flip should exist; the test isn't testing the deposit")
	}
}

func TestDepositsWeighOnExpectedProfit(t *testing.T) {
	// 4 units bought at 1g, sold at ~2g with a 40s deposit each: 2g profit if it all sells,
	// less once missed rounds lose 1.6g of deposit each. A 90s deposit eats the margin.
	mk := func(dep int64) FlipItem {
		return FlipItem{Item: 11, Name: "Deposit Heavy", Deposit: dep, Houses: map[string]*FlipStat{
			"blackwater": {Fair: g(2), Min: g(1), Book: []FlipLevel{{g(1), 4}}},
			"horde":      {Fair: g(2), Min: g(2), Sales: 200, SalesPerDay: 40, RobustPerDay: 400},
		}}
	}
	for _, r := range Styles {
		f := planRisk([]FlipItem{mk(4000)}, 40, r)
		light := planRisk([]FlipItem{mk(100)}, 40, r)
		if len(f) != 1 || len(light) != 1 || f[0].DepositAtRisk != 16000 || f[0].Expected > f[0].Profit || f[0].Expected >= light[0].Expected {
			t.Fatalf("%s: %+v (light deposit %+v)", r, f, light)
		}
		if f := planRisk([]FlipItem{mk(9000)}, 40, r); len(f) != 0 {
			t.Fatalf("%s: deposit eats it: %+v", r, f)
		}
	}
}

func TestBufferPicksTheUnitsThatSurviveIt(t *testing.T) {
	// Codex's case: one unit at 10s and seven at 1.3g on Blackwater, resale ~2g on Horde, 40s
	// deposit per unit. Low risk must buy just the 10s unit (~60s after three lost deposits),
	// not reject the item because the 1.3g units can't carry the buffer.
	it := FlipItem{Item: 12, Name: "Codex Buffer", Deposit: 4000, Houses: map[string]*FlipStat{
		"blackwater": {Fair: g(2), Min: 1000, Book: []FlipLevel{{1000, 1}, {g(1.3), 7}}},
		"horde":      {Fair: g(2), Min: g(2), Sales: 200, SalesPerDay: 40, RobustPerDay: 400},
	}}
	f := plan([]FlipItem{it}, 40)
	if len(f) != 1 || f[0].BuyUnits != 1 || f[0].Profit-2*f[0].DepositAtRisk < 1000 {
		t.Fatalf("%+v", f)
	}
}

func TestExpectedProfitWeighsRelists(t *testing.T) {
	// Sure to sell: expected equals gross - cost, in one round.
	if ev, r, all := expectedProfit(10000, 6000, 500, 1, 1, 0, 5, 0); ev != 4000 || r != 1 || all != 1 {
		t.Fatalf("sure: %d %v %v", ev, r, all)
	}
	// Half the time a round fails: some deposits and undercuts are expected, more rounds too.
	ev, r, _ := expectedProfit(10000, 6000, 500, 0.5, 1, 0, 5, 6000)
	if ev >= 4000 || ev < 3000 || r <= 1.5 {
		t.Fatalf("coin flip: %d %v", ev, r)
	}
	// A slow seller with a big deposit is worth less than the same flip that sells fast.
	slow, _, _ := expectedProfit(10000, 6000, 2000, 0.1, 1, 0, 5, 6000)
	fast, _, _ := expectedProfit(10000, 6000, 2000, 0.5, 1, 0, 5, 6000)
	if slow >= fast {
		t.Fatalf("slow %d fast %d", slow, fast)
	}
}

func TestExpectedProfitChargesOnlyRealRelists(t *testing.T) {
	// Visiting is free: a listing that's never undercut and doesn't run out keeps its price and
	// deposit, however often you look. One that's always undercut pays for every round.
	calm, _, _ := expectedProfit(10000, 6000, 500, 0.2, 0, 0, 12, 3000)
	chased, _, _ := expectedProfit(10000, 6000, 500, 0.2, 1, 0, 12, 3000)
	if calm <= chased {
		t.Fatalf("calm %d chased %d", calm, chased)
	}
	// More visits at the same per-round chance never hurt a listing nobody undercuts.
	few, _, _ := expectedProfit(10000, 6000, 500, 0.2, 0, 0, 5, 3000)
	if calm < few {
		t.Fatalf("12 rounds %d < 5 rounds %d", calm, few)
	}
	// A listing that runs out every round loses its deposit each time, at the same price.
	lapsing, _, _ := expectedProfit(10000, 6000, 500, 0.2, 0, 1, 12, 3000)
	if lapsing >= calm || lapsing <= chased {
		t.Fatalf("lapsing %d (calm %d, chased %d)", lapsing, calm, chased)
	}
}

func TestExpectedProfitValuesLeftoverStock(t *testing.T) {
	// One round (away), 20% chance: 80% of the time the stock is left at the going price. If
	// that's under cost, the flip loses money on average; the old model assumed a last sale.
	ev, _, all := expectedProfit(10000, 6000, 100, 0.2, 1, 0, 1, 3000)
	if ev >= 0 || math.Abs(all-0.2) > 1e-9 {
		t.Fatalf("leftover under cost: %d (sold out %.2f)", ev, all)
	}
	// More visits at the same per-visit chance sell more of it and earn more.
	more, _, all5 := expectedProfit(10000, 6000, 100, 0.2, 1, 0, 12, 3000)
	if more <= ev || all5 <= 0.9 {
		t.Fatalf("12 visits: %d (sold out %.2f)", more, all5)
	}
}

func TestOddsLookups(t *testing.T) {
	if sellChance("horde", 0.1) != 0.51 || sellChance("horde", 2) != 0.16 || sellChance("blackwater", 10) != 0.09 {
		t.Fatal("sell chance table")
	}
	one := &FlipStat{Fair: 1000, Book: []FlipLevel{{400, 1}}}
	if goneSoon("horde", one) != 0.51 {
		t.Fatal("single unit at 40% of market on Horde")
	}
	if goneSoon("horde", &FlipStat{Fair: 1000, Book: []FlipLevel{{950, 1}}}) != 0 {
		t.Fatal("near market isn't in the measurements")
	}
}

func TestMinimumDepositPerAuction(t *testing.T) {
	// A tiny deposit (0.2c a unit) still costs 1s per auction.
	it := FlipItem{Item: 13, Name: "Cheap Fish", Deposit: 0, Houses: map[string]*FlipStat{
		"blackwater": {Fair: 100, Min: 20, Book: []FlipLevel{{20, 100}}},
		"horde":      {Fair: 100, Min: 100, Sales: 900, SalesPerDay: 300, RobustPerDay: 2000},
	}}
	f := plan([]FlipItem{it}, 40)
	if len(f) != 1 || f[0].DepositAtRisk != 100 {
		t.Fatalf("%+v", f)
	}
}

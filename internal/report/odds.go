package report

import "math"

// Measured odds behind expected profit. Source: the NUC database, 2026-11-14 10:33 to
// 2026-11-15 13:19 UTC (about 27 hours), every listing that was the cheapest of its item when
// posted. They are early figures; rerun the backtest as history grows.

// minDeposit is the smallest deposit an auction costs (1s), seen in Journalator posting logs.
const minDeposit = 100

// undercutRatio is the median new/old price when a newer listing undercuts the cheapest one
// (Horde 0.969, Blackwater 0.964): what a relist gives up to be cheapest again.
const undercutRatio = 0.967

// sellChanceTable: share of cheapest-when-posted listings that sold within 12 hours, by the
// sell-out time (days) of the supply near their price, per house.
var sellChanceTable = map[string][]struct{ days, p float64 }{
	"horde":      {{0.25, 0.51}, {0.5, 0.41}, {1, 0.35}, {1.5, 0.22}, {3, 0.16}, {math.Inf(1), 0.09}},
	"blackwater": {{0.25, 0.48}, {0.5, 0.36}, {1, 0.32}, {1.5, 0.23}, {3, 0.15}, {math.Inf(1), 0.09}},
}

// sellChance is the measured chance one listing round sells before it's undercut or expires.
func sellChance(house string, days float64) float64 {
	t, ok := sellChanceTable[house]
	if !ok {
		t = sellChanceTable["horde"]
	}
	for _, b := range t {
		if days <= b.days {
			return b.p
		}
	}
	return t[len(t)-1].p
}

// goneSoonTable: share of listings at a given discount to market value (price/market up to
// the bound) and lot size that were gone within an hour of first being seen.
var goneSoonTable = map[string][]struct {
	ratio float64
	byQty [4]float64 // 1 unit, 2-5, 6-20, more
}{
	"horde":      {{0.5, [4]float64{0.51, 0.39, 0.31, 0.28}}, {0.7, [4]float64{0.40, 0.41, 0.48, 0.46}}, {0.85, [4]float64{0.39, 0.50, 0.54, 0.50}}},
	"blackwater": {{0.5, [4]float64{0.35, 0.39, 0.38, 0.21}}, {0.7, [4]float64{0.32, 0.42, 0.39, 0.31}}, {0.85, [4]float64{0.29, 0.39, 0.43, 0.32}}},
}

// goneSoon is the measured chance the cheapest listing on the buy side is gone within an hour.
func goneSoon(house string, b *FlipStat) float64 {
	t, ok := goneSoonTable[house]
	if !ok || len(b.Book) == 0 || b.Fair <= 0 {
		return 0
	}
	l := b.Book[0]
	r := float64(l.P) / float64(b.Fair)
	q := 3
	switch {
	case l.Q <= 1:
		q = 0
	case l.Q <= 5:
		q = 1
	case l.Q <= 20:
		q = 2
	}
	for _, row := range t {
		if r <= row.ratio {
			return row.byQty[q]
		}
	}
	return 0 // not underpriced enough to be in the measurements
}

// expectedProfit weighs the listing rounds of a play: each sells with chance p. A round that
// doesn't is undercut with chance under (the stock is relisted one typical undercut lower,
// losing the deposit), or else its listing runs out with chance expire (relisted at the same
// price, losing the deposit). Stock still unsold after the last round is worth leftover (its
// value at the going price), one more deposit lost. Returns the expected profit, the expected
// number of rounds held (for the holding time) and the chance it all sells within the rounds.
func expectedProfit(gross, cost, deposit int64, p, under, expire float64, rounds int, leftover int64) (int64, float64, float64) {
	var ev, held, reach float64 = 0, 0, 1 // reach: chance of getting to this round unsold
	price, lost := 1.0, 0.0               // expected price multiplier and deposits lost so far, given unsold
	relist := under + (1-under)*expire
	for k := 0; k < rounds; k++ {
		sell := reach * p
		ev += sell * (float64(gross)*price - float64(cost) - float64(deposit)*lost)
		held += sell * float64(k+1)
		reach -= sell
		if k < rounds-1 {
			price *= 1 - under*(1-undercutRatio)
			lost += relist
		}
	}
	ev += reach * (float64(leftover) - float64(cost) - float64(deposit)*(lost+1))
	held += reach * float64(rounds)
	return int64(math.Round(ev)), held, 1 - reach
}

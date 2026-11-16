package report

import "math"

// Market hints: hold or chase. Measured on the NUC database (2026-11-14 10:33 to 2026-11-16
// 07:20 UTC), every item scan with ≥10 earlier scans in the previous 3–36 hours; outcome = the
// item's p20 price 12 hours later. "Usual" was the median p20 of those earlier scans; the live
// hint uses the 7-day market value. Early figures: rerun the backtest as history grows.
//
//	                                       Horde              Blackwater
//	p20 ≥ 1.5× usual                       87% fell ≥10%      69%
//	p20 1.25–1.5× usual                    68%                53%
//	p20 falling ≥7% in ~3 h, listed ≥1.3×  59%                50%
//	p20 flat (±1%) within 10% of usual     71% stayed ±10%    58%
var hintOdds = map[string]map[string]float64{
	"horde":      {"sell_high": 0.87, "sell": 0.68, "chase": 0.59, "hold": 0.71},
	"blackwater": {"sell_high": 0.69, "sell": 0.53, "chase": 0.50, "hold": 0.58},
}

const (
	stretchSell   = 1.25 // p20 this far above usual: expect a fall
	stretchHigh   = 1.5
	fallingRatio  = 0.93 // p20 vs ~3 hours ago
	pilingRatio   = 1.3  // listed vs usual, for a falling market
	piledRatio    = 1.5  // listed vs usual, mentioned with a stretched price
	flatBand      = 0.01 // p20 vs ~3 hours ago
	usualBand     = 0.10 // p20 vs usual, for hold
	slowClearDays = 3    // listed stock outlasts this many days of certain sales
)

// HintInput is one item on one house: the latest scan, a scan about 3 hours earlier, and its
// 7-day usual levels.
type HintInput struct {
	P20, PrevP20 int64   // price at which the cheapest 20% of units clear, now and ~3 h ago
	Listed       int64   // units listed now
	Fair         int64   // 7-day market value (median p20)
	UsualListed  float64 // average units listed per day
	SoldPerDay   float64 // certain sales per day (floor)
}

// Hint says whether to sell into the market now, chase it down, hold, or stop reposting.
type Hint struct {
	Kind      string  `json:"kind"`                 // sell | chase | hold | slow
	Ratio     float64 `json:"ratio"`                // p20 / usual
	Odds      float64 `json:"odds,omitempty"`       // measured chance behind the hint (see hintOdds)
	Piling    bool    `json:"piling,omitempty"`     // listings well above usual
	ClearDays float64 `json:"clear_days,omitempty"` // slow: days of sales to clear what's listed (0 = no sales)
}

// MarketHint returns the hint for an item, or nil when nothing stands out (or there's no
// history to judge by yet).
func MarketHint(house string, in HintInput) *Hint {
	if in.P20 <= 0 || in.Fair <= 0 || in.Listed <= 0 {
		return nil
	}
	odds := hintOdds[house]
	if odds == nil {
		odds = hintOdds["horde"]
	}
	r := float64(in.P20) / float64(in.Fair)
	growth := 0.0
	if in.UsualListed > 0 {
		growth = float64(in.Listed) / in.UsualListed
	}
	h := &Hint{Ratio: math.Round(r*100) / 100}
	switch {
	case r >= stretchSell:
		h.Kind, h.Odds, h.Piling = "sell", odds["sell"], growth >= piledRatio
		if r >= stretchHigh {
			h.Odds = odds["sell_high"]
		}
		return h
	case in.PrevP20 > 0 && float64(in.P20)/float64(in.PrevP20) <= fallingRatio && growth >= pilingRatio:
		h.Kind, h.Odds, h.Piling = "chase", odds["chase"], true
		return h
	}
	if in.SoldPerDay <= 0 || float64(in.Listed)/in.SoldPerDay > slowClearDays {
		h.Kind = "slow"
		if in.SoldPerDay > 0 {
			h.ClearDays = math.Round(float64(in.Listed)/in.SoldPerDay*10) / 10
		}
		return h
	}
	if in.PrevP20 > 0 && math.Abs(float64(in.P20)/float64(in.PrevP20)-1) <= flatBand && math.Abs(r-1) <= usualBand {
		h.Kind, h.Odds = "hold", odds["hold"]
		return h
	}
	return nil
}

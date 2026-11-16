package report

import "testing"

// The Obsidian Greaves case: usual ~5g with ~6 listed, then 10g and listings piling up.
func TestHintSellWhenStretched(t *testing.T) {
	h := MarketHint("horde", HintInput{P20: 100000, PrevP20: 95000, Listed: 22, Fair: 50000, UsualListed: 6, SoldPerDay: 4})
	if h == nil || h.Kind != "sell" || h.Odds != 0.87 || h.Ratio != 2 || !h.Piling {
		t.Fatalf("%+v", h)
	}
	h = MarketHint("blackwater", HintInput{P20: 13000, PrevP20: 13000, Listed: 5, Fair: 10000, UsualListed: 5, SoldPerDay: 4})
	if h == nil || h.Kind != "sell" || h.Odds != 0.53 || h.Piling {
		t.Fatalf("1.3× on Blackwater: %+v", h)
	}
}

func TestHintChaseWhenFallingAndPilingUp(t *testing.T) {
	h := MarketHint("horde", HintInput{P20: 90000, PrevP20: 100000, Listed: 40, Fair: 95000, UsualListed: 20, SoldPerDay: 10})
	if h == nil || h.Kind != "chase" || h.Odds != 0.59 {
		t.Fatalf("%+v", h)
	}
	// Falling but supply isn't growing: no chase signal.
	if h := MarketHint("horde", HintInput{P20: 90000, PrevP20: 100000, Listed: 20, Fair: 95000, UsualListed: 20, SoldPerDay: 10}); h != nil && h.Kind == "chase" {
		t.Fatalf("no pile-up: %+v", h)
	}
}

func TestHintHoldWhenFlatAtUsualLevel(t *testing.T) {
	h := MarketHint("horde", HintInput{P20: 69998, PrevP20: 69998, Listed: 17, Fair: 69999, UsualListed: 15, SoldPerDay: 20})
	if h == nil || h.Kind != "hold" || h.Odds != 0.71 {
		t.Fatalf("%+v", h)
	}
}

func TestHintSlowWhenStockOutlastsSales(t *testing.T) {
	// 28 listed, half a sale a day: 56 days to clear.
	h := MarketHint("horde", HintInput{P20: 9300, PrevP20: 9500, Listed: 28, Fair: 9500, UsualListed: 25, SoldPerDay: 0.5})
	if h == nil || h.Kind != "slow" || h.ClearDays != 56 {
		t.Fatalf("%+v", h)
	}
	// No certain sales at all, with stock listed.
	if h := MarketHint("horde", HintInput{P20: 9300, PrevP20: 9300, Listed: 10, Fair: 9300, UsualListed: 10}); h == nil || h.Kind != "slow" {
		t.Fatalf("no sales: %+v", h)
	}
	// A price-only house has no sales data: never call it slow.
	if h := MarketHint("alliance", HintInput{P20: 9300, PrevP20: 9500, Listed: 28, Fair: 9500, UsualListed: 25, Untracked: true}); h != nil && h.Kind == "slow" {
		t.Fatalf("untracked: %+v", h)
	}
}

func TestHintStretchedBeatsSlow(t *testing.T) {
	h := MarketHint("horde", HintInput{P20: 20000, PrevP20: 20000, Listed: 30, Fair: 10000, UsualListed: 10, SoldPerDay: 0.2})
	if h == nil || h.Kind != "sell" {
		t.Fatalf("%+v", h)
	}
}

func TestHintNoneWithoutHistory(t *testing.T) {
	if h := MarketHint("horde", HintInput{P20: 20000, PrevP20: 0, Listed: 3, Fair: 0}); h != nil {
		t.Fatalf("no baseline yet: %+v", h)
	}
	if h := MarketHint("horde", HintInput{P20: 0, Listed: 0, Fair: 10000}); h != nil {
		t.Fatalf("nothing listed: %+v", h)
	}
	// Near the usual level, moving a bit: nothing worth saying.
	if h := MarketHint("horde", HintInput{P20: 10500, PrevP20: 10200, Listed: 10, Fair: 10000, UsualListed: 10, SoldPerDay: 10}); h != nil {
		t.Fatalf("ordinary: %+v", h)
	}
}

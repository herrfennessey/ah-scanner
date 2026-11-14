package report

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/stats"
)

func TestExpectedSold(t *testing.T) {
	if got := expectedSold(0, 0, 5); got != 0 {
		t.Fatalf("no demand: %v", got)
	}
	// Huge demand, nobody ahead: everything sells.
	if got := expectedSold(1000, 0, 5); math.Abs(got-5) > 1e-6 {
		t.Fatalf("huge demand: %v", got)
	}
	// One unit, nobody ahead: P(N >= 1) = 1 - e^-λ.
	if got, want := expectedSold(0.5, 0, 1), 1-math.Exp(-0.5); math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v want %v", got, want)
	}
	// More units ahead means fewer of mine sell.
	if a, b := expectedSold(10, 2, 5), expectedSold(10, 20, 5); a <= b {
		t.Fatalf("ahead should hurt: %v vs %v", a, b)
	}
}

func book(prices ...int64) []listing.Listing {
	var b []listing.Listing
	for _, p := range prices {
		b = append(b, listing.Listing{UnitPrice: p, Quantity: 1})
	}
	return b
}

func TestAdviseCancelRelistsWhenBuriedBehindCheaperListings(t *testing.T) {
	// 30 cheaper quivers ahead, ~4 sell a day, 7 h left: riding sells ~nothing.
	ic := &ItemContext{Kind: "gear", Fair: 102000, Min: 100000, SoldEst: 4, SoldLo: 3, SoldHi: 6, DepositUnit: 1500,
		book: append(book(100000, 100500, 101000), append(make([]listing.Listing, 0), book(make([]int64, 27)...)...)...)}
	for i := 3; i < 30; i++ {
		ic.book[i] = listing.Listing{UnitPrice: 105000, Quantity: 1}
	}
	ic.book = append(ic.book, listing.Listing{UnitPrice: 130000, Quantity: 1}) // mine
	ca := AdviseCancel(ic, 130000, 1, listing.Long, 0.05, 24)
	if ca.Verdict != "CANCEL AND RELIST" || ca.Ahead != 30 || ca.RelistPrice != 99999 {
		t.Fatalf("%s ahead=%d relist=%d values ride=%d relist=%d", ca.Verdict, ca.Ahead, ca.RelistPrice, ca.RideValue, ca.RelistValue)
	}
}

func TestAdviseCancelHoldsRatherThanDumpBelowMarket(t *testing.T) {
	// Buried behind cheap listings, but they are far below market value: relisting would
	// dump the item; holding (letting it ride or expire) keeps its value.
	ic := &ItemContext{Kind: "gear", Fair: 150000, Min: 100000, SoldEst: 4, SoldLo: 3, SoldHi: 6, DepositUnit: 1500,
		book: append(book(100000, 100000, 100000, 100000, 100000), listing.Listing{UnitPrice: 145000, Quantity: 1})}
	ca := AdviseCancel(ic, 145000, 1, listing.Long, 0.05, 24)
	if ca.Verdict != "LET IT RIDE" {
		t.Fatalf("%s ride=%d relist=%d", ca.Verdict, ca.RideValue, ca.RelistValue)
	}
	found := false
	for _, r := range ca.Reasons {
		if strings.Contains(r, "below market value") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing the below-market explanation: %v", ca.Reasons)
	}
}

func TestAdviseCancelRidesWhenNearTheFront(t *testing.T) {
	// One cheaper unit, brisk sales: you'll sell soon at your higher price.
	ic := &ItemContext{Kind: "commodity", Fair: 2300, Min: 2200, SoldEst: 200, SoldLo: 150, SoldHi: 260, DepositUnit: 80,
		book: []listing.Listing{{UnitPrice: 2200, Quantity: 1}, {UnitPrice: 2289, Quantity: 10}}}
	ca := AdviseCancel(ic, 2289, 10, listing.Long, 0.05, 24)
	if ca.Verdict != "LET IT RIDE" {
		t.Fatalf("%s ride=%d relist=%d", ca.Verdict, ca.RideValue, ca.RelistValue)
	}
	if ca.FoundMine != 1 {
		t.Fatalf("found %d", ca.FoundMine)
	}
}

func TestAdviseCancelAlreadyCheapest(t *testing.T) {
	ic := &ItemContext{Fair: 1000, Min: 900, SoldEst: 5, book: []listing.Listing{{UnitPrice: 900, Quantity: 2}}}
	if ca := AdviseCancel(ic, 900, 2, listing.Long, 0.05, 24); ca.Verdict != "ALREADY CHEAPEST" {
		t.Fatal(ca.Verdict)
	}
}

func TestAdviseSell(t *testing.T) {
	base := func() *ItemContext {
		return &ItemContext{Kind: "commodity", Fair: 600, Min: 590, Listed: 5000, SoldEst: 400, SoldLo: 300, SoldHi: 500,
			book: []listing.Listing{{UnitPrice: 590, Quantity: 5000}}}
	}
	if sa := AdviseSell(base(), 60, 0.05, 600, 5000, ""); sa.Verdict != "SELL NOW" || sa.Price != 589 || sa.ListNow != 60 {
		t.Fatalf("normal market: %+v", sa)
	}
	glut := base()
	glut.Min, glut.Listed = 400, 12000
	if sa := AdviseSell(glut, 60, 0.05, 600, 5000, "18–21"); sa.Verdict != "HOLD" {
		t.Fatalf("glut: %+v", sa)
	}
	thin := base()
	thin.SoldEst, thin.SoldLo, thin.SoldHi = 6, 4, 9
	if sa := AdviseSell(thin, 60, 0.05, 600, 5000, ""); sa.Verdict != "SELL IN PARTS" || sa.ListNow != 4 {
		t.Fatalf("thin market: %+v", sa)
	}
	gear := &ItemContext{Kind: "gear", Fair: 125000, Min: 123456, SoldEst: 3, SoldLo: 2, book: book(123456)}
	if sa := AdviseSell(gear, 1, 0.05, 0, 0, ""); sa.Price != 123399 {
		t.Fatalf("gear undercut to the silver: %d", sa.Price)
	}
}

func TestCheckPrice(t *testing.T) {
	ic := &ItemContext{Fair: 1000, Min: 950, Listed: 30, SoldEst: 10, recentMins: []int64{800, 900, 950, 1000, 1100},
		book: []listing.Listing{{UnitPrice: 950, Quantity: 10}, {UnitPrice: 1000, Quantity: 20}}}
	pc := CheckPrice(ic, 800)
	if pc.Verdict != "GOOD BUY" || pc.Percentile != 0.2 || pc.UnitsBelow != 0 {
		t.Fatalf("%+v", pc)
	}
	if pc := CheckPrice(ic, 1300); pc.Verdict != "EXPENSIVE" || pc.SellVerdict != "TOO HIGH" || pc.UnitsBelow != 30 {
		t.Fatalf("%+v", pc)
	}
}

func TestParseGold(t *testing.T) {
	for in, want := range map[string]int64{"12g30s": 123000, "12g 30s 5c": 123005, "45s": 4500, "80c": 80, "12.5g": 125000, "12.5": 125000, "1g99s99c": 19999} {
		if got, err := ParseGold(in); err != nil || got != want {
			t.Errorf("%q: got %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "12x", "12g30"} {
		if _, err := ParseGold(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestCancelUsesTimeOfDayDemand(t *testing.T) {
	// Same listing, same daily sales; all buying happens 18:00–22:00 Berlin.
	var counts [24]float64
	for h := 18; h < 22; h++ {
		counts[h] = 100
	}
	evening := stats.ProfileFrom(counts, stats.Flat(), 0)
	mk := func(now time.Time) *ItemContext {
		return &ItemContext{Kind: "commodity", Fair: 1000, Min: 990, SoldEst: 40, DepositUnit: 10, profile: evening, now: now,
			book: []listing.Listing{{UnitPrice: 990, Quantity: 5}, {UnitPrice: 1000, Quantity: 1}}}
	}
	morning := time.Date(2026, 11, 16, 6, 0, 0, 0, rollup.Berlin) // 2 h left at MEDIUM-ish: no buyers until 18:00
	lateAfternoon := time.Date(2026, 11, 16, 17, 0, 0, 0, rollup.Berlin)
	am := AdviseCancel(mk(morning), 1000, 1, listing.Medium, 0.05, 24)
	pm := AdviseCancel(mk(lateAfternoon), 1000, 1, listing.Medium, 0.05, 24)
	if !(pm.RideSells > am.RideSells) {
		t.Fatalf("a listing alive into the evening rush should sell more: am=%.3f pm=%.3f", am.RideSells, pm.RideSells)
	}
	if am.RideSells > 0.01 {
		t.Fatalf("no buyers in the morning window: %v", am.RideSells)
	}
}

func TestPlanFlipResalePrice(t *testing.T) {
	// Cross-house: market value 100, but the sell house has a listing at 80: resell at 79.
	units, _, _, sellAt := planFlip([]listing.Listing{{UnitPrice: 40, Quantity: 5}}, []listing.Listing{{UnitPrice: 80, Quantity: 3}}, false, 100, 0.05, 0, 0.15, 10)
	if sellAt != 79 || units != 5 {
		t.Fatalf("cross-house: units=%d sellAt=%d", units, sellAt)
	}
	// Same house: buy the two cheap units at 40; the next listing at 90 caps resale at 89.
	b := []listing.Listing{{UnitPrice: 40, Quantity: 2}, {UnitPrice: 90, Quantity: 10}}
	units, cost, upTo, sellAt := planFlip(b, b, true, 100, 0.05, 0, 0.15, 10)
	if units != 2 || cost != 80 || upTo != 40 || sellAt != 89 {
		t.Fatalf("same house: units=%d cost=%d upTo=%d sellAt=%d", units, cost, upTo, sellAt)
	}
	// Same house, no gap: everything is near market, nothing to flip.
	b = []listing.Listing{{UnitPrice: 95, Quantity: 2}, {UnitPrice: 96, Quantity: 10}}
	if units, _, _, _ := planFlip(b, b, true, 100, 0.05, 0, 0.15, 10); units != 0 {
		t.Fatalf("no gap: units=%d", units)
	}
}

func TestAllianceGate(t *testing.T) {
	// Alliance pays 1g at home; you resell on Blackwater at 80s: they save, their flippers profit.
	ok, price, margin, _ := allianceGate(8000, refPrice{min: 10000, fair: 10500}, true, 0.05)
	if !ok || price != 10000 || margin != 1500 {
		t.Fatalf("pricier at home: ok=%v price=%d margin=%d", ok, price, margin)
	}
	// Alliance pays less at home than your Blackwater price: they'll undercut you, not buy.
	if ok, _, _, note := allianceGate(8000, refPrice{min: 7000, fair: 7200}, true, 0.05); ok {
		t.Fatalf("cheaper at home should fail: %s", note)
	}
	// A momentary spike on Alliance doesn't count: use the lower of now and market value.
	if ok, price, _, _ := allianceGate(8000, refPrice{min: 20000, fair: 8100}, true, 0.05); ok || price != 8100 {
		t.Fatalf("spike: ok=%v price=%d", ok, price)
	}
	// Not listed on Alliance: they can only buy it on Blackwater.
	if ok, _, _, note := allianceGate(8000, refPrice{}, true, 0.05); !ok || !strings.Contains(note, "not listed") {
		t.Fatal(note)
	}
}

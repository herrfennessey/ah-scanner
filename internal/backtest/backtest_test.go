package backtest

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

// A wall at 10s sells 2 units an hour; 6 hours in, someone lists 50 at 8s that never sell.
func scenario(t *testing.T) (*store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ahc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	t0 := time.Now().Add(-30 * time.Hour).Unix()
	for i := 0; i < 26; i++ {
		cur := []listing.Listing{{AuctionID: 1, ItemID: 7, Quantity: int64(100 - 2*i), UnitPrice: 1000, TimeLeft: listing.VeryLong}}
		if i >= 6 {
			cur = append(cur, listing.Listing{AuctionID: 2, ItemID: 7, Quantity: 50, UnitPrice: 800, TimeLeft: listing.VeryLong})
		}
		sn := store.Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: t0 + int64(i)*3600, SHA256: fmt.Sprint(i)}
		if _, err := st.ApplySnapshot(context.Background(), sn, cur); err != nil {
			t.Fatal(err)
		}
	}
	return st, t0
}

func TestFillCountsBuyersAtOrAboveThePrice(t *testing.T) {
	st, t0 := scenario(t)
	flip := report.Flip{ItemID: 7, BuyUnits: 20, BuyUpTo: 500, BuyCost: 10000, SellAt: 999, DepositAtRisk: 100, Play: report.PlayHold}
	// Hold: one 24-hour listing at 9.99s. The wall's buyers paid 10s all along, so they would
	// have taken ours first: 2 an hour, all 20 within 10 hours.
	tr := Trade{Flip: flip, cut: 0.05}
	if err := fill(context.Background(), st.DB, 113, t0, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Sold != 20 || tr.Relists != 0 {
		t.Fatalf("hold: %+v", tr)
	}
	// Priced above what anyone paid: nothing sells, nothing is relisted, and the stock is valued
	// at the 8s floor.
	tr = Trade{Flip: flip, cut: 0.05}
	tr.Flip.SellAt = 1200
	if err := fill(context.Background(), st.DB, 113, t0, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Sold != 0 || tr.Relists != 0 || tr.Floor != 800 || tr.Marked != int64(-10000-100+20*800*0.95) {
		t.Fatalf("unsold hold: %+v", tr)
	}
	// Camp at 12s: undercut at the first hourly visit, so relisted just under the cheapest
	// listing (never under the 5s it cost), and then it sells to the wall's buyers.
	tr = Trade{Flip: flip, cut: 0.05}
	tr.Flip.SellAt, tr.Flip.Play = 1200, report.PlayCamp
	if err := fill(context.Background(), st.DB, 113, t0, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Relists == 0 || tr.Sold == 0 {
		t.Fatalf("camp: %+v", tr)
	}
}

func TestCheckInRelistsOnlyBehindALongQueue(t *testing.T) {
	st, t0 := scenario(t)
	// At 12s, behind the 10s wall: at the 5h visit 90 units are cheaper than ours.
	flip := report.Flip{ItemID: 7, BuyUnits: 20, BuyUpTo: 500, BuyCost: 10000, SellAt: 1200, DepositAtRisk: 100, Play: report.PlayCheckIn}
	// Buyers take 48 a day: in the 19 hours left they'd get through 38 of the 90 ahead of ours.
	// Relist under the wall, and it sells.
	tr := Trade{Flip: flip, cut: 0.05}
	tr.Flip.Demand = 48
	if err := fill(context.Background(), st.DB, 113, t0, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Relists == 0 || tr.Sold == 0 {
		t.Fatalf("long queue: %+v", tr)
	}
	// Were buyers taking 1000 a day, the queue would clear before ours ran out: leave it.
	tr = Trade{Flip: flip, cut: 0.05}
	tr.Flip.Demand = 1000
	if err := fill(context.Background(), st.DB, 113, t0, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Relists != 0 {
		t.Fatalf("short queue: %+v", tr)
	}
}

func TestCheckInQueueCountsSalesAfterTheWindow(t *testing.T) {
	st, t0 := scenario(t)
	// At the 5h visit the wall has 90 left. It's still open at the end of the history with 50,
	// having sold 2 at hour 25, after this trade's 24-hour window: that sale must be added back
	// too, or the queue looks like 88. Buyers taking 113 a day get through 89 in the 19 hours
	// left, so 90 ahead means relist at that first visit: under the wall at 9.99s, its buyers
	// take all 20 by hour 15. (Missing the late sale, it waits for the 10h visit.)
	flip := report.Flip{ItemID: 7, BuyUnits: 20, BuyUpTo: 500, BuyCost: 10000, SellAt: 1200, DepositAtRisk: 100, Play: report.PlayCheckIn, Demand: 113}
	tr := Trade{Flip: flip, cut: 0.05}
	if err := fill(context.Background(), st.DB, 113, t0, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Relists != 1 || tr.Sold != 20 || tr.Hours > 15 {
		t.Fatalf("90 ahead with room for 89 should relist at 5h and sell out by 15h: %+v", tr)
	}
}

func TestFillRelistsWhenTheListingRunsOut(t *testing.T) {
	st, t0 := scenario(t)
	// Camp: 8h listings and hourly visits over 12 hours, priced under everything (never
	// undercut). The listing runs out at 8h and is relisted at that visit, so the wall's 2 an
	// hour all count, and the lapse costs one relist.
	tr := Trade{Flip: report.Flip{ItemID: 7, BuyUnits: 100, BuyUpTo: 500, BuyCost: 50000, SellAt: 700, DepositAtRisk: 100, Play: report.PlayCamp}, cut: 0.05}
	if err := fill(context.Background(), st.DB, 113, t0, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Sold != 24 || tr.Relists != 1 {
		t.Fatalf("sold %d with %d relists, want 24 and 1: %+v", tr.Sold, tr.Relists, tr)
	}
}

func TestBoughtListingsAreNotBoughtAgain(t *testing.T) {
	in := &report.FlipInputs{Items: []report.FlipItem{{Item: 7, Houses: map[string]*report.FlipStat{
		"horde": {Min: 500, Listed: 15, Book: []report.FlipLevel{{P: 500, Q: 5}, {P: 900, Q: 10}}},
	}}}}
	f := report.Flip{ItemID: 7, BuyHouse: "horde", BuyUnits: 5, BuyUpTo: 500}
	c := &consumed{}
	if !c.take(in, f, 1000) {
		t.Fatal("first purchase")
	}
	if c.take(in, f, 1000) {
		t.Fatal("the same 5 can't be bought twice (another route, same time)")
	}
	// The next planning time sees the book without them, and the original is untouched.
	got := c.apply(in).Items[0].Houses["horde"]
	if len(got.Book) != 1 || got.Book[0].P != 900 || got.Min != 900 || got.Listed != 10 || in.Items[0].Houses["horde"].Min != 500 {
		t.Fatalf("applied %+v", got)
	}
	// Two days on, units at that price are someone else's listings.
	c.expire(1000 + consumedFor)
	if !c.take(in, f, 1000+consumedFor) {
		t.Fatal("after expiry")
	}
}

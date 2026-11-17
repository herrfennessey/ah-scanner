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
	flip := report.Flip{ItemID: 7, BuyUnits: 20, BuyUpTo: 500, BuyCost: 10000, SellAt: 999, DepositAtRisk: 100}
	// Away: one 12-hour listing at 9.99s. The wall's buyers paid 10s all along, so they would
	// have taken ours first: 2 an hour, all 20 within the 12 hours.
	tr := Trade{Flip: flip, cut: 0.05}
	if err := fill(context.Background(), st.DB, 113, t0, report.StyleAway, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Sold != 20 || tr.Relists != 0 {
		t.Fatalf("away: %+v", tr)
	}
	// Priced above what anyone paid: nothing sells, and the stock is valued at the 8s floor.
	tr = Trade{Flip: flip, cut: 0.05}
	tr.Flip.SellAt = 1200
	if err := fill(context.Background(), st.DB, 113, t0, report.StyleAway, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Sold != 0 || tr.Floor != 800 || tr.Marked != int64(-10000-100+20*800*0.95) {
		t.Fatalf("unsold: %+v", tr)
	}
	// Active at 12s: undercut at the first hourly visit, so relisted just under the cheapest
	// listing (never under the 5s it cost), and then it sells to the wall's buyers.
	tr = Trade{Flip: flip, cut: 0.05}
	tr.Flip.SellAt = 1200
	if err := fill(context.Background(), st.DB, 113, t0, report.StyleActive, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Relists == 0 || tr.Sold == 0 {
		t.Fatalf("active: %+v", tr)
	}
}

func TestFillStopsWhenTheListingRunsOut(t *testing.T) {
	st, t0 := scenario(t)
	// Regular: 8h listings, visits every 5h, a 24h window, priced under everything (never
	// undercut). Up 0-8h, out until the 10h visit, up 10-18h, out until 20h, up 20-24h: the
	// wall's 2 an hour count only while ours is up, and each lapse costs a relist.
	tr := Trade{Flip: report.Flip{ItemID: 7, BuyUnits: 100, BuyUpTo: 500, BuyCost: 50000, SellAt: 700, DepositAtRisk: 100}, cut: 0.05}
	if err := fill(context.Background(), st.DB, 113, t0, report.StyleRegular, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Sold != 40 || tr.Relists != 2 {
		t.Fatalf("sold %d with %d relists, want 40 and 2: %+v", tr.Sold, tr.Relists, tr)
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

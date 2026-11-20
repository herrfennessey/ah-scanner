package tracker

import (
	"testing"

	"github.com/herrfennessey/ah-scanner/internal/listing"
)

const (
	t0  = int64(1_800_000_000)
	t30 = t0 + 30*60
)

func open(id, item, price, qty int64, tl string) Open {
	return Open{AuctionID: id, ItemID: item, UnitPrice: price, Qty: qty, TimeLeft: tl, LastSeen: t0}
}

func cur(id, item, price, qty int64, tl string) listing.Listing {
	return listing.Listing{AuctionID: id, ItemID: item, UnitPrice: price, Quantity: qty, TimeLeft: tl, Buyout: price * qty}
}

func TestDiffRules(t *testing.T) {
	tests := []struct {
		name        string
		kind        listing.Kind
		open        []Open
		cur         []listing.Listing
		now         int64
		wantID      int64
		wantOutcome string
		wantUnits   int64
	}{
		{
			name: "rule1 commodity partial drop",
			kind: listing.Commodity,
			open: []Open{open(1, 7069, 100, 10, listing.Long)},
			cur:  []listing.Listing{cur(1, 7069, 100, 6, listing.Long)},
			now:  t30, wantID: 1, wantOutcome: Partial, wantUnits: 4,
		},
		{
			name: "rule2+5 vanish at VERY_LONG is sold",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 20, listing.VeryLong)},
			now:  t30, wantID: 1, wantOutcome: Sold, wantUnits: 20,
		},
		{
			name: "rule4c gear vanished while a cheaper copy stayed is cancelled",
			kind: listing.Gear,
			open: []Open{open(1, 3914, 300, 1, listing.VeryLong), open(2, 3914, 250, 1, listing.VeryLong)},
			cur:  []listing.Listing{cur(2, 3914, 250, 1, listing.VeryLong)},
			now:  t30, wantID: 1, wantOutcome: Cancelled, wantUnits: 1,
		},
		{
			name: "rule5 gear vanished as the cheapest copy is sold",
			kind: listing.Gear,
			open: []Open{open(1, 3914, 250, 1, listing.VeryLong), open(2, 3914, 300, 1, listing.VeryLong)},
			cur:  []listing.Listing{cur(2, 3914, 300, 1, listing.VeryLong)},
			now:  t30, wantID: 1, wantOutcome: Sold, wantUnits: 1,
		},
		{
			name: "rule2+5 vanish at LONG with 30 min gap is sold",
			kind: listing.Commodity,
			open: []Open{open(1, 7069, 100, 5, listing.Long)},
			now:  t30, wantID: 1, wantOutcome: Sold, wantUnits: 5,
		},
		{
			name: "rule3 vanish at SHORT is expired-or-sold",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 20, listing.Short)},
			now:  t30, wantID: 1, wantOutcome: ExpiredOrSold, wantUnits: 20,
		},
		{
			name: "rule3 vanish at MEDIUM with gap > 30 min is expired-or-sold",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 20, listing.Medium)},
			now:  t30 + 60, wantID: 1, wantOutcome: ExpiredOrSold, wantUnits: 20,
		},
		{
			name: "MEDIUM with gap under 30 min still had time: sold",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 20, listing.Medium)},
			now:  t0 + 25*60, wantID: 1, wantOutcome: Sold, wantUnits: 20,
		},
		{
			name: "LONG after a 3 h outage could have expired",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 20, listing.Long)},
			now:  t0 + 3*3600, wantID: 1, wantOutcome: ExpiredOrSold, wantUnits: 20,
		},
		{
			name: "rule4a repost at lower price is cancelled",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 20, listing.VeryLong)},
			cur:  []listing.Listing{cur(2, 4304, 90, 20, listing.VeryLong)},
			now:  t30, wantID: 1, wantOutcome: Cancelled, wantUnits: 20,
		},
		{
			name: "rule4d gear reposted at the same price is cancelled",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 1, listing.VeryLong)},
			cur:  []listing.Listing{cur(2, 4304, 100, 1, listing.VeryLong)},
			now:  t30, wantID: 1, wantOutcome: Cancelled, wantUnits: 1,
		},
		{
			name: "commodity reposted at the same price is still sold",
			kind: listing.Commodity,
			open: []Open{open(1, 7069, 100, 5, listing.Long)},
			cur:  []listing.Listing{cur(2, 7069, 100, 5, listing.Long)},
			now:  t30, wantID: 1, wantOutcome: Sold, wantUnits: 5,
		},
		{
			name: "repost with different quantity is not a cancel signal",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 20, listing.VeryLong)},
			cur:  []listing.Listing{cur(2, 4304, 90, 10, listing.VeryLong)},
			now:  t30, wantID: 1, wantOutcome: Sold, wantUnits: 20,
		},
		{
			name: "repost at higher price is not a cancel signal",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 100, 20, listing.VeryLong)},
			cur:  []listing.Listing{cur(2, 4304, 110, 20, listing.VeryLong)},
			now:  t30, wantID: 1, wantOutcome: Sold, wantUnits: 20,
		},
		{
			name: "rule4b cheaper commodity units skipped means unsold",
			kind: listing.Commodity,
			open: []Open{open(1, 7069, 200, 5, listing.Long), open(2, 7069, 150, 3, listing.Long)},
			cur:  []listing.Listing{cur(2, 7069, 150, 3, listing.Long)},
			now:  t30, wantID: 1, wantOutcome: Unsold, wantUnits: 5,
		},
		{
			name: "rule4b also settles a near-expiry commodity: expired, not sold",
			kind: listing.Commodity,
			open: []Open{open(1, 7069, 200, 100, listing.Short), open(2, 7069, 150, 3, listing.Long)},
			cur:  []listing.Listing{cur(2, 7069, 150, 3, listing.Long)},
			now:  t0 + 3600, wantID: 1, wantOutcome: Unsold, wantUnits: 100,
		},
		{
			name: "near-expiry commodity with no cheaper units stays ambiguous",
			kind: listing.Commodity,
			open: []Open{open(1, 7069, 100, 100, listing.Short), open(2, 7069, 150, 3, listing.Long)},
			cur:  []listing.Listing{cur(2, 7069, 150, 3, listing.Long)},
			now:  t0 + 3600, wantID: 1, wantOutcome: ExpiredOrSold, wantUnits: 100,
		},
		{
			name: "rule4b does not apply when the cheaper auction was partly bought",
			kind: listing.Commodity,
			open: []Open{open(1, 7069, 200, 5, listing.Long), open(2, 7069, 150, 3, listing.Long)},
			cur:  []listing.Listing{cur(2, 7069, 150, 1, listing.Long)},
			now:  t30, wantID: 1, wantOutcome: Sold, wantUnits: 5,
		},
		{
			name: "rule4b ignores cheaper units that are new this snapshot",
			kind: listing.Commodity,
			open: []Open{open(1, 7069, 200, 5, listing.Long)},
			cur:  []listing.Listing{cur(3, 7069, 150, 7, listing.Long)},
			now:  t30, wantID: 1, wantOutcome: Sold, wantUnits: 5,
		},
		{
			// Gear isn't filled cheapest first, so a cheaper copy staying doesn't make it certainly
			// unsold (4b); it does make it ambiguous rather than a certain sale (4c).
			name: "rule4b is commodity only",
			kind: listing.Gear,
			open: []Open{open(1, 4304, 200, 5, listing.Long), open(2, 4304, 150, 3, listing.Long)},
			cur:  []listing.Listing{cur(2, 4304, 150, 3, listing.Long)},
			now:  t30, wantID: 1, wantOutcome: Cancelled, wantUnits: 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			om := map[int64]Open{}
			for _, o := range tc.open {
				om[o.AuctionID] = o
			}
			res := Diff(tc.kind, om, tc.cur, tc.now)
			var found bool
			for _, e := range res.Events {
				if e.AuctionID != tc.wantID {
					continue
				}
				found = true
				if e.Outcome != tc.wantOutcome || e.Units != tc.wantUnits {
					t.Fatalf("got %s/%d, want %s/%d", e.Outcome, e.Units, tc.wantOutcome, tc.wantUnits)
				}
			}
			if !found {
				t.Fatalf("no event for auction %d: %+v", tc.wantID, res.Events)
			}
		})
	}
}

func TestDiffNewAndSeen(t *testing.T) {
	om := map[int64]Open{1: open(1, 7069, 100, 10, listing.Long)}
	res := Diff(listing.Commodity, om, []listing.Listing{cur(1, 7069, 100, 10, listing.Long), cur(2, 7069, 100, 1, listing.Long)}, t30)
	if len(res.New) != 1 || res.New[0].AuctionID != 2 || len(res.Seen) != 1 || len(res.Events) != 0 || len(res.Closed) != 0 {
		t.Fatalf("%+v", res)
	}
}

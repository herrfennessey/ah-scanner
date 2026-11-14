package aggregate

import (
	"testing"

	"github.com/herrfennessey/ah-scanner/internal/listing"
)

func TestStats(t *testing.T) {
	b := NewBuilder()
	// 10 units: 2 @100, 3 @200, 5 @1000 (a troll listing at 99999 for 0 effect on p20)
	b.Add(listing.Listing{ItemID: 1, Quantity: 2, UnitPrice: 100})
	b.Add(listing.Listing{ItemID: 1, Quantity: 3, UnitPrice: 200})
	b.Add(listing.Listing{ItemID: 1, Quantity: 5, UnitPrice: 1000})
	b.Add(listing.Listing{ItemID: 2, Quantity: 1, UnitPrice: 0}) // bid only
	s := b.Stats()
	if len(s) != 2 {
		t.Fatalf("%+v", s)
	}
	got := s[0]
	if got.ListedQty != 10 || got.NAuctions != 3 || got.MinUnit != 100 || got.P20Unit != 100 || got.MedianUnit != 200 {
		t.Fatalf("%+v", got)
	}
	if s[1].ListedQty != 1 || s[1].MinUnit != 0 {
		t.Fatalf("%+v", s[1])
	}
}

func TestP20IgnoresSingleCheapDump(t *testing.T) {
	b := NewBuilder()
	b.Add(listing.Listing{ItemID: 1, Quantity: 1, UnitPrice: 10})
	b.Add(listing.Listing{ItemID: 1, Quantity: 99, UnitPrice: 500})
	s := b.Stats()[0]
	if s.MinUnit != 10 || s.P20Unit != 500 {
		t.Fatalf("%+v", s)
	}
}

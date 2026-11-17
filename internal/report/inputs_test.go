package report

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

// A wall of 60 at 10s selling 2 an hour for 30 hours, then 3 dumped at 5s: the dump must not
// read as a sliding market.
func TestInputsCarryEvidenceAndRebuildThePast(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "ahc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	st.UpsertHouse(ctx, 113, "Horde Auction House", "horde")
	const scans = 30
	t0 := time.Now().Add(-scans * time.Hour).Unix()
	for i := 0; i < scans; i++ {
		cur := []listing.Listing{{AuctionID: 1, ItemID: 7, Quantity: int64(60 - 2*i), UnitPrice: 1000, TimeLeft: listing.VeryLong}}
		if i == scans-1 {
			cur = append(cur, listing.Listing{AuctionID: 2, ItemID: 7, Quantity: 3, UnitPrice: 500, TimeLeft: listing.Long})
		}
		sn := store.Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: t0 + int64(i)*3600, SHA256: fmt.Sprint(i)}
		if _, err := st.ApplySnapshot(ctx, sn, cur); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(t0+(scans-1)*3600, 0)
	if _, err := rollup.Range(ctx, st.DB, now.AddDate(0, 0, -2), now, rollup.Options{}); err != nil {
		t.Fatal(err)
	}
	houses := []FlipHouse{{ID: 113, Faction: "horde", Cut: 0.05, Tracked: true}}

	in, err := BuildFlipInputs(ctx, st, houses, 0.2, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Items) != 1 {
		t.Fatalf("items %+v", in.Items)
	}
	s := in.Items[0].Houses["horde"]
	if !s.Evidence || s.Clearing != 1000 || s.Trend != 0 || len(s.Book) != 2 || s.Book[0] != (FlipLevel{500, 3}) {
		t.Fatalf("now: %+v", s)
	}
	if u, n := atOrAbove(s.Sold12, 1000); u != 24 || n != 12 {
		t.Fatalf("12 h of sales at 10s: %d units in %d sales (%+v)", u, n, s.Sold12)
	}
	// The wall was posted as the cheapest and sold 2 of 60 within the hour.
	if s.Front == nil || s.Front.N != 1 || s.Front.Sold[0] != 2.0/60 {
		t.Fatalf("front %+v", s.Front)
	}

	// Ten hours in: no dump yet, and the wall still held 40 (60 less 2 an hour, its later
	// sales added back).
	past, err := BuildFlipInputsAt(ctx, st, houses, 0.2, time.Unix(t0+10*3600, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range past.Items {
		if p := it.Houses["horde"]; p.Min != 1000 {
			t.Fatalf("the dump came later: %+v", p)
		}
	}
}

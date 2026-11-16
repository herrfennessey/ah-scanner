package collector

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/store"
	"github.com/herrfennessey/ah-scanner/internal/supply"
)

// Three hourly scans: Earthroot is cheap, then bought down to its dearer listing, then a new
// cheap listing appears. Each chart point must price the order book as it was at its scan.
func TestRefreshSupplyRebuildsEachScan(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "ahc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	st.UpsertHouse(ctx, 113, "Horde Auction House", "horde")
	fill := `{"spells":[{"description":"Use: Fill the crate with any bundle from the following list:\n- 10 Earthroot"}]}`
	st.SetItem(ctx, store.Item{ID: 248682, Name: "Waylaid Crate: Apprentice Herbs", Tooltip: json.RawMessage(fill)})
	st.SetItem(ctx, store.Item{ID: 2449, Name: "Earthroot"})

	t0 := time.Now().Add(-3 * time.Hour).Unix()
	crate := listing.Listing{AuctionID: 1, ItemID: 248682, Quantity: 1, UnitPrice: 100, TimeLeft: listing.Long}
	cheap := listing.Listing{AuctionID: 2, ItemID: 2449, Quantity: 15, UnitPrice: 50, TimeLeft: listing.Long}
	dear := listing.Listing{AuctionID: 3, ItemID: 2449, Quantity: 20, UnitPrice: 90, TimeLeft: listing.Long}
	later := listing.Listing{AuctionID: 4, ItemID: 2449, Quantity: 10, UnitPrice: 40, TimeLeft: listing.Long}
	bought := cheap
	bought.Quantity = 5 // 10 of the cheap 15 bought

	scans := [][]listing.Listing{{crate, cheap, dear}, {crate, bought, dear}, {crate, bought, dear, later}}
	for i, l := range scans {
		sn := store.Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: t0 + int64(i)*3600, SHA256: string(rune('a' + i))}
		if _, err := st.ApplySnapshot(ctx, sn, l); err != nil {
			t.Fatal(err)
		}
	}

	c := &Collector{Cfg: config.Default(), Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Houses: []House{{ID: 113, Faction: "horde"}}}
	if err := c.RefreshSupply(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := st.SupplyPoints(ctx, 113, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	for _, r := range raw {
		var p supply.Point
		json.Unmarshal(r, &p)
		got = append(got, p.PerFavor)
	}
	// (100 crate + 10 Earthroot) / 5 Favor: 10×50; then 5×50 + 5×90; then 10×40.
	want := []float64{120, 160, 100}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("per Favor by scan = %v, want %v", got, want)
	}

	at, b, err := st.LoadSupplyCache(ctx)
	if err != nil || b == nil || at != t0+7200 {
		t.Fatalf("cache at %d (%v), want %d", at, err, t0+7200)
	}
	var v supply.View
	if err := json.Unmarshal(b, &v); err != nil || len(v.Crates) != 1 || v.Crates[0].PerFavor != 100 || v.Items[2449].Name != "Earthroot" {
		t.Fatalf("cached view %+v (%v)", v, err)
	}

	// A second refresh with nothing new keeps the same points.
	if err := c.RefreshSupply(ctx); err != nil {
		t.Fatal(err)
	}
	if raw, _ := st.SupplyPoints(ctx, 113, 0); len(raw) != 3 {
		t.Fatalf("%d points after a repeat refresh, want 3", len(raw))
	}
}

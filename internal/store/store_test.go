package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/herrfennessey/ah-scanner/internal/listing"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "ahc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func count(t *testing.T, st *Store, q string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestClosuresStayOnAuctionRows(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour).Unix()
	cur := []listing.Listing{
		{AuctionID: 1, ItemID: 7069, Quantity: 10, UnitPrice: 100, TimeLeft: listing.Long},
		{AuctionID: 2, ItemID: 7069, Quantity: 5, UnitPrice: 200, TimeLeft: listing.VeryLong},
	}
	st.ApplySnapshot(ctx, Snapshot{HouseID: 1, Kind: listing.Commodity, FetchedAt: t0, SHA256: "a"}, cur)
	// Auction 1 partly bought, auction 2 vanished.
	res, err := st.ApplySnapshot(ctx, Snapshot{HouseID: 1, Kind: listing.Commodity, FetchedAt: t0 + 1800, SHA256: "b"},
		[]listing.Listing{{AuctionID: 1, ItemID: 7069, Quantity: 4, UnitPrice: 100, TimeLeft: listing.Long}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events["partial"] != 6 || res.Events["sold"] != 5 {
		t.Fatalf("events %v", res.Events)
	}
	if n := count(t, st, `SELECT count(*) FROM sale_event`); n != 1 {
		t.Fatalf("sale_event rows %d, want only the partial", n)
	}
	if n := count(t, st, `SELECT count(*) FROM auction WHERE auction_id=2 AND outcome='sold' AND qty_last=5`); n != 1 {
		t.Fatal("closure not recorded on the auction row")
	}
}

func TestRetain(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	day := int64(86400)
	ins := func(id, at int64) {
		st.DB.Exec(`INSERT INTO snapshot (id, house_id, kind, fetched_at, body_sha256) VALUES (?,1,'commodity',?,?)`, id, at, id)
		st.DB.Exec(`INSERT INTO item_snapshot (snapshot_id, item_id, variant, listed_qty) VALUES (?,7069,'',1)`, id)
	}
	base := (now.Unix() - 40*day) / 21600 * 21600 // aligned to a 6-hour bucket, 40 days ago
	for i := int64(0); i < 12; i++ {              // 12 hourly snapshots = two 6-hour buckets
		ins(i+1, base+i*3600)
	}
	ins(100, now.Unix()-200*day) // past the 180-day limit
	ins(200, now.Unix()-day)     // recent: untouched
	st.DB.Exec(`INSERT INTO auction (house_id, kind, auction_id, item_id, closed_at) VALUES (1,'commodity',1,7069,?), (1,'commodity',2,7069,?), (1,'commodity',3,7069,NULL)`,
		now.Unix()-61*day, now.Unix()-59*day)
	st.DB.Exec(`INSERT INTO sale_event (house_id, ts, units) VALUES (1, ?, 1), (1, ?, 1)`, now.Unix()-61*day, now.Unix()-day)
	st.DB.Exec(`INSERT INTO item_daily (house_id, item_id, variant, day) VALUES (1, 7069, '', '2020-01-01')`)

	if _, err := st.Retain(ctx, now, Retention{AuctionDays: 60, FullDays: 30, ThinHours: 6, SnapshotDays: 180}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM item_snapshot WHERE snapshot_id <= 12`); n != 2 {
		t.Fatalf("thinned to %d snapshots, want 2 (one per 6 h)", n)
	}
	if n := count(t, st, `SELECT count(*) FROM item_snapshot WHERE snapshot_id = 100`); n != 0 {
		t.Fatal("expired snapshot kept")
	}
	if n := count(t, st, `SELECT count(*) FROM item_snapshot WHERE snapshot_id = 200`); n != 1 {
		t.Fatal("recent snapshot removed")
	}
	if n := count(t, st, `SELECT count(*) FROM auction`); n != 2 {
		t.Fatalf("auctions left %d, want 2 (recent closed + open)", n)
	}
	if n := count(t, st, `SELECT count(*) FROM sale_event`); n != 1 {
		t.Fatal("old sale_event kept")
	}
	if n := count(t, st, `SELECT count(*) FROM item_daily`); n != 1 {
		t.Fatal("item_daily must be kept forever")
	}
}

func TestBackupCompressedAndRotated(t *testing.T) {
	st := open(t)
	dir := t.TempDir()
	day := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := st.Backup(context.Background(), dir, day.AddDate(0, 0, i), 3); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 || entries[0].Name() != "ahc-20261103.db.zst" {
		t.Fatalf("%v", entries)
	}
	f, _ := os.Open(filepath.Join(dir, entries[2].Name()))
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 16)
	if _, err := zr.Read(head); err != nil || string(head[:15]) != "SQLite format 3" {
		t.Fatalf("not a compressed SQLite file: %q %v", head, err)
	}
}

func TestMeta(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	if v, err := st.Meta(ctx, "k"); err != nil || v != "" {
		t.Fatalf("unset: %q %v", v, err)
	}
	st.SetMeta(ctx, "k", "a")
	st.SetMeta(ctx, "k", "b")
	if v, _ := st.Meta(ctx, "k"); v != "b" {
		t.Fatalf("got %q", v)
	}
}

func TestSetItemKeepsTooltip(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	if err := st.SetItem(ctx, Item{ID: 1, Name: "X", Tooltip: json.RawMessage(`{"name":"X"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetItem(ctx, Item{ID: 1, Name: "X"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ItemTooltip(ctx, 1)
	if err != nil || string(got) != `{"name":"X"}` {
		t.Fatalf("tooltip %q, err %v", got, err)
	}
	if got, _ := st.ItemTooltip(ctx, 2); got != nil {
		t.Fatalf("unknown item tooltip %q, want nil", got)
	}
}

func TestItemsNeedingNamesIncludesMissingTooltip(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	st.ApplySnapshot(ctx, Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: 1, SHA256: "a"},
		[]listing.Listing{{AuctionID: 1, ItemID: 5, Quantity: 1, UnitPrice: 10, TimeLeft: listing.Long}})
	st.SetItem(ctx, Item{ID: 5, Name: "Old", Icon: "x.jpg"})
	st.DB.Exec(`UPDATE item SET looked_up_at = 0 WHERE id = 5`)
	ids, err := st.ItemsNeedingNames(ctx, 10, time.Hour)
	if err != nil || len(ids) != 1 || ids[0] != 5 {
		t.Fatalf("ids %v, err %v", ids, err)
	}
}

func TestHaveIcons(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	st.SetItem(ctx, Item{ID: 1, Name: "A", Icon: "a.jpg"})
	st.SetItem(ctx, Item{ID: 2, Name: "B"})
	got, err := st.HaveIcons(ctx)
	if err != nil || !got[1] || got[2] || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestWritRepRoundTrip(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	if err := st.SetWritRep(ctx, 264021, 94221, 75); err != nil {
		t.Fatal(err)
	}
	got, err := st.WritReps(ctx)
	if err != nil || got[264021] != 75 || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestItemsNeedingNamesPutsUnnamedFirst(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	var l []listing.Listing
	for i := int64(1); i <= 20; i++ {
		l = append(l, listing.Listing{AuctionID: i, ItemID: i, Quantity: 1, UnitPrice: 10, TimeLeft: listing.Long})
	}
	st.ApplySnapshot(ctx, Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: 1, SHA256: "a"}, l)
	for i := int64(1); i <= 19; i++ { // named and iconed, only the tooltip missing
		st.SetItem(ctx, Item{ID: i, Name: "Old", Icon: "x.jpg"})
	}
	st.DB.Exec(`UPDATE item SET looked_up_at = 0`)
	ids, err := st.ItemsNeedingNames(ctx, 1, time.Hour)
	if err != nil || len(ids) != 1 || ids[0] != 20 {
		t.Fatalf("the never-seen item must come first: %v %v", ids, err)
	}
}

func TestItemsNeedingNamesSkipsRetryWaitForMissingTooltip(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	st.ApplySnapshot(ctx, Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: 1, SHA256: "a"},
		[]listing.Listing{{AuctionID: 1, ItemID: 5, Quantity: 1, UnitPrice: 10, TimeLeft: listing.Long}})
	st.SetItem(ctx, Item{ID: 5, Name: "Looked up by an older version", Icon: "x.jpg"}) // looked_up_at = now, no tooltip
	ids, err := st.ItemsNeedingNames(ctx, 10, 24*time.Hour)
	if err != nil || len(ids) != 1 || ids[0] != 5 {
		t.Fatalf("a named item with no tooltip must not wait out the retry period: %v %v", ids, err)
	}
}

func TestMissingTooltips(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	st.SetItem(ctx, Item{ID: 1, Name: "A", Tooltip: json.RawMessage(`{}`)})
	st.SetItem(ctx, Item{ID: 2, Name: "B"})
	got, err := st.MissingTooltips(ctx, []int64{1, 2, 3})
	if err != nil || !reflect.DeepEqual(got, []int64{2, 3}) {
		t.Fatalf("%v %v", got, err)
	}
}

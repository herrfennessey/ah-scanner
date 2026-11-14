package store

import (
	"context"
	"os"
	"path/filepath"
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

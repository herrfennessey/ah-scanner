package rollup_test

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

type simAuction struct {
	id, item, qty, price int64
	expires              int64
	present              bool // was in the previous snapshot
}

type market struct {
	kind     listing.Kind
	rng      *rand.Rand
	nextID   int64
	auctions []*simAuction
	trueSold map[string]int64 // "item|day" → units
	items    []int64
}

func bucket(remaining int64) string {
	switch {
	case remaining < 30*60:
		return listing.Short
	case remaining < 2*3600:
		return listing.Medium
	case remaining < 12*3600:
		return listing.Long
	}
	return listing.VeryLong
}

func (m *market) post(item, qty, price, now int64) {
	durations := []int64{2 * 3600, 8 * 3600, 24 * 3600}
	m.nextID++
	m.auctions = append(m.auctions, &simAuction{id: m.nextID, item: item, qty: qty, price: price, expires: now + durations[m.rng.Intn(3)]})
}

func (m *market) sold(item, units, now int64) {
	m.trueSold[fmt.Sprintf("%d|%s", item, rollup.DayString(time.Unix(now, 0), rollup.Berlin))] += units
}

// step advances the market from the previous snapshot to `now` and returns the new snapshot.
func (m *market) step(now int64) []listing.Listing {
	var keep []*simAuction
	var reposts [][3]int64
	// 1. Expiries, sales and cancels — only on auctions that were in the previous snapshot,
	//    so nothing is posted and bought unseen (the documented blind spot).
	if m.kind == listing.Commodity {
		for _, item := range m.items {
			buys := m.rng.Intn(4)
			for b := 0; b < buys; b++ {
				want := int64(1 + m.rng.Intn(15))
				var avail []*simAuction
				for _, a := range m.auctions {
					if a.item == item && a.present && a.qty > 0 && a.expires > now {
						avail = append(avail, a)
					}
				}
				sort.Slice(avail, func(i, j int) bool { return avail[i].price < avail[j].price })
				for _, a := range avail {
					if want == 0 {
						break
					}
					take := min(want, a.qty)
					a.qty -= take
					want -= take
					m.sold(item, take, now)
				}
			}
		}
	}
	for _, a := range m.auctions {
		switch {
		case !a.present:
			keep = append(keep, a)
		case a.expires <= now:
			// expired, gone
		case a.qty == 0:
			// fully bought (commodity)
		case m.kind == listing.Gear && m.rng.Float64() < 0.06:
			m.sold(a.item, a.qty, now)
		case m.rng.Float64() < 0.03:
			reposts = append(reposts, [3]int64{a.item, a.qty, a.price - 1 - m.rng.Int63n(5)})
		default:
			keep = append(keep, a)
		}
	}
	m.auctions = keep
	for _, r := range reposts {
		m.post(r[0], r[1], r[2], now)
	}
	// 2. New postings.
	for _, item := range m.items {
		for n := m.rng.Intn(4); n > 0; n-- {
			qty := int64(1)
			if m.kind == listing.Commodity {
				qty = int64(1 + m.rng.Intn(20))
			} else if m.rng.Intn(2) == 0 {
				qty = 20
			}
			m.post(item, qty, 1000+m.rng.Int63n(400), now)
		}
	}
	// 3. Snapshot.
	out := make([]listing.Listing, 0, len(m.auctions))
	for _, a := range m.auctions {
		a.present = true
		out = append(out, listing.Listing{AuctionID: a.id, ItemID: a.item, Quantity: a.qty, UnitPrice: a.price,
			Buyout: a.price * a.qty, TimeLeft: bucket(a.expires - now)})
	}
	return out
}

func TestSyntheticWeekBoundsHold(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "ahc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertHouse(ctx, 6, "Horde Auction House", "horde"); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 11, 2, 0, 0, 0, 0, rollup.Berlin).Unix()
	markets := []*market{
		{kind: listing.Commodity, rng: rand.New(rand.NewSource(1)), trueSold: map[string]int64{}, items: []int64{7068, 7069}},
		{kind: listing.Gear, rng: rand.New(rand.NewSource(2)), trueSold: map[string]int64{}, items: []int64{4304, 15215}, nextID: 1 << 30},
	}
	for step := int64(0); step <= 7*48; step++ {
		now := start + step*1800
		for _, m := range markets {
			cur := m.step(now)
			sn := store.Snapshot{HouseID: 6, Kind: m.kind, FetchedAt: now, SHA256: fmt.Sprintf("%s-%d", m.kind, step)}
			if _, err := st.ApplySnapshot(ctx, sn, cur); err != nil {
				t.Fatal(err)
			}
		}
	}

	opt := rollup.Options{AmbiguousWeight: 0.25}
	if _, err := rollup.Range(ctx, st.DB, time.Unix(start, 0), time.Unix(start+6*86400, 0), opt); err != nil {
		t.Fatal(err)
	}

	checked, totalTrue := 0, int64(0)
	for _, m := range markets {
		for k, trueUnits := range m.trueSold {
			var item int64
			var day string
			fmt.Sscanf(k, "%d|%s", &item, &day)
			if day > rollup.DayString(time.Unix(start+6*86400, 0), rollup.Berlin) {
				continue
			}
			var lo, hi, est int64
			var fair int64
			err := st.DB.QueryRow(`SELECT sold_lo, sold_hi, sold_est, COALESCE(fair_unit,0) FROM item_daily WHERE house_id=6 AND item_id=? AND day=?`, item, day).
				Scan(&lo, &hi, &est, &fair)
			if err != nil {
				t.Fatalf("%s %s: %v", m.kind, k, err)
			}
			if lo > trueUnits || trueUnits > hi {
				t.Errorf("%s item %d %s: sold_lo %d ≤ true %d ≤ sold_hi %d violated", m.kind, item, day, lo, trueUnits, hi)
			}
			if est < lo || est > hi {
				t.Errorf("est %d outside [%d,%d]", est, lo, hi)
			}
			if fair < 1000 || fair > 1400 {
				t.Errorf("fair %d outside listed price band", fair)
			}
			checked++
			totalTrue += trueUnits
		}
	}
	if checked < 20 || totalTrue == 0 {
		t.Fatalf("simulation too small: %d item-days, %d units", checked, totalTrue)
	}
}

func TestDayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "ahc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.UpsertHouse(ctx, 6, "H", "horde")
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, rollup.Berlin).Unix()
	l := []listing.Listing{{AuctionID: 1, ItemID: 7069, Quantity: 5, UnitPrice: 100, TimeLeft: listing.Long}}
	st.ApplySnapshot(ctx, store.Snapshot{HouseID: 6, Kind: listing.Commodity, FetchedAt: now, SHA256: "a"}, l)
	l[0].Quantity = 2
	st.ApplySnapshot(ctx, store.Snapshot{HouseID: 6, Kind: listing.Commodity, FetchedAt: now + 1800, SHA256: "b"}, l)
	for i := 0; i < 2; i++ {
		if _, err := rollup.Day(ctx, st.DB, 6, "2026-11-02", rollup.Options{AmbiguousWeight: 0.25}); err != nil {
			t.Fatal(err)
		}
	}
	var n, lo, listed int64
	st.DB.QueryRow(`SELECT count(*), SUM(sold_lo), SUM(avg_listed) FROM item_daily`).Scan(&n, &lo, &listed)
	if n != 1 || lo != 3 || listed != 4 { // (5+2)/2 rounded
		t.Fatalf("n=%d lo=%d listed=%d", n, lo, listed)
	}
}

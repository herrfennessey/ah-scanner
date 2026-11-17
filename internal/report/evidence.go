package report

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Evidence is what recently happened to an item on a tracked house, as of the build: the
// planner checks a resale price against real purchases rather than against asking prices.
//
// Sales are what the scans can see (tracker rules): a commodity listing that loses units, or
// vanishes while nothing cheaper is left, was bought (the commodity AH fills cheapest first).
// Vanished gear counts only if no cheaper copy, listed before it, outlasted it (rule 4c): buyers
// take the cheapest copy. Buys within one scan interval of the listing appearing are never
// seen, so demand is, if anything, undercounted.

// frontHours are the check intervals the sell rates are measured for.
var frontHours = [4]float64{1, 5, 12, 24}

// FrontStat measures listings that were the cheapest of their item when first seen: the share
// of their units sold within each of frontHours.
type FrontStat struct {
	N    float64    `json:"n"`    // listings
	Sold [4]float64 `json:"sold"` // sum over listings of the share of units sold within frontHours[i]
}

func (f *FrontStat) add(o *FrontStat) {
	f.N += o.N
	for i := range f.Sold {
		f.Sold[i] += o.Sold[i]
	}
}

// rate is the share sold within h hours (the nearest measured interval at or above it).
func (f *FrontStat) rate(h float64) (float64, float64) {
	if f == nil || f.N == 0 {
		return 0, 0
	}
	i := len(frontHours) - 1
	for j, x := range frontHours {
		if h <= x {
			i = j
			break
		}
	}
	return f.Sold[i] / f.N, f.N
}

const (
	bucketStep   = 1.03 // sale and listing prices are kept in 3% steps, rounded down
	frontPrior   = 10.0 // listings of the house average an item's own sell rate is blended with
	trendMinScan = 6
)

func priceStep(p int64) int64 {
	if p <= 0 {
		return 0
	}
	l := math.Log(bucketStep)
	return int64(math.Exp(math.Floor(math.Log(float64(p))/l+1e-9) * l))
}

// addLevel adds q units in n sales at price p to a ladder kept in price buckets.
func addLevel(l []SaleLevel, p, q, n int64) []SaleLevel {
	b := priceStep(p)
	for i := range l {
		if l[i].P == b {
			l[i].Q += q
			l[i].N += n
			return l
		}
	}
	return append(l, SaleLevel{P: b, Q: q, N: n})
}

// atOrAbove sums the units and sales of a ladder at price p or more, by 3% step: sales in p's
// own step count, so a resale 1c under a wall still sees the buyers who paid the wall price.
func atOrAbove(l []SaleLevel, p int64) (units, sales int64) {
	b := priceStep(p)
	for _, x := range l {
		if x.P >= b {
			units += x.Q
			sales += x.N
		}
	}
	return
}

func idList(keep []Key) string {
	seen := map[int64]bool{}
	var ids []string
	for _, k := range keep {
		if !seen[k.Item] {
			seen[k.Item] = true
			ids = append(ids, fmt.Sprint(k.Item))
		}
	}
	return "[" + strings.Join(ids, ",") + "]"
}

// addEvidence fills the evidence of the kept items on one tracked house, and returns the
// house's own front sell rates (over those items) as the prior for thin items.
func addEvidence(ctx context.Context, db *sql.DB, house int64, faction string, keep []Key, at int64, stats map[Key]map[string]*FlipStat) (*FrontStat, error) {
	ids := idList(keep)
	stat := func(k Key) *FlipStat {
		if m := stats[k]; m != nil {
			return m[faction]
		}
		return nil
	}
	for _, k := range keep {
		if s := stat(k); s != nil {
			s.Evidence = true
		}
	}

	// Certain sales in the last 48 hours.
	rows, err := db.QueryContext(ctx, `SELECT item_id, variant, ts, units, unit_price FROM sale_event
			WHERE house_id=? AND ts>? AND ts<=? AND outcome='partial' AND item_id IN (SELECT value FROM json_each(?))
		UNION ALL
		SELECT a.item_id, a.variant, a.closed_at, a.qty_last, a.unit_price FROM auction a
			WHERE a.house_id=? AND a.closed_at>? AND a.closed_at<=? AND a.outcome='sold' AND a.item_id IN (SELECT value FROM json_each(?))`,
		house, at-48*3600, at, ids, house, at-48*3600, at, ids)
	if err != nil {
		return nil, fmt.Errorf("evidence sales: %w", err)
	}
	day := map[Key][]int64{}
	for rows.Next() {
		var k Key
		var ts, units, price int64
		if err := rows.Scan(&k.Item, &k.Variant, &ts, &units, &price); err != nil {
			rows.Close()
			return nil, err
		}
		s := stat(k)
		if s == nil || price <= 0 || units <= 0 {
			continue
		}
		s.Sold48 = addLevel(s.Sold48, price, units, 1)
		if ts > at-12*3600 {
			s.Sold12 = addLevel(s.Sold12, price, units, 1)
		}
		if ts > at-24*3600 {
			day[k] = append(day[k], price)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k, p := range day {
		if len(p) >= 3 {
			sort.Slice(p, func(i, j int) bool { return p[i] < p[j] })
			stat(k).Clearing = p[len(p)/2]
		}
	}

	// Trend of the median asking price over the last 24 hours, by the median of the slopes
	// between every pair of scans (Theil-Sen): one dump, like the one a relist buys, can't
	// make a market look like it's sliding.
	rows, err = db.QueryContext(ctx, `SELECT s.fetched_at, i.item_id, i.variant, i.median_unit FROM snapshot s
		JOIN item_snapshot i ON i.snapshot_id=s.id
		WHERE s.house_id=? AND s.fetched_at>? AND s.fetched_at<=? AND i.median_unit>0 AND i.item_id IN (SELECT value FROM json_each(?))`,
		house, at-24*3600, at, ids)
	if err != nil {
		return nil, fmt.Errorf("evidence trend: %w", err)
	}
	type pt struct{ x, y float64 }
	series := map[Key][]pt{}
	for rows.Next() {
		var k Key
		var t, p int64
		if err := rows.Scan(&t, &k.Item, &k.Variant, &p); err != nil {
			rows.Close()
			return nil, err
		}
		if stat(k) != nil {
			series[k] = append(series[k], pt{float64(t-at) / 86400, math.Log(float64(p))})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k, v := range series {
		if len(v) < trendMinScan {
			continue
		}
		var slopes []float64
		lo, hi := v[0].x, v[0].x
		for i := range v {
			lo, hi = math.Min(lo, v[i].x), math.Max(hi, v[i].x)
			for j := i + 1; j < len(v); j++ {
				if dx := v[j].x - v[i].x; dx != 0 {
					slopes = append(slopes, (v[j].y-v[i].y)/dx)
				}
			}
		}
		if hi-lo < 0.5 || len(slopes) == 0 {
			continue // under 12 hours of scans
		}
		sort.Float64s(slopes)
		stat(k).Trend = math.Exp(slopes[len(slopes)/2]) - 1
	}

	// How the listings that were cheapest when posted did, over the 3 days before the last 24
	// hours (so the day after each one is known by now).
	type fa struct {
		k                Key
		first, qty, sold int64 // sold: units of a bought closure
		closed           int64
		fracs            [4]float64
	}
	front := map[[2]any]*fa{}
	rows, err = db.QueryContext(ctx, `SELECT a.kind, a.auction_id, a.item_id, a.variant, a.first_seen, a.qty_first,
			CASE WHEN a.outcome='sold' THEN a.qty_last ELSE 0 END, COALESCE(a.closed_at, 0)
		FROM auction a WHERE a.house_id=? AND a.front=1 AND a.first_seen>? AND a.first_seen<=? AND a.item_id IN (SELECT value FROM json_each(?))`,
		house, at-4*86400, at-86400, ids)
	if err != nil {
		return nil, fmt.Errorf("evidence front: %w", err)
	}
	for rows.Next() {
		var kind string
		var id int64
		x := &fa{}
		if err := rows.Scan(&kind, &id, &x.k.Item, &x.k.Variant, &x.first, &x.qty, &x.sold, &x.closed); err != nil {
			rows.Close()
			return nil, err
		}
		if x.qty <= 0 || stat(x.k) == nil {
			continue
		}
		if x.sold > 0 {
			for i, h := range frontHours {
				if x.closed-x.first <= int64(h*3600) {
					x.fracs[i] += float64(x.sold)
				}
			}
		}
		front[[2]any{kind, id}] = x
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT e.kind, e.auction_id, e.ts, e.units FROM sale_event e
		JOIN auction a ON a.house_id=e.house_id AND a.kind=e.kind AND a.auction_id=e.auction_id
		WHERE e.house_id=? AND e.outcome='partial' AND e.ts>? AND e.ts<=? AND a.front=1 AND a.first_seen>? AND a.first_seen<=?
			AND a.item_id IN (SELECT value FROM json_each(?))`, house, at-4*86400, at, at-4*86400, at-86400, ids)
	if err != nil {
		return nil, fmt.Errorf("evidence partials: %w", err)
	}
	for rows.Next() {
		var kind string
		var id, ts, units int64
		if err := rows.Scan(&kind, &id, &ts, &units); err != nil {
			rows.Close()
			return nil, err
		}
		x := front[[2]any{kind, id}]
		if x == nil {
			continue
		}
		for i, h := range frontHours {
			if ts-x.first <= int64(h*3600) {
				x.fracs[i] += float64(units)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	all := &FrontStat{}
	for _, x := range front {
		s := stat(x.k)
		if s.Front == nil {
			s.Front = &FrontStat{}
		}
		f := FrontStat{N: 1}
		for i := range f.Sold {
			f.Sold[i] = math.Min(1, x.fracs[i]/float64(x.qty))
		}
		s.Front.add(&f)
		all.add(&f)
	}
	return all, nil
}

// frontChance is the chance one listing round of h hours sells: the item's own rate for
// cheapest-when-posted listings, blended with the house's when it has few of them.
func frontChance(s *FlipStat, house *FrontStat, h float64, fallback float64) float64 {
	prior := fallback
	if r, n := house.rate(h); n > 0 {
		prior = r
	}
	r, n := s.Front.rate(h)
	return (r*n + prior*frontPrior) / (n + frontPrior)
}

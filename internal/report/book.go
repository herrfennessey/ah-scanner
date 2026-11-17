package report

import (
	"context"
	"database/sql"
	"sort"
)

// ItemBook is one item's order book on a house right now, next to what buyers paid lately: the
// price distribution the item page draws.
type ItemBook struct {
	Listed   []BookLevel `json:"listed"`       // open listings by unit price, cheapest first
	Sold     []BookLevel `json:"sold"`         // certain purchases in the last 48 h by unit price
	Clearing int64       `json:"recent_price"` // median price of the purchases in the last 24 h (3+), else 0
	Tracked  bool        `json:"tracked"`      // auctions tracked here, so Sold means something
	Since    int64       `json:"sold_since"`   // start of the Sold window
}

// BookLevel is the units (and auctions or sales) at one unit price.
type BookLevel struct {
	P int64 `json:"p"`
	Q int64 `json:"q"`
	N int64 `json:"n"`
}

// LoadItemBook reads the open listings and the last 48 hours of purchases of one item, as the
// tracker classified them.
func LoadItemBook(ctx context.Context, db *sql.DB, house, item int64, variant string, tracked bool, now int64) (*ItemBook, error) {
	b := &ItemBook{Tracked: tracked, Since: now - 48*3600, Listed: []BookLevel{}, Sold: []BookLevel{}}
	rows, err := db.QueryContext(ctx, `SELECT unit_price, SUM(qty_last), COUNT(*) FROM auction
		WHERE house_id=? AND item_id=? AND variant=? AND closed_at IS NULL AND unit_price>0 GROUP BY unit_price ORDER BY unit_price`, house, item, variant)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l BookLevel
		if err := rows.Scan(&l.P, &l.Q, &l.N); err != nil {
			rows.Close()
			return nil, err
		}
		b.Listed = append(b.Listed, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !tracked {
		return b, nil
	}
	rows, err = db.QueryContext(ctx, `SELECT price, SUM(units), COUNT(*), SUM(ts > ?) FROM (
			SELECT ts, units, unit_price price FROM sale_event WHERE house_id=? AND item_id=? AND variant=? AND ts>? AND outcome='partial'
			UNION ALL
			SELECT a.closed_at, a.qty_last, a.unit_price FROM auction a WHERE a.house_id=? AND a.item_id=? AND a.variant=? AND a.closed_at>? AND a.outcome='sold'
		) WHERE price>0 GROUP BY price ORDER BY price`, now-24*3600, house, item, variant, b.Since, house, item, variant, b.Since)
	if err != nil {
		return nil, err
	}
	var day []BookLevel // sales in the last 24 h, by price
	for rows.Next() {
		var l BookLevel
		var recent int64
		if err := rows.Scan(&l.P, &l.Q, &l.N, &recent); err != nil {
			rows.Close()
			return nil, err
		}
		b.Sold = append(b.Sold, l)
		if recent > 0 {
			day = append(day, BookLevel{P: l.P, N: recent})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Median by separate sale, as the flip planner prices resales.
	var n int64
	for _, l := range day {
		n += l.N
	}
	if n >= 3 {
		sort.Slice(day, func(i, j int) bool { return day[i].P < day[j].P })
		half, seen := n/2, int64(0)
		for _, l := range day {
			seen += l.N
			if seen > half {
				b.Clearing = l.P
				break
			}
		}
	}
	return b, nil
}

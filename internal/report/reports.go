package report

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

type current struct {
	min    int64
	listed int64
}

// latestStats returns each item's min price and listed quantity in the newest snapshot of each kind.
func latestStats(ctx context.Context, db *sql.DB, houseID int64) (map[Key]current, error) {
	rows, err := db.QueryContext(ctx, `SELECT i.item_id, i.variant, COALESCE(i.min_unit,0), i.listed_qty FROM item_snapshot i
		WHERE i.snapshot_id IN (SELECT MAX(id) FROM snapshot WHERE house_id=? GROUP BY kind)`, houseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[Key]current{}
	for rows.Next() {
		var k Key
		var c current
		if err := rows.Scan(&k.Item, &k.Variant, &c.min, &c.listed); err != nil {
			return nil, err
		}
		m[k] = c
	}
	return m, rows.Err()
}

type DealsOptions struct {
	Options
	MinDiscount     float64
	IncludeVolatile bool
}

// Deals: what is listed below its fair price right now.
func Deals(ctx context.Context, st *store.Store, o DealsOptions) error {
	daily, w, err := LoadDaily(ctx, st.DB, o.HouseID, o.Days, o.Now)
	if err != nil {
		return err
	}
	cur, err := latestStats(ctx, st.DB, o.HouseID)
	if err != nil {
		return err
	}
	names, err := LoadNames(ctx, st.DB)
	if err != nil {
		return err
	}
	type row struct {
		k        Key
		c        current
		d        *Daily
		discount float64
		units    int64
	}
	var rows []row
	for k, c := range cur {
		d := daily[k]
		if d == nil || d.Fair == 0 || c.min == 0 {
			continue
		}
		disc := 1 - float64(c.min)/float64(d.Fair)
		if disc < o.MinDiscount || d.SoldLo < o.MinSold || (d.Volatile() && !o.IncludeVolatile) {
			continue
		}
		rows = append(rows, row{k: k, c: c, d: d, discount: disc})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].discount > rows[j].discount })
	if o.Limit > 0 && len(rows) > o.Limit {
		rows = rows[:o.Limit]
	}
	// Units listed at or below the discount threshold.
	q, err := st.DB.PrepareContext(ctx, `SELECT COALESCE(SUM(qty_last),0) FROM auction WHERE house_id=? AND item_id=? AND variant=?
		AND closed_at IS NULL AND unit_price>0 AND unit_price<=?`)
	if err != nil {
		return err
	}
	defer q.Close()
	for i := range rows {
		limit := int64(float64(rows[i].d.Fair) * (1 - o.MinDiscount))
		if err := q.QueryRowContext(ctx, o.HouseID, rows[i].k.Item, rows[i].k.Variant, limit).Scan(&rows[i].units); err != nil {
			return err
		}
	}

	Header(ctx, st, o.Options, fmt.Sprintf("Deals: ≥%.0f%% under 7-day fair, sold_lo ≥ %s/day", o.MinDiscount*100, num(o.MinSold)), &w)
	t := &Table{Header: []string{"item", "min now", "fair 7d", "discount", "units ≤ threshold", "sold/day est (lo–hi)", "volatile"}}
	for _, r := range rows {
		vol := ""
		if r.d.Volatile() {
			vol = fmt.Sprintf("yes (cv %.2f)", r.d.CV)
		}
		t.Add(ItemLabel(names, r.k), Gold(r.c.min), Gold(r.d.Fair), Pct(r.discount), strconv.FormatInt(r.units, 10),
			Range(r.d.SoldEst, r.d.SoldLo, r.d.SoldHi), vol)
	}
	return t.Write(o.Options)
}

// Movers: what sells reliably.
func Movers(ctx context.Context, st *store.Store, o Options) error {
	daily, w, err := LoadDaily(ctx, st.DB, o.HouseID, o.Days, o.Now)
	if err != nil {
		return err
	}
	names, err := LoadNames(ctx, st.DB)
	if err != nil {
		return err
	}
	type row struct {
		k        Key
		d        *Daily
		turnover float64
	}
	var rows []row
	for k, d := range daily {
		if d.SoldEst < o.MinSold {
			continue
		}
		rows = append(rows, row{k, d, d.SoldEst * float64(d.Fair)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].turnover > rows[j].turnover })
	if o.Limit > 0 && len(rows) > o.Limit {
		rows = rows[:o.Limit]
	}
	Header(ctx, st, o, fmt.Sprintf("Movers: sold_est ≥ %s/day", num(o.MinSold)), &w)
	t := &Table{Header: []string{"item", "sold/day est (lo–hi)", "sell-through", "fair", "gold/day", "avg listed"}}
	for _, r := range rows {
		t.Add(ItemLabel(names, r.k), Range(r.d.SoldEst, r.d.SoldLo, r.d.SoldHi), Pct(r.d.SellThrough()),
			Gold(r.d.Fair), Gold(int64(math.Round(r.turnover))), num(r.d.Listed))
	}
	return t.Write(o)
}

// Item: full history of one item.
func Item(ctx context.Context, st *store.Store, o Options, query string) error {
	id, err := ResolveItem(ctx, st.DB, query)
	if err != nil {
		return err
	}
	names, err := LoadNames(ctx, st.DB)
	if err != nil {
		return err
	}
	from := rollup.DayString(o.Now.AddDate(0, 0, -(o.Days-1)), rollup.Berlin)
	rows, err := st.DB.QueryContext(ctx, `SELECT variant, day, COALESCE(fair_unit,0), COALESCE(min_unit,0), avg_listed, sold_est, sold_lo, sold_hi, ended, price_cv
		FROM item_daily WHERE house_id=? AND item_id=? AND day>=? ORDER BY variant, day`, o.HouseID, id, from)
	if err != nil {
		return err
	}
	defer rows.Close()
	t := &Table{Header: []string{"day", "variant", "fair", "min", "avg listed", "sold est (lo–hi)", "sell-through", "price cv"}}
	var fairs, solds []int64
	for rows.Next() {
		var variant, day string
		var fair, min, listed, est, lo, hi, ended int64
		var cv sql.NullFloat64
		if err := rows.Scan(&variant, &day, &fair, &min, &listed, &est, &lo, &hi, &ended, &cv); err != nil {
			return err
		}
		through := -1.0
		if ended > 0 {
			through = float64(est) / float64(ended)
		}
		cvs := "-"
		if cv.Valid {
			cvs = fmt.Sprintf("%.2f", cv.Float64)
		}
		t.Add(day, variant, Gold(fair), Gold(min), strconv.FormatInt(listed, 10),
			Range(float64(est), float64(lo), float64(hi)), Pct(through), cvs)
		fairs = append(fairs, fair)
		solds = append(solds, est)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	Header(ctx, st, o, fmt.Sprintf("Item %s (%d), last %d days", ItemLabel(names, Key{id, ""}), id, o.Days), nil)
	if !o.CSV && len(fairs) > 0 {
		fmt.Fprintf(o.Out, "fair  %s\nsold  %s\n\n", Sparkline(fairs), Sparkline(solds))
	}
	if err := t.Write(o); err != nil {
		return err
	}
	if !o.CSV {
		cur, err := latestStats(ctx, st.DB, o.HouseID)
		if err == nil {
			if c, ok := cur[Key{id, ""}]; ok {
				fmt.Fprintf(o.Out, "\nnow: min %s, %d listed\n", Gold(c.min), c.listed)
			}
		}
	}
	return nil
}

// ResolveItem maps an item ID or (part of) a name to an item ID.
func ResolveItem(ctx context.Context, db *sql.DB, q string) (int64, error) {
	if id, err := strconv.ParseInt(q, 10, 64); err == nil {
		return id, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM item WHERE name LIKE ? ORDER BY (lower(name)=lower(?)) DESC, length(name) LIMIT 10`, "%"+q+"%", q)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	var opts []string
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return 0, err
		}
		if strings.EqualFold(name, q) {
			return id, nil
		}
		ids = append(ids, id)
		opts = append(opts, fmt.Sprintf("%d %s", id, name))
	}
	switch len(ids) {
	case 0:
		return 0, errors.New("no item matches " + strconv.Quote(q))
	case 1:
		return ids[0], nil
	}
	return 0, fmt.Errorf("%q is ambiguous:\n  %s", q, strings.Join(opts, "\n  "))
}

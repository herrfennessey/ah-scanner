// Package rollup turns snapshots and sale events into one item_daily row per item per day.
package rollup

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/tracker"
)

// Berlin is the display and day-boundary time zone.
var Berlin = mustLoad("Europe/Berlin")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

type Options struct {
	AmbiguousWeight float64 // share of ambiguous units counted in sold_est
	Loc             *time.Location
}

func (o Options) loc() *time.Location {
	if o.Loc != nil {
		return o.Loc
	}
	return Berlin
}

type key struct {
	item    int64
	variant string
}

type acc struct {
	kind      string
	p20s      []int64 // 7-day window
	mins      []int64 // 7-day window
	dayMin    int64
	dayListed int64
	soldLo    int64
	ambiguous int64
	ended     int64
	sales     int64 // separate certain sales (sold auctions + partial commodity buys)
	largest   int64 // units in the single largest certain sale
}

// DayString formats a local date as YYYY-MM-DD.
func DayString(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01-02") }

// DayBounds returns [start, end) Unix seconds for a YYYY-MM-DD date in loc.
func DayBounds(day string, loc *time.Location) (int64, int64, error) {
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return 0, 0, err
	}
	return t.Unix(), t.AddDate(0, 0, 1).Unix(), nil
}

// Day computes item_daily for one house and one local day (YYYY-MM-DD). It is idempotent.
func Day(ctx context.Context, db *sql.DB, houseID int64, day string, opt Options) (int, error) {
	loc := opt.loc()
	start, end, err := DayBounds(day, loc)
	if err != nil {
		return 0, err
	}
	t, _ := time.ParseInLocation("2006-01-02", day, loc)
	winStart := t.AddDate(0, 0, -6).Unix()

	// Number of distinct snapshots per kind on the day, for the average listed quantity.
	snaps := map[string]int64{}
	rows, err := db.QueryContext(ctx, `SELECT kind, count(*) FROM snapshot WHERE house_id=? AND fetched_at>=? AND fetched_at<? GROUP BY kind`, houseID, start, end)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return 0, err
		}
		snaps[k] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	m := map[key]*acc{}
	get := func(k key) *acc {
		a := m[k]
		if a == nil {
			a = &acc{}
			m[k] = a
		}
		return a
	}

	rows, err = db.QueryContext(ctx, `SELECT s.kind, s.fetched_at, i.item_id, i.variant, i.listed_qty, COALESCE(i.min_unit,0), COALESCE(i.p20_unit,0)
		FROM snapshot s JOIN item_snapshot i ON i.snapshot_id = s.id
		WHERE s.house_id=? AND s.fetched_at>=? AND s.fetched_at<?`, houseID, winStart, end)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var kind string
		var at, item, listed, minU, p20 int64
		var variant string
		if err := rows.Scan(&kind, &at, &item, &variant, &listed, &minU, &p20); err != nil {
			rows.Close()
			return 0, err
		}
		a := get(key{item, variant})
		a.kind = kind
		if p20 > 0 {
			a.p20s = append(a.p20s, p20)
		}
		if minU > 0 {
			a.mins = append(a.mins, minU)
		}
		if at >= start {
			a.dayListed += listed
			if minU > 0 && (a.dayMin == 0 || minU < a.dayMin) {
				a.dayMin = minU
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	rows, err = db.QueryContext(ctx, `SELECT item_id, variant, kind, outcome, SUM(units), COUNT(*), MAX(units) FROM (
		SELECT item_id, variant, kind, outcome, units FROM sale_event WHERE house_id=? AND ts>=? AND ts<?
		UNION ALL
		SELECT item_id, variant, kind, outcome, qty_last FROM auction WHERE house_id=? AND closed_at>=? AND closed_at<?
	) GROUP BY item_id, variant, kind, outcome`, houseID, start, end, houseID, start, end)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var item, units, n, biggest int64
		var variant, kind, outcome string
		if err := rows.Scan(&item, &variant, &kind, &outcome, &units, &n, &biggest); err != nil {
			rows.Close()
			return 0, err
		}
		a := get(key{item, variant})
		if a.kind == "" {
			a.kind = kind
		}
		a.ended += units
		switch {
		case tracker.Certain(outcome):
			a.soldLo += units
			a.sales += n
			a.largest = max(a.largest, biggest)
		case tracker.Ambiguous(outcome):
			a.ambiguous += units
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM item_daily WHERE house_id=? AND day=?`, houseID, day); err != nil {
		return 0, err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO item_daily (house_id, item_id, variant, day, fair_unit, min_unit, avg_listed,
		sold_est, sold_lo, sold_hi, ended, price_cv, sales, largest_sale) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	n := 0
	for k, a := range m {
		if a.dayListed == 0 && a.ended == 0 {
			continue // only in the trailing window, not on this day
		}
		var avgListed int64
		if s := snaps[a.kind]; s > 0 {
			avgListed = int64(math.Round(float64(a.dayListed) / float64(s)))
		}
		soldEst := a.soldLo + int64(math.Round(opt.AmbiguousWeight*float64(a.ambiguous)))
		if _, err := ins.ExecContext(ctx, houseID, k.item, k.variant, day,
			nullIfZero(median(a.p20s)), nullIfZero(a.dayMin), avgListed,
			soldEst, a.soldLo, a.soldLo+a.ambiguous, a.ended, cv(a.mins), a.sales, a.largest); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// cv is the coefficient of variation (population stddev / mean), nil with < 2 points.
func cv(v []int64) any {
	if len(v) < 2 {
		return nil
	}
	var sum float64
	for _, x := range v {
		sum += float64(x)
	}
	mean := sum / float64(len(v))
	if mean == 0 {
		return nil
	}
	var ss float64
	for _, x := range v {
		d := float64(x) - mean
		ss += d * d
	}
	return math.Sqrt(ss/float64(len(v))) / mean
}

// Range rolls up every house for each day from `from` to `to` inclusive (local dates).
func Range(ctx context.Context, db *sql.DB, from, to time.Time, opt Options) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM house`)
	if err != nil {
		return 0, err
	}
	var houses []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		houses = append(houses, id)
	}
	rows.Close()
	loc := opt.loc()
	total := 0
	for d := from.In(loc); !d.After(to.In(loc)); d = d.AddDate(0, 0, 1) {
		for _, h := range houses {
			n, err := Day(ctx, db, h, DayString(d, loc), opt)
			if err != nil {
				return total, err
			}
			total += n
		}
	}
	return total, nil
}

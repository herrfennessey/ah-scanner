// Package report renders the CLI reports (deals, movers, craft, item) as tables or CSV.
package report

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"github.com/herrfennessey/ah-scanner/internal/suffix"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

type Options struct {
	HouseID     int64
	HouseName   string
	Cut         float64 // AH cut on sales
	DepositRate float64
	Days        int
	MinSold     float64
	CSV         bool
	Limit       int
	Out         io.Writer
	Now         time.Time
}

type Key struct {
	Item    int64
	Variant string
}

// Daily is an item's averages over the report window, from item_daily.
type Daily struct {
	Fair    int64   // latest fair_unit
	CV      float64 // latest price_cv
	HasCV   bool
	SoldEst float64 // per day
	SoldLo  float64
	SoldHi  float64
	Ended   int64 // total over window
	SoldSum int64 // est total over window
	Listed  float64
	LastDay string
}

func (d *Daily) Volatile() bool { return d.HasCV && d.CV > 0.5 }

// SellThrough is est sold / units that ended, or -1 if nothing ended.
func (d *Daily) SellThrough() float64 {
	if d.Ended == 0 {
		return -1
	}
	return float64(d.SoldSum) / float64(d.Ended)
}

// Window describes which days the averages came from.
type Window struct {
	From, To string
	Days     int
	Partial  bool // only today's partial day was available
}

// LoadDaily averages item_daily over the last `days` complete days (falling back to today
// when no complete day exists yet).
func LoadDaily(ctx context.Context, db *sql.DB, houseID int64, days int, now time.Time) (map[Key]*Daily, Window, error) {
	today := rollup.DayString(now, rollup.Berlin)
	from := rollup.DayString(now.AddDate(0, 0, -days), rollup.Berlin)
	w := Window{From: from, To: rollup.DayString(now.AddDate(0, 0, -1), rollup.Berlin)}
	if err := db.QueryRowContext(ctx, `SELECT count(DISTINCT day) FROM item_daily WHERE house_id=? AND day>=? AND day<?`, houseID, from, today).Scan(&w.Days); err != nil {
		return nil, w, err
	}
	to := today // exclusive
	if w.Days == 0 {
		w.Partial, w.Days, w.From, w.To = true, 1, today, today
		from, to = today, "9999-12-31"
	}
	rows, err := db.QueryContext(ctx, `SELECT item_id, variant, day, COALESCE(fair_unit,0), price_cv, sold_est, sold_lo, sold_hi, ended, avg_listed
		FROM item_daily WHERE house_id=? AND day>=? AND day<? ORDER BY day`, houseID, from, to)
	if err != nil {
		return nil, w, err
	}
	defer rows.Close()
	m := map[Key]*Daily{}
	n := float64(w.Days)
	for rows.Next() {
		var k Key
		var day string
		var fair, est, lo, hi, ended, listed int64
		var cv sql.NullFloat64
		if err := rows.Scan(&k.Item, &k.Variant, &day, &fair, &cv, &est, &lo, &hi, &ended, &listed); err != nil {
			return nil, w, err
		}
		d := m[k]
		if d == nil {
			d = &Daily{}
			m[k] = d
		}
		if fair > 0 {
			d.Fair = fair
		}
		if cv.Valid {
			d.CV, d.HasCV = cv.Float64, true
		}
		d.SoldEst += float64(est) / n
		d.SoldLo += float64(lo) / n
		d.SoldHi += float64(hi) / n
		d.Listed += float64(listed) / n
		d.Ended += ended
		d.SoldSum += est
		d.LastDay = day
	}
	return m, w, rows.Err()
}

// LoadNames maps item IDs to names.
func LoadNames(ctx context.Context, db *sql.DB) (map[int64]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM item WHERE name IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		m[id] = name
	}
	return m, rows.Err()
}

func ItemLabel(names map[int64]string, k Key) string {
	name := names[k.Item]
	if name == "" {
		name = fmt.Sprintf("#%d", k.Item)
	}
	return suffix.Label(name, k.Variant)
}

// Gold formats copper as 12g34s56c, dropping leading zero units.
func Gold(c int64) string {
	if c == 0 {
		return "-"
	}
	neg := ""
	if c < 0 {
		neg, c = "-", -c
	}
	g, s, cp := c/10000, c%10000/100, c%100
	switch {
	case g > 0:
		return fmt.Sprintf("%s%dg%02ds%02dc", neg, g, s, cp)
	case s > 0:
		return fmt.Sprintf("%s%ds%02dc", neg, s, cp)
	}
	return fmt.Sprintf("%s%dc", neg, cp)
}

func Range(est, lo, hi float64) string {
	return fmt.Sprintf("%s (%s–%s)", num(est), num(lo), num(hi))
}

func num(v float64) string {
	if v >= 10 || v == 0 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func Pct(v float64) string {
	if v < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

// Table writes aligned columns or CSV.
type Table struct {
	Header []string
	Rows   [][]string
}

func (t *Table) Add(cells ...string) { t.Rows = append(t.Rows, cells) }

func (t *Table) Write(o Options) error {
	if o.CSV {
		w := csv.NewWriter(o.Out)
		w.Write(t.Header)
		w.WriteAll(t.Rows)
		return w.Error()
	}
	if len(t.Rows) == 0 {
		_, err := fmt.Fprintln(o.Out, "(no rows match the filters)")
		return err
	}
	tw := tabwriter.NewWriter(o.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(t.Header, "\t"))
	seps := make([]string, len(t.Header))
	for i, h := range t.Header {
		seps[i] = strings.Repeat("─", utf8.RuneCountInString(h))
	}
	fmt.Fprintln(tw, strings.Join(seps, "\t"))
	for _, r := range t.Rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

// Header prints the report title plus data-freshness caveats (skipped for CSV).
func Header(ctx context.Context, st *store.Store, o Options, title string, w *Window) {
	if o.CSV {
		return
	}
	fmt.Fprintf(o.Out, "%s — %s (house %d)\n", title, o.HouseName, o.HouseID)
	if w != nil {
		if w.Partial {
			fmt.Fprintf(o.Out, "sold/day from today's partial data only (%s); numbers will grow as the day fills\n", w.From)
		} else {
			fmt.Fprintf(o.Out, "sold/day averaged over %d day(s), %s → %s\n", w.Days, w.From, w.To)
		}
	}
	for _, k := range []listing.Kind{listing.Commodity, listing.Gear} {
		iv, err := st.EffectiveInterval(ctx, o.HouseID, k, 24*time.Hour)
		if err == nil && iv > 45*time.Minute {
			fmt.Fprintf(o.Out, "note: %s data refreshed only every ~%s over the last 24 h; fast movers are undercounted\n", k, iv.Round(time.Minute))
		}
	}
	fmt.Fprintln(o.Out)
}

// Sparkline renders values as ▁▂▃▄▅▆▇█ (zeros as a space).
func Sparkline(vals []int64) string {
	bars := []rune("▁▂▃▄▅▆▇█")
	var lo, hi int64
	first := true
	for _, v := range vals {
		if v == 0 {
			continue
		}
		if first || v < lo {
			lo = v
		}
		if first || v > hi {
			hi = v
		}
		first = false
	}
	var b strings.Builder
	for _, v := range vals {
		switch {
		case v == 0:
			b.WriteRune(' ')
		case hi == lo:
			b.WriteRune(bars[3])
		default:
			b.WriteRune(bars[int(float64(v-lo)/float64(hi-lo)*float64(len(bars)-1)+0.5)])
		}
	}
	return b.String()
}

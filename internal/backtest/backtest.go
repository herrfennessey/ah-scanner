// Package backtest replays the flip planner over the recorded history and scores each flip by
// what the market did afterwards, for a player who posts once and walks away.
//
// At each planning time the planner sees the inputs as they were then (report.BuildFlipInputs
// rebuilds them). A flip's resale listing is then filled only by buyers who really paid at
// least its price after it was posted: a buyer who paid more would have taken the cheaper
// listing first. Listings that undercut it in the meantime are in the history too, so the
// buyers they took aren't counted. The listing is never relisted; whatever hasn't sold by the
// end of the hold is valued at the cheapest price listed then.
package backtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

// Options says when to plan and what the player looks like.
type Options struct {
	From, To    time.Time     // plan at the scans in this window (To is cut back so every hold ends in the history)
	Every       time.Duration // at most one planning time per this (default 1h)
	Hold        time.Duration // how long a resale listing stays up, unattended (default 24h)
	Gold        int64         // copper; default 40g
	Risks       []report.Risk // default low, medium, high
	DepositRate float64
	// Houses as the collector resolved them; default the ones in the stored flip cache.
	Houses []report.FlipHouse
}

// Trade is one flip the planner proposed and what came of it.
type Trade struct {
	At     int64       `json:"at"`
	Risk   report.Risk `json:"risk"`
	Flip   report.Flip `json:"flip"`
	Sold   int64       `json:"sold"`   // units the buyers after At would have taken within the hold
	Cash   int64       `json:"cash"`   // copper back minus cost and lost deposit, unsold stock counted as nothing
	Marked int64       `json:"marked"` // Cash plus the unsold stock at the cheapest price at the end of the hold
	Floor  int64       `json:"floor"`  // that cheapest price (0: none listed, valued at the resale price)
	Hours  float64     `json:"hours"`  // until the last unit sold (the hold if not all did)
	cut    float64
}

// Summary totals one risk level.
type Summary struct {
	Risk       report.Risk `json:"risk"`
	Trades     int         `json:"trades"`
	Cost       int64       `json:"cost"`
	Expected   int64       `json:"expected"`   // what the planner promised
	Cash       int64       `json:"cash"`       // sold within the hold, unsold stock as nothing
	Marked     int64       `json:"marked"`     // unsold stock at the end-of-hold floor
	UnitsSold  float64     `json:"units_sold"` // share of bought units sold within the hold
	AllSold    float64     `json:"all_sold"`   // share of trades that sold out
	Losing     float64     `json:"losing"`     // share of trades with Marked < 0
	MedianHold float64     `json:"median_hours"`
}

// Run plans at every scan in the window and scores the flips.
func Run(ctx context.Context, st *store.Store, o Options) ([]Trade, error) {
	if o.Every <= 0 {
		o.Every = time.Hour
	}
	if o.Hold <= 0 {
		o.Hold = 24 * time.Hour
	}
	if len(o.Risks) == 0 {
		o.Risks = []report.Risk{report.RiskLow, report.RiskMedium, report.RiskHigh}
	}
	if len(o.Houses) == 0 {
		h, err := cachedHouses(ctx, st)
		if err != nil {
			return nil, err
		}
		o.Houses = h
	}
	var last sql.NullInt64
	if err := st.DB.QueryRowContext(ctx, `SELECT MAX(fetched_at) FROM snapshot`).Scan(&last); err != nil || !last.Valid {
		return nil, fmt.Errorf("no scans: %v", err)
	}
	to := min(o.To.Unix(), last.Int64-int64(o.Hold.Seconds()))
	times, err := planTimes(ctx, st.DB, o.From.Unix(), to, int64(o.Every.Seconds()))
	if err != nil {
		return nil, err
	}
	cut := map[string]float64{}
	ids := map[string]int64{}
	for _, h := range o.Houses {
		cut[h.Faction], ids[h.Faction] = h.Cut, h.ID
	}
	var out []Trade
	busy := map[string]int64{} // risk|item|route → when its stock is gone (one position at a time)
	for _, t := range times {
		in, err := report.BuildFlipInputs(ctx, st, o.Houses, o.DepositRate, time.Unix(t, 0))
		if err != nil {
			return nil, fmt.Errorf("inputs at %d: %w", t, err)
		}
		for _, r := range o.Risks {
			flips, _, _ := report.PlanFlips(in, report.FlipOptions{Gold: o.Gold, Risk: r})
			for _, f := range flips {
				key := fmt.Sprint(r, "|", f.ItemID, "|", f.Variant, "|", f.BuyHouse, ">", f.SellHouse)
				if busy[key] > t {
					continue // already holding this one: the same cheap listings can't be bought twice
				}
				tr := Trade{At: t, Risk: r, Flip: f, cut: cut[f.SellHouse]}
				if err := fill(ctx, st.DB, ids[f.SellHouse], t, int64(o.Hold.Seconds()), &tr); err != nil {
					return nil, err
				}
				busy[key] = t + int64(tr.Hours*3600)
				out = append(out, tr)
			}
		}
	}
	return out, nil
}

// cachedHouses reads the houses from the stored flip inputs.
func cachedHouses(ctx context.Context, st *store.Store) ([]report.FlipHouse, error) {
	_, b, err := st.LoadFlipCache(ctx)
	if err != nil || b == nil {
		return nil, fmt.Errorf("no flip cache to take the houses from (run the collector once): %v", err)
	}
	var in report.FlipInputs
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	return in.Houses, nil
}

// planTimes are the scan times in [from, to], at least every apart.
func planTimes(ctx context.Context, db *sql.DB, from, to, every int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT fetched_at FROM snapshot WHERE fetched_at>=? AND fetched_at<=? ORDER BY fetched_at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		if len(out) == 0 || t-out[len(out)-1] >= every {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// fill plays the resale listing against the purchases made after it was posted.
func fill(ctx context.Context, db *sql.DB, house, t, hold int64, tr *Trade) error {
	f := tr.Flip
	end := t + hold
	rows, err := db.QueryContext(ctx, `SELECT ts, units FROM (
		SELECT ts, units, unit_price FROM sale_event WHERE house_id=? AND item_id=? AND variant=? AND ts>? AND ts<=? AND outcome IN ('sold','partial')
		UNION ALL
		SELECT closed_at, qty_last, unit_price FROM auction WHERE house_id=? AND item_id=? AND variant=? AND closed_at>? AND closed_at<=? AND outcome='sold'
	) WHERE unit_price>=? ORDER BY ts`, house, f.ItemID, f.Variant, t, end, house, f.ItemID, f.Variant, t, end, f.SellAt)
	if err != nil {
		return err
	}
	lastTS := end
	for rows.Next() {
		var ts, units int64
		if err := rows.Scan(&ts, &units); err != nil {
			rows.Close()
			return err
		}
		tr.Sold += min(units, f.BuyUnits-tr.Sold)
		if tr.Sold >= f.BuyUnits {
			lastTS = ts
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tr.Hours = float64(lastTS-t) / 3600
	unsold := f.BuyUnits - tr.Sold
	gross := float64(tr.Sold) * float64(f.SellAt) * (1 - tr.cut)
	lost := 0.0
	if unsold > 0 {
		lost = float64(f.DepositAtRisk) * float64(unsold) / float64(f.BuyUnits)
	}
	tr.Cash = int64(math.Round(gross - float64(f.BuyCost) - lost))
	tr.Marked = tr.Cash
	if unsold > 0 {
		err := db.QueryRowContext(ctx, `SELECT COALESCE(i.min_unit, 0) FROM item_snapshot i JOIN snapshot s ON s.id=i.snapshot_id
			WHERE s.house_id=? AND i.item_id=? AND i.variant=? AND s.fetched_at<=? ORDER BY s.fetched_at DESC LIMIT 1`,
			house, f.ItemID, f.Variant, end).Scan(&tr.Floor)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		v := f.SellAt
		if tr.Floor > 0 {
			v = min(tr.Floor, f.SellAt)
		}
		tr.Marked += int64(math.Round(float64(unsold) * float64(v) * (1 - tr.cut)))
	}
	return nil
}

// Summarize totals the trades per risk level.
func Summarize(trades []Trade) []Summary {
	by := map[report.Risk]*Summary{}
	units := map[report.Risk][2]int64{}
	hours := map[report.Risk][]float64{}
	var order []report.Risk
	for _, tr := range trades {
		s := by[tr.Risk]
		if s == nil {
			s = &Summary{Risk: tr.Risk}
			by[tr.Risk] = s
			order = append(order, tr.Risk)
		}
		s.Trades++
		s.Cost += tr.Flip.BuyCost
		s.Expected += tr.Flip.Expected
		s.Cash += tr.Cash
		s.Marked += tr.Marked
		u := units[tr.Risk]
		units[tr.Risk] = [2]int64{u[0] + tr.Sold, u[1] + tr.Flip.BuyUnits}
		if tr.Sold == tr.Flip.BuyUnits {
			s.AllSold++
		}
		if tr.Marked < 0 {
			s.Losing++
		}
		hours[tr.Risk] = append(hours[tr.Risk], tr.Hours)
	}
	var out []Summary
	for _, r := range order {
		s := by[r]
		n := float64(s.Trades)
		s.AllSold /= n
		s.Losing /= n
		if u := units[r]; u[1] > 0 {
			s.UnitsSold = float64(u[0]) / float64(u[1])
		}
		h := hours[r]
		sort.Float64s(h)
		s.MedianHold = h[len(h)/2]
		out = append(out, *s)
	}
	return out
}

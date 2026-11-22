// Package backtest replays the flip planner over the recorded history and scores each flip by
// what the market did afterwards, for a player of each trading style.
//
// At each planning time the planner sees the inputs as they were then (report.BuildFlipInputs
// rebuilds them). A flip's resale listing is then filled only by buyers who really paid at
// least its price after it was posted: a buyer who paid more would have taken the cheaper
// listing first. Listings that undercut it in the meantime are in the history too, so the
// buyers they took aren't counted. Each flip is played as planned: a hold is left alone; a
// check-in relists at a visit when the queue cheaper than it is longer than buyers would get
// through before it runs out; a camp relists whenever undercut. A relist (one more deposit) goes
// just under the cheapest other listing, never below what it cost. Whatever hasn't sold by the
// end of the window is valued at the cheapest price listed then.
package backtest

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/store"
	"github.com/herrfennessey/ah-scanner/internal/version"
)

// Options says when to plan and what the player looks like.
type Options struct {
	From, To    time.Time      // plan at the scans in this window (To is cut back so every hold ends in the history)
	Every       time.Duration  // at most one planning time per this (default 1h)
	Gold        int64          // copper; default 40g
	Styles      []report.Style // default all
	DepositRate float64
	// Houses as the collector resolved them; default the ones in the stored flip cache.
	Houses []report.FlipHouse
	// QueueScale scales the check-in relist rule's queue (default 1): under 1 relists sooner.
	QueueScale float64

	// Inputs: "stored" (default) replays the flip inputs in flip_history (the collector's own
	// after each scan, else ones a backtest rebuilt) and rebuilds only what's missing; "rebuild"
	// rebuilds every planning time (after changing how inputs are built). Rebuilt inputs are saved.
	Inputs string
	// Read is a read-only store with several connections for rebuilding inputs in parallel
	// (store.OpenReadOnly); nil rebuilds them one at a time on st.
	Read *store.Store
	// Workers is how many planning times to rebuild at once (default: Read's, or 1).
	Workers int
	// Log reports progress (nil: quiet).
	Log func(format string, args ...any)
}

// liveMatch is how long after a planning time a live flip_history row may have been computed
// and still stand for it (the collector refreshes within a minute or two of a publish).
const liveMatch = 10 * 60

// Trade is one flip the planner proposed and what came of it.
type Trade struct {
	At      int64        `json:"at"`
	Style   report.Style `json:"style"`
	Flip    report.Flip  `json:"flip"`
	Sold    int64        `json:"sold"`    // units the buyers after At would have taken within the window
	Cash    int64        `json:"cash"`    // copper back minus cost and lost deposits, unsold stock counted as nothing
	Marked  int64        `json:"marked"`  // Cash plus the unsold stock at the cheapest price at the end of the window
	Floor   int64        `json:"floor"`   // the cheapest other listing then (0: none, valued at the last price)
	Hours   float64      `json:"hours"`   // until the last unit sold (the window if not all did)
	Relists int          `json:"relists"` // visits at which it was relisted
	cut     float64
	queue   float64
}

// Summary totals one style.
type Summary struct {
	Style      report.Style `json:"style"`
	Play       report.Play  `json:"play"` // "" totals every play of the style
	Trades     int          `json:"trades"`
	Cost       int64        `json:"cost"`
	Expected   int64        `json:"expected"`   // what the planner promised
	Cash       int64        `json:"cash"`       // sold within the window, unsold stock as nothing
	Marked     int64        `json:"marked"`     // unsold stock at the end-of-window floor
	UnitsSold  float64      `json:"units_sold"` // share of bought units sold within the window
	AllSold    float64      `json:"all_sold"`   // share of trades that sold out
	Losing     float64      `json:"losing"`     // share of trades with Marked < 0
	MedianHold float64      `json:"median_hours"`
}

// Run plans at every scan in the window and scores the flips.
func Run(ctx context.Context, st *store.Store, o Options) ([]Trade, error) {
	if o.Every <= 0 {
		o.Every = time.Hour
	}
	if len(o.Styles) == 0 {
		o.Styles = report.Styles
	}
	if len(o.Houses) == 0 {
		h, err := report.CachedHouses(ctx, st)
		if err != nil {
			return nil, err
		}
		o.Houses = h
	}
	var last sql.NullInt64
	if err := st.DB.QueryRowContext(ctx, `SELECT MAX(fetched_at) FROM snapshot`).Scan(&last); err != nil {
		return nil, err
	} else if !last.Valid {
		return nil, fmt.Errorf("no scans")
	}
	if o.QueueScale <= 0 {
		o.QueueScale = 1
	}
	longest := 0.0
	for _, p := range report.Plays {
		_, w, _, _, _ := report.PlayTiming(p)
		longest = max(longest, w)
	}
	to := min(o.To.Unix(), last.Int64-int64(longest*3600))
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
	busy := map[string]int64{} // style|item|route → when its stock is gone (one position at a time)
	// What each style has bought out of the recorded order books: those listings are still in
	// the history, so later planning times (and other routes at the same time) must not buy
	// them again.
	bought := map[report.Style]*consumed{}
	for _, r := range o.Styles {
		bought[r] = &consumed{}
	}
	stored, err := inputsFor(ctx, st, o, times)
	if err != nil {
		return nil, err
	}
	for _, t := range times {
		in, err := report.DecodeInputs(stored[t])
		if err != nil {
			return nil, fmt.Errorf("inputs at %d: %w", t, err)
		}
		for _, r := range o.Styles {
			c := bought[r]
			c.expire(t)
			flips, _, _ := report.PlanFlips(c.apply(in), report.FlipOptions{Gold: o.Gold, Style: r})
			for _, f := range flips {
				key := fmt.Sprint(r, "|", f.ItemID, "|", f.Variant, "|", f.BuyHouse, ">", f.SellHouse)
				if busy[key] > t {
					continue // already holding this one
				}
				if !c.take(in, f, t) {
					continue // another route at this time already bought those listings
				}
				tr := Trade{At: t, Style: r, Flip: f, cut: cut[f.SellHouse], queue: o.QueueScale}
				if err := fill(ctx, st.DB, ids[f.SellHouse], t, &tr); err != nil {
					return nil, err
				}
				busy[key] = t + int64(tr.Hours*3600)
				out = append(out, tr)
			}
		}
	}
	return out, nil
}

// inputsFor returns compressed flip inputs for every planning time: replayed from flip_history
// where possible, rebuilt (in parallel on o.Read) and saved where not. Planning itself stays
// sequential: each time depends on what was bought and held before it.
func inputsFor(ctx context.Context, st *store.Store, o Options, times []int64) (map[int64][]byte, error) {
	out := make(map[int64][]byte, len(times))
	if len(times) == 0 {
		return out, nil
	}
	var missing []int64
	if o.Inputs == "rebuild" {
		missing = times
	} else {
		rows, err := st.FlipHistory(ctx, times[0], times[len(times)-1]+liveMatch)
		if err != nil {
			return nil, err
		}
		j := 0
		for _, t := range times {
			for j < len(rows) && rows[j].At < t {
				j++
			}
			// The first row at or after t: a live one computed right after that scan, or the
			// one a backtest rebuilt for exactly t.
			for k := j; k < len(rows) && rows[k].At < t+liveMatch; k++ {
				if r := rows[k]; (r.Source == store.FlipHistoryLive) || (r.Source == store.FlipHistoryRebuilt && r.At == t) {
					out[t] = r.Data
					break
				}
			}
			if out[t] == nil {
				missing = append(missing, t)
			}
		}
	}
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	logf("inputs: %d of %d planning times stored, rebuilding %d", len(times)-len(missing), len(times), len(missing))
	if len(missing) == 0 {
		return out, nil
	}
	read, workers := o.Read, o.Workers
	if read == nil {
		read, workers = st, 1
	}
	if workers <= 0 {
		workers = 1
	}
	type result struct {
		t   int64
		z   []byte
		err error
	}
	jobs := make(chan int64)
	results := make(chan result)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for w := 0; w < workers; w++ {
		go func() {
			for t := range jobs {
				in, err := report.BuildFlipInputsAt(ctx, read, o.Houses, o.DepositRate, time.Unix(t, 0))
				var z []byte
				if err == nil {
					z, err = report.EncodeInputs(in)
				}
				select {
				case results <- result{t, z, err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, t := range missing {
			select {
			case jobs <- t:
			case <-ctx.Done():
				return
			}
		}
	}()
	start := time.Now()
	for done := 1; done <= len(missing); done++ {
		r := <-results
		if r.err != nil {
			return nil, fmt.Errorf("inputs at %d: %w", r.t, r.err)
		}
		out[r.t] = r.z
		// Saved on the writer so the next backtest replays them.
		if err := st.SaveFlipHistory(ctx, r.t, store.FlipHistoryRebuilt, version.String(), r.z); err != nil {
			return nil, err
		}
		if done%10 == 0 || done == len(missing) {
			logf("inputs: rebuilt %d/%d (%s)", done, len(missing), time.Since(start).Round(time.Second))
		}
	}
	return out, nil
}

// consumedFor is how long a bought listing is assumed to stay in the recorded history: the
// longest listing duration. After that, units at the same price are someone else's.
const consumedFor = 48 * 3600

type levelKey struct {
	house   string
	item    int64
	variant string
	price   int64
}

type buy struct {
	units, at int64
}

// consumed tracks the units a style bought out of the recorded order books, by price level.
type consumed struct{ m map[levelKey][]buy }

func (c *consumed) expire(t int64) {
	for k, v := range c.m {
		keep := v[:0]
		for _, b := range v {
			if t-b.at < consumedFor {
				keep = append(keep, b)
			}
		}
		if len(keep) == 0 {
			delete(c.m, k)
		} else {
			c.m[k] = keep
		}
	}
}

func (c *consumed) units(k levelKey) int64 {
	var n int64
	for _, b := range c.m[k] {
		n += b.units
	}
	return n
}

// apply returns the inputs with the bought units taken out of the order books. Items without
// purchases are shared, not copied.
func (c *consumed) apply(in *report.FlipInputs) *report.FlipInputs {
	if len(c.m) == 0 {
		return in
	}
	touched := map[[2]any]bool{}
	for k := range c.m {
		touched[[2]any{k.item, k.variant}] = true
	}
	out := *in
	out.Items = make([]report.FlipItem, len(in.Items))
	for i, it := range in.Items {
		out.Items[i] = it
		if !touched[[2]any{it.Item, it.Variant}] {
			continue
		}
		houses := make(map[string]*report.FlipStat, len(it.Houses))
		for f, s := range it.Houses {
			houses[f] = s
			if s == nil || len(s.Book) == 0 {
				continue
			}
			cp := *s
			cp.Book = nil
			var gone int64
			for _, l := range s.Book {
				q := l.Q - min(l.Q, c.units(levelKey{f, it.Item, it.Variant, l.P}))
				gone += l.Q - q
				if q > 0 {
					cp.Book = append(cp.Book, report.FlipLevel{P: l.P, Q: q})
				}
			}
			if gone == 0 {
				continue
			}
			cp.Listed = max(0, cp.Listed-gone)
			cp.Min = 0
			if len(cp.Book) > 0 {
				cp.Min = cp.Book[0].P
			}
			houses[f] = &cp
		}
		out.Items[i].Houses = houses
	}
	return &out
}

// take records a flip's purchase, cheapest first, from the recorded book less what was already
// bought. It reports false (recording nothing) if those units are no longer there.
func (c *consumed) take(in *report.FlipInputs, f report.Flip, t int64) bool {
	var book []report.FlipLevel
	for _, it := range in.Items {
		if it.Item == f.ItemID && it.Variant == f.Variant {
			if s := it.Houses[f.BuyHouse]; s != nil {
				book = s.Book
			}
			break
		}
	}
	need := f.BuyUnits
	var takes []struct {
		k levelKey
		n int64
	}
	for _, l := range book {
		if l.P > f.BuyUpTo || need == 0 {
			break
		}
		k := levelKey{f.BuyHouse, f.ItemID, f.Variant, l.P}
		n := min(need, l.Q-min(l.Q, c.units(k)))
		if n > 0 {
			takes = append(takes, struct {
				k levelKey
				n int64
			}{k, n})
			need -= n
		}
	}
	if need > 0 {
		return false
	}
	if c.m == nil {
		c.m = map[levelKey][]buy{}
	}
	for _, x := range takes {
		c.m[x.k] = append(c.m[x.k], buy{x.n, t})
	}
	return true
}

// planTimes are the scan times in [from, to], at least every apart.
func planTimes(ctx context.Context, db *sql.DB, from, to, every int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT fetched_at FROM snapshot WHERE fetched_at>=? AND fetched_at<=? ORDER BY fetched_at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// The first scan in each slot of a grid fixed to the epoch (not to when the backtest
	// started), so every run picks the same scans and replays the inputs stored for them.
	var out []int64
	next := int64(0)
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		if t >= next {
			out = append(out, t)
			next = (t/every + 1) * every
		}
	}
	return out, rows.Err()
}

// fill plays the resale listing against the purchases made after it was posted, by the flip's
// play. Between a listing running out and the next visit, nothing of ours is up.
func fill(ctx context.Context, db *sql.DB, house, t int64, tr *Trade) error {
	f := tr.Flip
	checkH, windowH, listH, rounds, relists := report.PlayTiming(f.Play)
	if tr.queue <= 0 {
		tr.queue = 1
	}
	end := t + int64(windowH*3600)
	type sale struct{ ts, units, price int64 }
	rows, err := db.QueryContext(ctx, `SELECT closed_at, qty_last, unit_price FROM auction
			WHERE house_id=? AND item_id=? AND variant=? AND closed_at>? AND closed_at<=? AND outcome='sold'
		UNION ALL
		SELECT ts, units, unit_price FROM sale_event WHERE house_id=? AND item_id=? AND variant=? AND ts>? AND ts<=? AND outcome='partial'
		ORDER BY 1`, house, f.ItemID, f.Variant, t, end, house, f.ItemID, f.Variant, t, end)
	if err != nil {
		return err
	}
	var sales []sale
	for rows.Next() {
		var x sale
		if err := rows.Scan(&x.ts, &x.units, &x.price); err != nil {
			rows.Close()
			return err
		}
		sales = append(sales, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Every partial sale after t, by auction, to rebuild what an open listing still had at a
	// visit: its last count plus all it lost later, including after the window (a check-in only).
	var partials map[[2]any][]sale
	if f.Play == report.PlayCheckIn {
		partials = map[[2]any][]sale{}
		rows, err := db.QueryContext(ctx, `SELECT kind, auction_id, ts, units FROM sale_event
			WHERE house_id=? AND item_id=? AND variant=? AND ts>? AND outcome='partial'`, house, f.ItemID, f.Variant, t)
		if err != nil {
			return err
		}
		for rows.Next() {
			var kind string
			var id int64
			var x sale
			if err := rows.Scan(&kind, &id, &x.ts, &x.units); err != nil {
				rows.Close()
				return err
			}
			k := [2]any{kind, id}
			partials[k] = append(partials[k], x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	// The listings the flip bought are still in the history (nobody really bought them): for a
	// relist in the same house, leave out what was open at t up to the buy price, or they'd look
	// like undercuts.
	notBought := ""
	args := func(at int64, extra ...any) []any {
		a := append([]any{house, f.ItemID, f.Variant}, extra...)
		a = append(a, at, at)
		if f.Kind == "relist" {
			a = append(a, t, f.BuyUpTo, t)
		}
		return a
	}
	if f.Kind == "relist" {
		notBought = ` AND NOT (first_seen<=? AND unit_price<=? AND (closed_at IS NULL OR closed_at>?))`
	}
	floorAt := func(at int64) (int64, error) {
		var p sql.NullInt64
		var err error
		if f.Kind == "relist" {
			err = db.QueryRowContext(ctx, `SELECT MIN(unit_price) FROM auction
				WHERE house_id=? AND item_id=? AND variant=? AND unit_price>0 AND first_seen<=? AND (closed_at IS NULL OR closed_at>?)`+notBought,
				args(at)...).Scan(&p)
		} else {
			err = db.QueryRowContext(ctx, `SELECT i.min_unit FROM item_snapshot i JOIN snapshot s ON s.id=i.snapshot_id
				WHERE s.house_id=? AND i.item_id=? AND i.variant=? AND s.fetched_at<=? ORDER BY s.fetched_at DESC LIMIT 1`,
				house, f.ItemID, f.Variant, at).Scan(&p)
		}
		if err != nil && err != sql.ErrNoRows {
			return 0, err
		}
		return p.Int64, nil
	}
	// aheadAt is the units listed cheaper than price at a visit: what open listings had left
	// then (their last count, plus what was bought from them later).
	aheadAt := func(at, price int64) (int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT kind, auction_id, qty_last FROM auction
			WHERE house_id=? AND item_id=? AND variant=? AND unit_price>0 AND unit_price<? AND first_seen<=? AND (closed_at IS NULL OR closed_at>?)`+notBought,
			args(at, price)...)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		var n int64
		for rows.Next() {
			var kind string
			var id, q int64
			if err := rows.Scan(&kind, &id, &q); err != nil {
				return 0, err
			}
			for _, p := range partials[[2]any{kind, id}] {
				if p.ts > at {
					q += p.units
				}
			}
			n += q
		}
		return n, rows.Err()
	}
	price, gross, lost := f.SellAt, 0.0, 0.0
	depositShare := func() float64 { return float64(f.DepositAtRisk) * float64(f.BuyUnits-tr.Sold) / float64(f.BuyUnits) }
	lastTS := end
	listed, life := t, int64(listH*3600)
	i := 0
	for k := 0; k < rounds && tr.Sold < f.BuyUnits; k++ {
		roundEnd := t + int64(float64(k+1)*checkH*3600)
		if k == rounds-1 {
			roundEnd = end
		}
		for ; i < len(sales) && sales[i].ts <= roundEnd && tr.Sold < f.BuyUnits; i++ {
			if sales[i].price < price || sales[i].ts > listed+life {
				continue // cheaper than ours, or ours had run out
			}
			take := min(sales[i].units, f.BuyUnits-tr.Sold)
			tr.Sold += take
			gross += float64(take) * float64(price) * (1 - tr.cut)
			if tr.Sold == f.BuyUnits {
				lastTS = sales[i].ts
			}
		}
		if !relists || tr.Sold >= f.BuyUnits || k == rounds-1 {
			continue
		}
		// The visit.
		fl, err := floorAt(roundEnd)
		if err != nil {
			return err
		}
		undercut := fl > 0 && fl <= price
		lapsed := roundEnd >= listed+life
		move := undercut
		if undercut && f.Play == report.PlayCheckIn && !lapsed {
			// Only when buyers wouldn't get through the cheaper queue and ours before it runs out.
			ahead, err := aheadAt(roundEnd, price)
			if err != nil {
				return err
			}
			left := float64(listed+life-roundEnd) / 3600
			limit := tr.queue * float64(report.RelistQueue(f.Demand, left))
			move = float64(ahead) > limit
		}
		if move || lapsed {
			lost += depositShare()
			if undercut {
				price = max(fl-1, f.BuyUpTo)
			}
			listed = roundEnd
			tr.Relists++
		}
	}
	tr.Hours = float64(lastTS-t) / 3600
	unsold := f.BuyUnits - tr.Sold
	if unsold > 0 {
		lost += depositShare()
	}
	tr.Cash = int64(math.Round(gross - float64(f.BuyCost) - lost))
	tr.Marked = tr.Cash
	if unsold > 0 {
		fl, err := floorAt(end)
		if err != nil {
			return err
		}
		tr.Floor = fl
		v := price
		if fl > 0 {
			v = min(fl, price)
		}
		tr.Marked += int64(math.Round(float64(unsold) * float64(v) * (1 - tr.cut)))
	}
	return nil
}

// Summarize totals the trades per trading style.
func Summarize(trades []Trade) []Summary {
	type key struct {
		s report.Style
		p report.Play
	}
	by := map[key]*Summary{}
	units := map[key][2]int64{}
	hours := map[key][]float64{}
	var order []key
	add := func(r key, tr Trade) {
		s := by[r]
		if s == nil {
			s = &Summary{Style: r.s, Play: r.p}
			by[r] = s
			order = append(order, r)
		}
		s.Trades++
		s.Cost += tr.Flip.BuyCost
		s.Expected += tr.Flip.Expected
		s.Cash += tr.Cash
		s.Marked += tr.Marked
		u := units[r]
		units[r] = [2]int64{u[0] + tr.Sold, u[1] + tr.Flip.BuyUnits}
		if tr.Sold == tr.Flip.BuyUnits {
			s.AllSold++
		}
		if tr.Marked < 0 {
			s.Losing++
		}
		hours[r] = append(hours[r], tr.Hours)
	}
	for _, tr := range trades {
		add(key{tr.Style, ""}, tr)
		add(key{tr.Style, tr.Flip.Play}, tr)
	}
	// Each style's total, then its plays, least attentive first.
	rank := func(k key) (int, int) {
		si, pi := len(report.Styles), 0
		for i, x := range report.Styles {
			if x == k.s {
				si = i
			}
		}
		for i, x := range report.Plays {
			if x == k.p {
				pi = i + 1
			}
		}
		return si, pi
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		as, ap := rank(a)
		bs, bp := rank(b)
		if as != bs {
			return as < bs
		}
		return ap < bp
	})
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

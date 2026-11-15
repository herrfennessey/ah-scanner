package report

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

// Flips are built in two steps so requests stay instant:
//
//  1. BuildFlipInputs (expensive, after each publish): per item, each house's market value,
//     sales speed and the cheap end of its order book. Only items with a plausible price gap.
//  2. PlanFlips (cheap, per request): applies the player's gold and the rules to those inputs.

// FlipHouse is a house as the flip planner sees it.
type FlipHouse struct {
	ID       int64   `json:"id"`
	Faction  string  `json:"faction"`
	Cut      float64 `json:"cut"`
	ViewOnly bool    `json:"view_only"` // can't trade here (Alliance from a Horde character)
	Tracked  bool    `json:"tracked"`   // auctions tracked, so sales are known
}

// FlipLevel is one price level of an order book.
type FlipLevel struct {
	P int64 `json:"p"` // unit price
	Q int64 `json:"q"` // units
}

// FlipStat is one item on one house.
type FlipStat struct {
	Fair         int64       `json:"fair"`           // 7-day market value
	Min          int64       `json:"min"`            // lowest unit price now
	Listed       int64       `json:"listed"`         // units listed now
	SoldPerDay   float64     `json:"sold_per_day"`   // certain sales (floor), units/day
	RobustPerDay float64     `json:"robust_per_day"` // same, without the single largest sale
	SalesPerDay  float64     `json:"sales_per_day"`  // separate certain sales per day
	Sales        int64       `json:"sales"`          // separate certain sales in the window
	Volatile     bool        `json:"volatile"`
	Book         []FlipLevel `json:"book,omitempty"` // cheapest first (tradeable houses only)
	// Beyond is the price of the first level left out of a truncated Book (0 = Book is
	// complete), so buying the whole cached book can't hide the next competing listing.
	Beyond int64 `json:"beyond,omitempty"`
}

type FlipItem struct {
	Item    int64                `json:"item"`
	Variant string               `json:"variant,omitempty"`
	Name    string               `json:"name"`
	Deposit int64                `json:"deposit"` // estimated deposit per unit
	Houses  map[string]*FlipStat `json:"houses"`  // by faction
}

// FlipInputs is everything PlanFlips needs; stored after each publish.
type FlipInputs struct {
	ComputedAt int64       `json:"computed_at"`
	SpanDays   float64     `json:"span_days"` // history behind the sales rates
	Houses     []FlipHouse `json:"houses"`
	Items      []FlipItem  `json:"items"`
}

func (in *FlipInputs) Confidence() string {
	switch {
	case in.SpanDays < 3:
		return "early"
	case in.SpanDays < 10:
		return "fair"
	}
	return "good"
}

const bookLevels = 40

// BuildFlipInputs reads current order books and 7 days of daily rows for every house.
func BuildFlipInputs(ctx context.Context, st *store.Store, houses []FlipHouse, depositRate float64, now time.Time) (*FlipInputs, error) {
	in := &FlipInputs{ComputedAt: now.Unix(), Houses: houses, SpanDays: 7}
	from := rollup.DayString(now.AddDate(0, 0, -6), rollup.Berlin)
	dayStart, _, _ := rollup.DayBounds(from, rollup.Berlin)

	type key = Key
	stats := map[key]map[string]*FlipStat{}
	get := func(k key, faction string) *FlipStat {
		m := stats[k]
		if m == nil {
			m = map[string]*FlipStat{}
			stats[k] = m
		}
		s := m[faction]
		if s == nil {
			s = &FlipStat{}
			m[faction] = s
		}
		return s
	}

	for _, h := range houses {
		// History span: the 7-day window, or since this house was first scanned.
		var first sql.NullInt64
		if err := st.DB.QueryRowContext(ctx, `SELECT MIN(fetched_at) FROM snapshot WHERE house_id=?`, h.ID).Scan(&first); err != nil {
			return nil, err
		}
		start := dayStart
		if first.Valid && first.Int64 > start {
			start = first.Int64
		}
		if h.Tracked {
			// Sales exist only since auctions were tracked here, which can be later than the
			// first snapshot (a house switched from price_only): count from the first day with any.
			var firstSale sql.NullString
			if err := st.DB.QueryRowContext(ctx, `SELECT MIN(day) FROM item_daily WHERE house_id=? AND day>=? AND sold_lo>0`, h.ID, from).Scan(&firstSale); err != nil {
				return nil, err
			}
			if firstSale.Valid {
				if t, _, err := rollup.DayBounds(firstSale.String, rollup.Berlin); err == nil && t > start {
					start = t
				}
			}
		}
		span := math.Max(float64(now.Unix()-start)/86400, 1.0/24)
		in.SpanDays = math.Min(in.SpanDays, span)

		rows, err := st.DB.QueryContext(ctx, `SELECT item_id, variant, day, COALESCE(fair_unit,0), COALESCE(price_cv,0),
			sold_lo, COALESCE(sales,0), COALESCE(largest_sale,0) FROM item_daily WHERE house_id=? AND day>=? ORDER BY day`, h.ID, from)
		if err != nil {
			return nil, err
		}
		type sums struct{ sold, sales, largest int64 }
		agg := map[key]*sums{}
		for rows.Next() {
			var k key
			var day string
			var fair, sold, sales, largest int64
			var cv float64
			if err := rows.Scan(&k.Item, &k.Variant, &day, &fair, &cv, &sold, &sales, &largest); err != nil {
				rows.Close()
				return nil, err
			}
			s := get(k, h.Faction)
			if fair > 0 {
				s.Fair = fair
			}
			// Any volatile day in the window counts: today's partial row must not clear it.
			s.Volatile = s.Volatile || cv > 0.5
			a := agg[k]
			if a == nil {
				a = &sums{}
				agg[k] = a
			}
			a.sold += sold
			a.sales += sales
			a.largest = max(a.largest, largest)
		}
		rows.Close()
		for k, a := range agg {
			s := get(k, h.Faction)
			s.Sales = a.sales
			s.SoldPerDay = float64(a.sold) / span
			s.RobustPerDay = float64(a.sold-a.largest) / span
			s.SalesPerDay = float64(a.sales) / span
		}

		// Current lowest price and supply from the newest snapshots.
		rows, err = st.DB.QueryContext(ctx, `SELECT item_id, variant, COALESCE(min_unit,0), listed_qty, COALESCE(p20_unit,0) FROM item_snapshot
			WHERE snapshot_id IN (SELECT MAX(id) FROM snapshot WHERE house_id=? GROUP BY kind)`, h.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k key
			var mn, listed, p20 int64
			if err := rows.Scan(&k.Item, &k.Variant, &mn, &listed, &p20); err != nil {
				rows.Close()
				return nil, err
			}
			s := get(k, h.Faction)
			s.Min, s.Listed = mn, listed
			if s.Fair == 0 {
				s.Fair = p20 // no daily row yet
			}
		}
		rows.Close()
	}

	// Keep items with sales evidence somewhere (every route needs 5+ sales) where some tradeable
	// house sells below what the item could fetch.
	var keep []key
	for k, m := range stats {
		best, evidence := int64(0), false
		for _, s := range m {
			best = max(best, s.Fair, s.Min)
			evidence = evidence || s.Sales >= 5
		}
		if !evidence {
			continue
		}
		for _, h := range houses {
			if s := m[h.Faction]; !h.ViewOnly && s != nil && s.Min > 0 && float64(s.Min) <= 0.85*float64(best) {
				keep = append(keep, k)
				break
			}
		}
	}
	want := map[key]bool{}
	for _, k := range keep {
		want[k] = true
	}

	// Order books for kept items on tradeable houses.
	for _, h := range houses {
		if h.ViewOnly || !h.Tracked {
			continue
		}
		rows, err := st.DB.QueryContext(ctx, `SELECT item_id, variant, unit_price, SUM(qty_last) FROM auction
			WHERE house_id=? AND closed_at IS NULL AND unit_price > 0 GROUP BY item_id, variant, unit_price ORDER BY item_id, variant, unit_price`, h.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k key
			var l FlipLevel
			if err := rows.Scan(&k.Item, &k.Variant, &l.P, &l.Q); err != nil {
				rows.Close()
				return nil, err
			}
			if !want[k] {
				continue
			}
			s := stats[k][h.Faction]
			switch {
			case s == nil:
			case len(s.Book) < bookLevels:
				s.Book = append(s.Book, l)
			case s.Beyond == 0:
				s.Beyond = l.P // levels arrive cheapest first: this is the next one up
			}
		}
		rows.Close()
	}

	names, err := LoadNames(ctx, st.DB)
	if err != nil {
		return nil, err
	}
	sellPrice := map[int64]int64{}
	rows, err := st.DB.QueryContext(ctx, `SELECT id, COALESCE(sell_price,0) FROM item`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, sp int64
		if err := rows.Scan(&id, &sp); err != nil {
			rows.Close()
			return nil, err
		}
		sellPrice[id] = sp
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(keep, func(i, j int) bool {
		return keep[i].Item < keep[j].Item || (keep[i].Item == keep[j].Item && keep[i].Variant < keep[j].Variant)
	})
	for _, k := range keep {
		in.Items = append(in.Items, FlipItem{Item: k.Item, Variant: k.Variant, Name: ItemLabel(names, k),
			Deposit: int64(math.Round(float64(sellPrice[k.Item]) * depositRate)), Houses: stats[k]})
	}
	return in, nil
}

// FlipOptions are the player's risk settings.
type FlipOptions struct {
	Gold      int64   // copper the player has; default 40g
	MaxShare  float64 // most of it one flip may use, default 0.5
	MinROI    float64 // default 0.25
	MaxDays   float64 // must sell within this many days at the floor rate, default 1
	MinProfit int64   // default 0.5% of gold, between 10s and 1g
	Limit     int
	// Horde → Blackwater: the Alliance must buy it fast and pay more at home.
	AllyMinSalesPerDay float64 // default 3
	AllyEdge           float64 // your Blackwater price must be this much under Alliance's, default 0.05
	AllyCapture        float64 // share of Alliance demand assumed to come to Blackwater, default 0.1
}

func (o FlipOptions) withDefaults() FlipOptions {
	if o.Gold <= 0 {
		o.Gold = 40 * 10000
	}
	if o.MaxShare <= 0 {
		o.MaxShare = 0.5
	}
	if o.MinROI <= 0 {
		o.MinROI = 0.25
	}
	if o.MaxDays <= 0 {
		o.MaxDays = 1
	}
	if o.MinProfit <= 0 {
		// Scales gently: a sure 1g is worth showing whatever the budget.
		o.MinProfit = min(10000, max(1000, o.Gold/200))
	}
	if o.AllyMinSalesPerDay <= 0 {
		o.AllyMinSalesPerDay = 3
	}
	if o.AllyEdge <= 0 {
		o.AllyEdge = 0.05
	}
	if o.AllyCapture <= 0 {
		o.AllyCapture = 0.1
	}
	return o
}

// Flip is one recommended trade.
type Flip struct {
	ItemID        int64   `json:"item_id"`
	Variant       string  `json:"variant,omitempty"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind" jsonschema:"relist (buy underpriced and relist in the same house), cross (between houses) or alliance (Horde to Blackwater, sold to Alliance players)"`
	BuyHouse      string  `json:"buy_house"`
	SellHouse     string  `json:"sell_house"`
	BuyUnits      int64   `json:"buy_units"`
	BuyUpTo       int64   `json:"buy_up_to_price"`
	BuyCost       int64   `json:"buy_cost"`
	SellAt        int64   `json:"sell_at"`
	Profit        int64   `json:"expected_profit"` // after cut and deposit, if all sell at sell_at
	ROI           float64 `json:"roi"`
	ShareOfGold   float64 `json:"share_of_gold"`
	DaysToSell    float64 `json:"days_to_sell"` // at the floor sales rate
	SalesPerDay   float64 `json:"sales_per_day" jsonschema:"separate sales per day in the market that absorbs the resale"`
	AlliancePrice int64   `json:"alliance_price,omitempty"`
	Why           string  `json:"why"`
}

// FlipFunnel counts why items were rejected, per route.
type FlipFunnel struct {
	Candidates   int `json:"candidates"`
	NoBuyListing int `json:"not_listed_to_buy"`
	SlowSales    int `json:"sells_too_slowly"`  // resale market too slow or too thin
	NoGap        int `json:"no_profitable_gap"` // nothing cheap enough for the margin
	OverBudget   int `json:"needs_more_gold"`   // gap exists but not within this budget/profit floor
	Passed       int `json:"passed"`
}

// PlanFlips applies the rules and the player's gold to precomputed inputs. It touches no database.
// It also returns the gold it planned for (the default when o.Gold is unset).
func PlanFlips(in *FlipInputs, o FlipOptions) ([]Flip, map[string]*FlipFunnel, int64) {
	o = o.withDefaults()
	maxSpend := int64(float64(o.Gold) * o.MaxShare)
	funnel := map[string]*FlipFunnel{}
	var ally *FlipHouse
	for i := range in.Houses {
		if in.Houses[i].Faction == "alliance" {
			ally = &in.Houses[i]
		}
	}
	var flips []Flip
	for _, buy := range in.Houses {
		if buy.ViewOnly {
			continue
		}
		for _, sell := range in.Houses {
			if sell.ViewOnly {
				continue
			}
			kind := "cross"
			switch {
			case buy.ID == sell.ID:
				kind = "relist"
			case sell.Faction == "blackwater":
				kind = "alliance"
			}
			route := buy.Faction + " → " + sell.Faction
			fn := &FlipFunnel{}
			funnel[route] = fn
			for _, it := range in.Items {
				b, s := it.Houses[buy.Faction], it.Houses[sell.Faction]
				if s == nil {
					continue
				}
				fn.Candidates++
				if b == nil || len(b.Book) == 0 {
					fn.NoBuyListing++
					continue
				}
				f, reason := planOne(it, b, s, buy, sell, kind, ally, maxSpend, o)
				switch reason {
				case "":
					f.ShareOfGold = float64(f.BuyCost) / float64(o.Gold)
					fn.Passed++
					flips = append(flips, f)
				case "slow":
					fn.SlowSales++
				case "gap":
					fn.NoGap++
				case "budget":
					fn.OverBudget++
				}
			}
		}
	}
	sort.Slice(flips, func(i, j int) bool { return flips[i].Profit > flips[j].Profit })
	if o.Limit > 0 && len(flips) > o.Limit {
		flips = flips[:o.Limit]
	}
	return flips, funnel, o.Gold
}

// planOne decides one item on one route. reason is "" (a flip), "slow", "gap" or "budget".
func planOne(it FlipItem, b, s *FlipStat, buy, sell FlipHouse, kind string, ally *FlipHouse, maxSpend int64, o FlipOptions) (Flip, string) {
	var rate float64 // units/day the resale market absorbs (floor)
	var salesDay float64
	var ceiling int64 // highest sensible resale price
	var allyPrice int64
	var why string
	switch kind {
	case "alliance":
		// Rule: the Alliance must buy it fast, and pay more at home than our Blackwater price.
		if ally == nil || !ally.Tracked {
			return Flip{}, "slow"
		}
		a := it.Houses["alliance"]
		if a == nil || a.Sales < 5 || a.SalesPerDay < o.AllyMinSalesPerDay || s.Volatile {
			return Flip{}, "slow"
		}
		allyPrice = a.Min
		if a.Fair > 0 && (allyPrice == 0 || a.Fair < allyPrice) {
			allyPrice = a.Fair
		}
		if allyPrice == 0 {
			return Flip{}, "gap"
		}
		ceiling = int64(float64(allyPrice) * (1 - o.AllyEdge))
		rate = math.Max(s.RobustPerDay, o.AllyCapture*a.RobustPerDay)
		salesDay = a.SalesPerDay
		why = fmt.Sprintf("Alliance pays %s at home and buys %.0f times a day; yours on Blackwater is cheaper", Gold(allyPrice), a.SalesPerDay)
	default:
		if s.Sales < 5 || s.SalesPerDay < 2 || s.Volatile || s.Fair == 0 {
			return Flip{}, "slow"
		}
		ceiling = s.Fair // never bet on the price going above market value
		rate = s.RobustPerDay
		salesDay = s.SalesPerDay
	}
	capUnits := int64(rate * o.MaxDays)
	if capUnits < 1 {
		return Flip{}, "slow"
	}

	// Resale price: the ceiling, or just under the cheapest competing listing. In the same house
	// the competition is what's left after our own purchases, so iterate until stable.
	sellAt := ceiling
	if kind != "relist" && s.Min > 0 && s.Min-1 < sellAt {
		sellAt = s.Min - 1
	}
	var units, cost, upTo, left int64
	fill := func() {
		net := float64(sellAt)*(1-sell.Cut) - float64(it.Deposit)
		maxBuy := int64(net / (1 + o.MinROI))
		units, cost, upTo, left = 0, 0, 0, 0
		for _, l := range b.Book {
			if l.P > maxBuy || units >= capUnits {
				break
			}
			take := min(l.Q, capUnits-units, (maxSpend-cost)/l.P)
			if take <= 0 {
				break
			}
			units += take
			cost += take * l.P
			upTo = l.P
			left = l.Q - take
		}
	}
	fill()
	for iter := 0; iter < 4 && kind == "relist" && units > 0; iter++ {
		next := int64(0)
		if left > 0 {
			next = upTo
		} else {
			var seen int64
			for _, l := range b.Book {
				seen += l.Q
				if seen > units {
					next = l.P
					break
				}
			}
			if next == 0 {
				next = b.Beyond // bought the whole cached book: the competition is past it
			}
		}
		want := ceiling
		if next > 0 && next-1 < want {
			want = next - 1
		}
		if want == sellAt {
			break
		}
		sellAt = want
		fill() // the purchase must match the price it's resold at
	}
	if units == 0 {
		// Is there a gap at all, ignoring the budget?
		net := float64(sellAt)*(1-sell.Cut) - float64(it.Deposit)
		if float64(b.Book[0].P) <= net/(1+o.MinROI) {
			return Flip{}, "budget"
		}
		return Flip{}, "gap"
	}
	net := float64(sellAt)*(1-sell.Cut) - float64(it.Deposit)
	profit := int64(math.Round(net*float64(units))) - cost
	roi := float64(profit) / float64(cost)
	if roi < o.MinROI {
		return Flip{}, "gap"
	}
	if profit < o.MinProfit {
		return Flip{}, "budget"
	}
	if why == "" {
		if kind == "relist" {
			why = fmt.Sprintf("listed well under the next price (%s); sells %.0f times a day", Gold(sellAt+1), salesDay)
		} else {
			why = fmt.Sprintf("cheaper on %s than it sells for on %s; sells %.0f times a day there", title(buy.Faction), title(sell.Faction), salesDay)
		}
	}
	return Flip{ItemID: it.Item, Variant: it.Variant, Name: it.Name, Kind: kind, BuyHouse: buy.Faction, SellHouse: sell.Faction,
		BuyUnits: units, BuyUpTo: upTo, BuyCost: cost, SellAt: sellAt, Profit: profit, ROI: roi,
		DaysToSell: float64(units) / rate, SalesPerDay: salesDay, AlliancePrice: allyPrice, Why: why}, ""
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

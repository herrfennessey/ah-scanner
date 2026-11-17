package report

import (
	"context"
	"database/sql"
	"encoding/json"
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
	ID      int64   `json:"id"`
	Faction string  `json:"faction"`
	Cut     float64 `json:"cut"`
	// DepositMult scales FlipItem.Deposit for listings here (Blackwater 5×); 0 means 1.
	DepositMult float64 `json:"deposit_mult,omitempty"`
	ViewOnly    bool    `json:"view_only"` // can't trade here (Alliance from a Horde character)
	Tracked     bool    `json:"tracked"`   // auctions tracked, so sales are known
}

// FlipLevel is one price level of an order book.
type FlipLevel struct {
	P int64 `json:"p"` // unit price
	Q int64 `json:"q"` // units
}

// SaleLevel is one price step of recent purchases.
type SaleLevel struct {
	P int64 `json:"p"` // unit price, rounded down to a 3% step
	Q int64 `json:"q"` // units
	N int64 `json:"n"` // separate sales
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

	// Evidence (tracked houses, see evidence.go): set when the fields below were measured.
	Evidence bool        `json:"evidence,omitempty"`
	Sold12   []SaleLevel `json:"sold12,omitempty"`   // certain sales in the last 12 h, in 3% price steps
	Sold48   []SaleLevel `json:"sold48,omitempty"`   // the same over 48 h
	Clearing int64       `json:"clearing,omitempty"` // median price of the sales in the last 24 h (3+ sales)
	Trend    float64     `json:"trend,omitempty"`    // daily change of the median asking price over 24 h
	Front    *FrontStat  `json:"front,omitempty"`    // how listings that were cheapest when posted sold
}

type FlipItem struct {
	Item    int64                `json:"item"`
	Variant string               `json:"variant,omitempty"`
	Name    string               `json:"name"`
	Icon    string               `json:"icon,omitempty"`
	Quality int                  `json:"quality,omitempty"`
	Deposit int64                `json:"deposit"` // estimated faction-house deposit per unit
	Houses  map[string]*FlipStat `json:"houses"`  // by faction
}

// FlipInputs is everything PlanFlips needs; stored after each publish.
type FlipInputs struct {
	ComputedAt int64       `json:"computed_at"`
	SpanDays   float64     `json:"span_days"` // history behind the sales rates
	Houses     []FlipHouse `json:"houses"`
	Items      []FlipItem  `json:"items"`
	// Front is each tracked house's sell rate for cheapest-when-posted listings, by faction:
	// the prior for items with few of their own.
	Front map[string]*FrontStat `json:"front,omitempty"`
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
// CachedHouses reads the houses from the stored flip inputs: the collector resolved them, so a
// copied database brings them along.
func CachedHouses(ctx context.Context, st *store.Store) ([]FlipHouse, error) {
	_, b, err := st.LoadFlipCache(ctx)
	if err != nil || b == nil {
		return nil, fmt.Errorf("no flip cache to take the houses from (run the collector once): %v", err)
	}
	var in FlipInputs
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	return in.Houses, nil
}

// RebuildFlipCache recomputes the stored flip inputs from the database as of its newest scan,
// for the houses already in the cache: what the collector does after each scan, for a copy of
// the database with no collector running (ahc serve -rebuild).
func RebuildFlipCache(ctx context.Context, st *store.Store, depositRate float64) (*FlipInputs, error) {
	houses, err := CachedHouses(ctx, st)
	if err != nil {
		return nil, err
	}
	var last sql.NullInt64
	if err := st.DB.QueryRowContext(ctx, `SELECT MAX(fetched_at) FROM snapshot`).Scan(&last); err != nil {
		return nil, err
	} else if !last.Valid {
		return nil, fmt.Errorf("no scans")
	}
	in, err := BuildFlipInputs(ctx, st, houses, depositRate, time.Unix(last.Int64, 0))
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return in, st.SaveFlipCache(ctx, in.ComputedAt, b)
}

func BuildFlipInputs(ctx context.Context, st *store.Store, houses []FlipHouse, depositRate float64, now time.Time) (*FlipInputs, error) {
	return buildFlipInputs(ctx, st, houses, depositRate, now, false)
}

// BuildFlipInputsAt rebuilds the inputs as they were at a past time (for the backtest): order
// books from when auctions appeared, sold down and closed, and today's sales from the raw
// events up to then. Today's market value and volatility come from the days before, since
// today's daily row already holds what happened after.
func BuildFlipInputsAt(ctx context.Context, st *store.Store, houses []FlipHouse, depositRate float64, at time.Time) (*FlipInputs, error) {
	return buildFlipInputs(ctx, st, houses, depositRate, at, true)
}

func buildFlipInputs(ctx context.Context, st *store.Store, houses []FlipHouse, depositRate float64, now time.Time, past bool) (*FlipInputs, error) {
	at := now.Unix()
	in := &FlipInputs{ComputedAt: at, Houses: houses, SpanDays: 7}
	from := rollup.DayString(now.AddDate(0, 0, -6), rollup.Berlin)
	today := rollup.DayString(now, rollup.Berlin)
	dayStart, _, _ := rollup.DayBounds(from, rollup.Berlin)
	todayStart, _, _ := rollup.DayBounds(today, rollup.Berlin)

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
		if err := st.DB.QueryRowContext(ctx, `SELECT MIN(fetched_at) FROM snapshot WHERE house_id=? AND fetched_at<=?`, h.ID, at).Scan(&first); err != nil {
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
			if err := st.DB.QueryRowContext(ctx, `SELECT MIN(day) FROM item_daily WHERE house_id=? AND day>=? AND day<=? AND sold_lo>0`, h.ID, from, today).Scan(&firstSale); err != nil {
				return nil, err
			}
			if firstSale.Valid {
				if t, _, err := rollup.DayBounds(firstSale.String, rollup.Berlin); err == nil && t > start {
					start = t
				}
			}
		}
		span := math.Max(float64(at-start)/86400, 1.0/24)
		in.SpanDays = math.Min(in.SpanDays, span)

		rows, err := st.DB.QueryContext(ctx, `SELECT item_id, variant, day, COALESCE(fair_unit,0), COALESCE(price_cv,0),
			sold_lo, COALESCE(sales,0), COALESCE(largest_sale,0) FROM item_daily WHERE house_id=? AND day>=? AND day<=? ORDER BY day`, h.ID, from, today)
		if err != nil {
			return nil, err
		}
		type sums struct{ sold, sales, largest int64 }
		agg := map[key]*sums{}
		add := func(k key, sold, sales, largest int64) {
			a := agg[k]
			if a == nil {
				a = &sums{}
				agg[k] = a
			}
			a.sold += sold
			a.sales += sales
			a.largest = max(a.largest, largest)
		}
		for rows.Next() {
			var k key
			var day string
			var fair, sold, sales, largest int64
			var cv float64
			if err := rows.Scan(&k.Item, &k.Variant, &day, &fair, &cv, &sold, &sales, &largest); err != nil {
				rows.Close()
				return nil, err
			}
			if past && day == today {
				continue // holds the rest of the day: counted from the raw events below
			}
			s := get(k, h.Faction)
			if fair > 0 {
				s.Fair = fair
			}
			// Any volatile day in the window counts: today's partial row must not clear it.
			s.Volatile = s.Volatile || cv > 0.5
			add(k, sold, sales, largest)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if past {
			// Today's certain sales up to now, as the rollup counts them.
			rows, err := st.DB.QueryContext(ctx, `SELECT item_id, variant, SUM(units), COUNT(*), MAX(units) FROM (
				SELECT item_id, variant, units FROM sale_event WHERE house_id=? AND ts>=? AND ts<=? AND outcome IN ('sold','partial')
				UNION ALL
				SELECT item_id, variant, qty_last FROM auction WHERE house_id=? AND closed_at>=? AND closed_at<=? AND outcome='sold'
			) GROUP BY item_id, variant`, h.ID, todayStart, at, h.ID, todayStart, at)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var k key
				var sold, sales, largest int64
				if err := rows.Scan(&k.Item, &k.Variant, &sold, &sales, &largest); err != nil {
					rows.Close()
					return nil, err
				}
				get(k, h.Faction)
				add(k, sold, sales, largest)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
		for k, a := range agg {
			s := get(k, h.Faction)
			s.Sales = a.sales
			s.SoldPerDay = float64(a.sold) / span
			s.RobustPerDay = float64(a.sold-a.largest) / span
			s.SalesPerDay = float64(a.sales) / span
		}

		// Current lowest price and supply from the newest snapshots.
		rows, err = st.DB.QueryContext(ctx, `SELECT item_id, variant, COALESCE(min_unit,0), listed_qty, COALESCE(p20_unit,0) FROM item_snapshot
			WHERE snapshot_id IN (SELECT MAX(id) FROM snapshot WHERE house_id=? AND fetched_at<=? GROUP BY kind)`, h.ID, at)
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
		rows, err := bookRows(ctx, st.DB, h.ID, keep, past, at)
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

	in.Front = map[string]*FrontStat{}
	for _, h := range houses {
		if !h.Tracked || len(keep) == 0 {
			continue
		}
		f, err := addEvidence(ctx, st.DB, h.ID, h.Faction, keep, at, stats)
		if err != nil {
			return nil, err
		}
		in.Front[h.Faction] = f
	}

	names, err := LoadNames(ctx, st.DB)
	if err != nil {
		return nil, err
	}
	sellPrice := map[int64]int64{}
	type look struct {
		icon    string
		quality int
	}
	looks := map[int64]look{}
	rows, err := st.DB.QueryContext(ctx, `SELECT id, COALESCE(sell_price,0), COALESCE(icon,''), COALESCE(quality,1) FROM item`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, sp int64
		var l look
		if err := rows.Scan(&id, &sp, &l.icon, &l.quality); err != nil {
			rows.Close()
			return nil, err
		}
		sellPrice[id] = sp
		looks[id] = l
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
			Icon: looks[k.Item].icon, Quality: looks[k.Item].quality,
			Deposit: int64(math.Round(float64(sellPrice[k.Item]) * depositRate)), Houses: stats[k]})
	}
	return in, nil
}

// bookRows reads the open listings of a house by item and price, cheapest first. In the past
// (the backtest) it rebuilds them at t for the wanted items: listings seen by t and not closed
// by then, each with the units it still had (its last quantity plus partial sales after t).
func bookRows(ctx context.Context, db *sql.DB, house int64, want []Key, past bool, t int64) (*sql.Rows, error) {
	if !past {
		return db.QueryContext(ctx, `SELECT item_id, variant, unit_price, SUM(qty_last) FROM auction
			WHERE house_id=? AND closed_at IS NULL AND unit_price > 0 GROUP BY item_id, variant, unit_price ORDER BY item_id, variant, unit_price`, house)
	}
	return db.QueryContext(ctx, `SELECT a.item_id, a.variant, a.unit_price, SUM(a.qty_last + COALESCE(p.u, 0)) FROM auction a
		LEFT JOIN (SELECT kind, auction_id, SUM(units) u FROM sale_event WHERE house_id=? AND ts>? GROUP BY kind, auction_id) p
			ON p.kind=a.kind AND p.auction_id=a.auction_id
		WHERE a.house_id=? AND a.item_id IN (SELECT value FROM json_each(?)) AND a.first_seen<=? AND (a.closed_at IS NULL OR a.closed_at>?) AND a.unit_price>0
		GROUP BY a.item_id, a.variant, a.unit_price ORDER BY a.item_id, a.variant, a.unit_price`,
		house, t, house, idList(want), t, t)
}

// Style is how the player trades: how often they can come back to the auction house. It
// decides which plays they're offered, and each style can do everything the less attentive
// ones can: active ⊇ casual ⊇ away.
type Style string

const (
	StyleActive Style = "active" // checks about hourly
	StyleCasual Style = "casual" // checks 4-5 times a day
	StyleAway   Style = "away"   // posts long listings and comes back later
)

// Play is what the player does with a resale listing once it's posted.
type Play string

const (
	// PlayHold: post a 24h listing at a price buyers pay and leave it, even when undercut. On
	// the commodity AH an undercut listing still sells once the cheaper ones are bought.
	PlayHold Play = "hold"
	// PlayCheckIn: come back every few hours and relist only when the queue cheaper than yours
	// is longer than buyers will take before the listing runs out.
	PlayCheckIn Play = "check_in"
	// PlayCamp: check hourly and relist whenever undercut.
	PlayCamp Play = "camp"
)

// playProfile is what a play means in numbers.
type playProfile struct {
	CheckHours  float64 // between visits: one listing round (a hold has one round, the listing)
	ListHours   float64 // listing duration posted
	WindowHours float64 // the stock should be gone by then; what's left is valued at the going price
	MinSales    float64 // separate sales per day in the market that absorbs the resale
	// Volatile: only a player who's there to react can trade items whose price swings or slides.
	Volatile bool
	// Relists: whether the play ever relists (a hold doesn't).
	Relists bool
}

var playProfiles = map[Play]playProfile{
	PlayHold:    {CheckHours: 24, ListHours: 24, WindowHours: 24, MinSales: 4},
	PlayCheckIn: {CheckHours: 5, ListHours: 24, WindowHours: 24, MinSales: 4, Relists: true},
	PlayCamp:    {CheckHours: 1, ListHours: 8, WindowHours: 12, MinSales: 6, Volatile: true, Relists: true},
}

// rounds is how many listing rounds fit in the window: visits at which unsold stock may be relisted.
func (p playProfile) rounds() int { return max(1, int(math.Round(p.WindowHours/p.CheckHours))) }

// styleProfile is what a style means in numbers.
type styleProfile struct {
	Plays    []Play  // offered, least attentive first
	MaxShare float64 // most of the gold one flip may use
	MinROI   float64
	// SellBlackwater allows routes that resell on Blackwater: 15% cut, 5x deposit, a sixth of
	// Horde's buyers and faster undercuts, so not for a player who's away.
	SellBlackwater bool
}

var styleProfiles = map[Style]styleProfile{
	StyleActive: {Plays: []Play{PlayHold, PlayCheckIn, PlayCamp}, MaxShare: 0.5, MinROI: 0.15, SellBlackwater: true},
	StyleCasual: {Plays: []Play{PlayHold, PlayCheckIn}, MaxShare: 0.4, MinROI: 0.2, SellBlackwater: true},
	StyleAway:   {Plays: []Play{PlayHold}, MaxShare: 0.3, MinROI: 0.25},
}

// ParseStyle accepts active, casual or away (default casual). Older names map to the nearest
// style: regular to casual, and the risk levels high, medium and low to active, casual and away.
func ParseStyle(s string) Style {
	switch s {
	case "high":
		return StyleActive
	case "medium", "regular":
		return StyleCasual
	case "low":
		return StyleAway
	}
	if _, ok := styleProfiles[Style(s)]; ok {
		return Style(s)
	}
	return StyleCasual
}

// competeBand: listings up to this much above our price compete with us for the same buyers,
// since sellers undercut each other.
const competeBand = 1.10

// FlipOptions are the player's gold and trading style. Zero fields come from the style (and,
// for MinSales, from each play).
type FlipOptions struct {
	Gold      int64 // copper the player has; default 40g
	Style     Style // default casual
	MaxShare  float64
	MinROI    float64
	MinSales  float64 // separate sales per day
	MinProfit int64   // default 0.25% of gold, between 10s and 50s
	Volatile  bool    // allow items whose price swings, whatever the play
	// SellBlackwater allows reselling on Blackwater (from the style).
	SellBlackwater bool
	Limit          int
	// Horde → Blackwater: the Alliance must buy it fast and pay more at home.
	AllyEdge    float64 // your Blackwater price must be this much under Alliance's, default 0.05
	AllyCapture float64 // share of Alliance demand assumed to come to Blackwater, default 0.1

	plays []Play
}

func (o FlipOptions) withDefaults() FlipOptions {
	if o.Gold <= 0 {
		o.Gold = 40 * 10000
	}
	o.Style = ParseStyle(string(o.Style))
	p := styleProfiles[o.Style]
	o.plays = p.Plays
	if o.MaxShare <= 0 {
		o.MaxShare = p.MaxShare
	}
	if o.MinROI <= 0 {
		o.MinROI = p.MinROI
	}
	o.SellBlackwater = o.SellBlackwater || p.SellBlackwater
	if o.MinProfit <= 0 {
		// Scales gently: a quick, sure 50s is worth showing whatever the budget.
		o.MinProfit = min(5000, max(1000, o.Gold/400))
	}
	if o.AllyEdge <= 0 {
		o.AllyEdge = 0.05
	}
	if o.AllyCapture <= 0 {
		o.AllyCapture = 0.1
	}
	return o
}

// playOptions are the gates for one play.
type playOptions struct {
	FlipOptions
	play     Play
	p        playProfile
	minSales float64
	volatile bool
}

func (o FlipOptions) forPlay(pl Play) playOptions {
	p := playProfiles[pl]
	po := playOptions{FlipOptions: o, play: pl, p: p, minSales: o.MinSales, volatile: o.Volatile || p.Volatile}
	if po.minSales <= 0 {
		po.minSales = p.MinSales
	}
	return po
}

// Flip is one recommended trade.
type Flip struct {
	ItemID    int64  `json:"item_id"`
	Variant   string `json:"variant,omitempty"`
	Name      string `json:"name"`
	Icon      string `json:"icon,omitempty"`
	Quality   int    `json:"quality,omitempty"`
	Kind      string `json:"kind" jsonschema:"relist (buy underpriced and relist in the same house), cross (between houses) or alliance (Horde to Blackwater, sold to Alliance players)"`
	BuyHouse  string `json:"buy_house"`
	SellHouse string `json:"sell_house"`
	BuyUnits  int64  `json:"buy_units"`
	BuyUpTo   int64  `json:"buy_up_to_price"`
	BuyCost   int64  `json:"buy_cost"`
	SellAt    int64  `json:"sell_at"`
	// Play is how to handle the listing: hold, check_in or camp (the best the style allows).
	Play       Play `json:"play" jsonschema:"hold (post 24h and leave it, even when undercut), check_in (come back every check_hours and relist only when more than relist_queue units are cheaper than yours) or camp (check hourly and relist whenever undercut)"`
	ListHours  int  `json:"list_hours" jsonschema:"listing duration to post"`
	CheckHours int  `json:"check_hours,omitempty" jsonschema:"hours between visits for check_in and camp"`
	// RelistQueue: for check_in, relist when more units than this are listed cheaper than yours
	// at your first visit (buyers wouldn't get through them and yours before it runs out).
	RelistQueue int64   `json:"relist_queue,omitempty" jsonschema:"check_in only: at your first visit, relist if more than this many units are listed cheaper than yours (0: whenever undercut)"`
	Demand      float64 `json:"demand_per_day" jsonschema:"units a day buyers take near the floor: what clears the queue ahead of you"`
	// Profit is after the cut and one lost deposit, if all sell at sell_at.
	Profit int64 `json:"profit_if_sold" jsonschema:"profit if every unit sells at sell_at, after the cut and one lost deposit"`
	// Expected weighs the listing rounds of the play: each sells with SellChance; one that
	// doesn't may be undercut (and relisted, for the plays that relist). Stock left at the end
	// of the window is valued at the going price.
	Expected      int64   `json:"expected_profit" jsonschema:"profit weighted by the measured chance each round's listing sells, counting the deposits and price cuts of the relists the play makes, and valuing stock still unsold at the end of the play's window at the going price"`
	SellChance    float64 `json:"sell_chance" jsonschema:"measured share of this item's units, listed as the cheapest, that sold within one round of the play (a hold: its 24h listing; otherwise until your next visit), blended with the house's rate when the item has few such listings"`
	Undercut      float64 `json:"undercut_chance" jsonschema:"measured chance a fresh cheapest listing of this item is undercut within one round"`
	SoldOut       float64 `json:"sold_out_chance" jsonschema:"chance everything sells within the play's window"`
	GoneSoon      float64 `json:"gone_soon" jsonschema:"measured share of listings this far under market value that are gone within an hour of being seen: how fast to act"`
	DepositAtRisk int64   `json:"deposit_at_risk" jsonschema:"deposit paid to list your units, lost if they expire or you cancel to undercut (Blackwater's deposit is 5x a faction house's)"`
	ROI           float64 `json:"roi"`
	ShareOfGold   float64 `json:"share_of_gold"`
	DaysToSell    float64 `json:"days_to_sell" jsonschema:"days until your units and the competing listings near your price have all sold, at the recent rate of purchases at or above your price"`
	Competing     int64   `json:"competing_units" jsonschema:"units other sellers list within 10% of your price"`
	ProfitPerDay  int64   `json:"profit_per_day" jsonschema:"expected profit divided by the expected holding time (at least an hour): breaks ties in the ranking"`
	SalesPerDay   float64 `json:"sales_per_day" jsonschema:"separate sales per day in the market that absorbs the resale"`
	RecentAbove   int64   `json:"recent_sales_above" jsonschema:"separate purchases at or above sell_at in the last 12 hours: proof buyers still pay it"`
	Clearing      int64   `json:"recent_price,omitempty" jsonschema:"median price actually paid over the last 24 hours"`
	Trend         float64 `json:"trend" jsonschema:"daily change of the asking price over the last 24 hours (-0.1 = falling 10% a day)"`
	AlliancePrice int64   `json:"alliance_price,omitempty"`
	Why           string  `json:"why"`
}

// FlipFunnel counts why items were rejected, per route.
type FlipFunnel struct {
	Candidates   int `json:"candidates"`
	NoBuyListing int `json:"not_listed_to_buy"`
	SlowSales    int `json:"sells_too_slowly"`  // resale market too slow or too thin
	Falling      int `json:"price_falling"`     // the price is sliding: by the trend, or resale well under market value
	Unproven     int `json:"unproven"`          // nobody recently paid the resale price
	Deposit      int `json:"deposit_too_high"`  // relist rounds and unsold stock would eat the profit
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
			if sell.ViewOnly || (sell.Faction == "blackwater" && !o.SellBlackwater) {
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
				f, reason := bestPlay(it, b, s, buy, sell, kind, ally, in.Front[sell.Faction], maxSpend, o)
				switch reason {
				case "":
					f.ShareOfGold = float64(f.BuyCost) / float64(o.Gold)
					fn.Passed++
					flips = append(flips, f)
				case "slow":
					fn.SlowSales++
				case "falling":
					fn.Falling++
				case "unproven":
					fn.Unproven++
				case "deposit":
					fn.Deposit++
				case "gap":
					fn.NoGap++
				case "budget":
					fn.OverBudget++
				}
			}
		}
	}
	// Most gold first: expected profit already weighs in the chance it sells, the relists and
	// stock left over. Gold tied up is not the limit, so speed only breaks ties.
	sort.Slice(flips, func(i, j int) bool {
		if flips[i].Expected != flips[j].Expected {
			return flips[i].Expected > flips[j].Expected
		}
		return flips[i].ProfitPerDay > flips[j].ProfitPerDay
	})
	if o.Limit > 0 && len(flips) > o.Limit {
		flips = flips[:o.Limit]
	}
	return flips, funnel, o.Gold
}

// nearSupply counts units listed in book within competeBand of price, skipping the cheapest
// skip units (our own purchases, when buying and relisting in the same house). A truncated
// book whose next level is still in the band falls back to every listed unit.
func nearSupply(s *FlipStat, price, skip int64) int64 {
	limit := int64(float64(price) * competeBand)
	if len(s.Book) == 0 {
		// No book loaded (a price_only house): all we know is the cheapest price and the count.
		if s.Min > 0 && s.Min <= limit {
			return max(0, s.Listed-skip)
		}
		return 0
	}
	var n, total int64
	for _, l := range s.Book {
		total += l.Q
		if l.P > limit {
			return max(0, n-skip)
		}
		n += l.Q
	}
	if s.Beyond > 0 && s.Beyond <= limit && s.Listed > total {
		n += s.Listed - total
	}
	return max(0, n-skip)
}

const (
	// fallingFloor: a resale price under this share of the reference market value means the
	// market is sliding, and the stock could be worth less again by the time it sells.
	fallingFloor = 0.85
	// fallingTrend: the asking price falling faster than this a day is a slide too.
	fallingTrend = -0.10
	// minProof: separate purchases at or above the resale price in the last 12 hours.
	minProof = 2
	// fallbackChance: per-visit sell chance when a house has no measurements at all.
	fallbackChance = 0.3
	// fallbackUndercut: chance a fresh cheapest listing is undercut within a round, when a
	// house has no measurements at all (a busy AH undercuts most listings within hours).
	fallbackUndercut = 0.5
)

// marketCeiling is the most a resale can ask: the market value, but never more than buyers
// actually paid over the last day, and lower again if the price is sliding (projected to the
// middle of the window).
func marketCeiling(s *FlipStat, o playOptions) int64 {
	c := s.Fair
	if s.Clearing > 0 && (c == 0 || s.Clearing < c) {
		c = s.Clearing
	}
	if s.Trend < 0 {
		c = int64(float64(c) * math.Max(0.5, 1+s.Trend*o.p.WindowHours/24/2))
	}
	return c
}

// reasonStage orders the rejection reasons by how far the item got: when no play works, the
// funnel shows the reason of the play that got furthest.
var reasonStage = map[string]int{"slow": 0, "falling": 1, "unproven": 2, "gap": 3, "deposit": 4, "budget": 5}

// bestPlay plans the item with every play the style allows and keeps the one expected to make
// the most (then the quickest): an attentive player can always do what a less attentive one
// would, so they're never offered less.
func bestPlay(it FlipItem, b, s *FlipStat, buy, sell FlipHouse, kind string, ally *FlipHouse, houseFront *FrontStat, maxSpend int64, o FlipOptions) (Flip, string) {
	var best Flip
	found, why := false, ""
	for _, pl := range o.plays {
		f, reason := planOne(it, b, s, buy, sell, kind, ally, houseFront, maxSpend, o.forPlay(pl))
		if reason != "" {
			if !found && (why == "" || reasonStage[reason] > reasonStage[why]) {
				why = reason
			}
			continue
		}
		if !found || f.Expected > best.Expected || (f.Expected == best.Expected && f.ProfitPerDay > best.ProfitPerDay) {
			best = f
		}
		found = true
	}
	if found {
		return best, ""
	}
	return Flip{}, why
}

// planOne decides one item on one route for one play. reason is "" (a flip), "slow", "gap",
// "budget", "falling", "unproven" or "deposit".
func planOne(it FlipItem, b, s *FlipStat, buy, sell FlipHouse, kind string, ally *FlipHouse, houseFront *FrontStat, maxSpend int64, o playOptions) (Flip, string) {
	var rate float64 // units/day the resale market absorbs (floor)
	var salesDay float64
	var ceiling int64 // highest sensible resale price
	var allyPrice int64
	var refFair int64 // market value the resale is judged against for a slide
	var why string
	window := o.p.WindowHours / 24
	switch kind {
	case "alliance":
		// Rule: the Alliance must buy it fast, and pay more at home than our Blackwater price.
		if ally == nil || !ally.Tracked {
			return Flip{}, "slow"
		}
		a := it.Houses["alliance"]
		if a == nil || a.Sales < 5 || a.SalesPerDay < max(3, o.minSales) || (s.Volatile && !o.volatile) {
			return Flip{}, "slow"
		}
		if !o.volatile && a.Trend < fallingTrend {
			return Flip{}, "falling"
		}
		allyPrice = a.Min
		if c := marketCeiling(a, o); c > 0 && (allyPrice == 0 || c < allyPrice) {
			allyPrice = c
		}
		if allyPrice == 0 {
			return Flip{}, "gap"
		}
		ceiling = int64(float64(allyPrice) * (1 - o.AllyEdge))
		refFair = a.Fair
		rate = math.Max(s.RobustPerDay, o.AllyCapture*a.RobustPerDay)
		salesDay = a.SalesPerDay
		why = fmt.Sprintf("Alliance pays %s at home and buys %.0f times a day; yours on Blackwater is cheaper", Gold(allyPrice), a.SalesPerDay)
	default:
		if s.Sales < 5 || s.SalesPerDay < o.minSales || (s.Volatile && !o.volatile) || s.Fair == 0 {
			return Flip{}, "slow"
		}
		if !o.volatile && s.Evidence && s.Trend < fallingTrend {
			return Flip{}, "falling"
		}
		ceiling = marketCeiling(s, o) // never bet on the price going above what buyers pay
		refFair = s.Fair
		rate = s.RobustPerDay
		salesDay = s.SalesPerDay
	}
	if sell.Faction == "blackwater" {
		// Every Blackwater buyer has a faction house at home: nobody pays more on Blackwater
		// than the dearer of the two home prices (a stale market value can say otherwise).
		if home := blackwaterHomeCap(it, ally != nil); home > 0 && home-1 < ceiling {
			ceiling = home - 1
		}
	}
	relist := kind == "relist"
	dep := float64(it.Deposit) // deposit per unit on the house we sell on
	if sell.DepositMult > 0 {
		dep *= sell.DepositMult
	}

	// Resale price: the ceiling, or just under the cheapest competing listing. In the same house
	// the competition is what's left after our own purchases, so iterate until stable.
	var sellAt, units, cost, upTo, left, capUnits, clearCap int64
	fill := func() {
		net := float64(sellAt)*(1-sell.Cut) - dep
		// A unit is worth buying only if it keeps the minimum return and its margin also covers
		// one relist deposit, so a few dear units can't sink the whole flip.
		maxBuy := int64(min(net/(1+o.MinROI), net-dep))
		units, cost, upTo, left = 0, 0, 0, 0
		if relist {
			// What we buy here leaves the band and comes back into it at our price, so the
			// whole band must clear, whatever we take: checked once the price settles, since
			// the band at the starting price (the ceiling) can be far wider than at the final one.
			capUnits = clearCap
		} else {
			capUnits = clearCap - nearSupply(s, sellAt, 0)
		}
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
	price := func() {
		sellAt = ceiling
		if kind != "relist" && s.Min > 0 && s.Min-1 < sellAt {
			sellAt = s.Min - 1
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
	}
	// demand is units/day bought at or above p lately (the recent floor of what our listing
	// would have caught), or the window's sales rate when there's no evidence.
	demand := func(p int64) float64 {
		if !s.Evidence || kind == "alliance" {
			return rate
		}
		u, _ := atOrAbove(s.Sold48, p)
		return math.Min(rate, float64(u)/2)
	}
	// Everything near our price must clear within the window: our units and the competition.
	clearCap = int64(rate * window)
	if clearCap < 1 {
		return Flip{}, "slow"
	}
	price()
	if units > 0 && s.Evidence && kind != "alliance" {
		// Size to the purchases that really happen at the resale price, not to all sales.
		if c := int64(demand(sellAt) * window); c < clearCap {
			clearCap = c
			if clearCap < 1 {
				return Flip{}, "unproven"
			}
			price()
		}
	}
	if (units == 0 && capUnits <= 0) || (relist && units > 0 && nearSupply(s, sellAt, 0) > clearCap) {
		return Flip{}, "slow" // too much competing supply near the resale price
	}
	if !o.volatile && refFair > 0 && float64(sellAt) < fallingFloor*float64(refFair) {
		return Flip{}, "falling"
	}
	if units == 0 {
		// Is there a gap at all, ignoring the budget?
		net := float64(sellAt)*(1-sell.Cut) - dep
		p := float64(b.Book[0].P)
		switch {
		case p > net/(1+o.MinROI):
			return Flip{}, "gap"
		case p > net-dep:
			return Flip{}, "deposit" // a margin, but not one that survives being undercut
		}
		return Flip{}, "budget"
	}
	// Proof: buyers paid at least our price lately (for Alliance flips, demand is the
	// Alliance's at home, judged above).
	var proof int64
	if s.Evidence && kind != "alliance" {
		_, proof = atOrAbove(s.Sold12, sellAt)
		if proof < minProof {
			return Flip{}, "unproven"
		}
	}
	// Sold in one auction: the deposit has a 1s minimum per auction (seen in Journalator logs).
	depositAtRisk := max(minDeposit, int64(math.Round(dep*float64(units))))
	gross := int64(math.Round(float64(sellAt) * (1 - sell.Cut) * float64(units)))
	profit := gross - cost - depositAtRisk
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
	skip := int64(0)
	if relist {
		skip = units
	}
	competing := nearSupply(s, sellAt, skip)
	days := float64(units+competing) / math.Max(demand(sellAt), 1e-9)
	// The fallback table is measured over 12 hours: scaled to the round, as if each hour were
	// an equal, independent chance.
	fallback := 1 - math.Pow(1-sellChance(sell.Faction, days), o.p.CheckHours/12)
	chance := frontChance(s, houseFront, o.p.CheckHours, fallback)
	under := 0.0 // a hold never relists, whatever happens
	if o.p.Relists {
		under = undercutChance(s, houseFront, o.p.CheckHours, fallbackUndercut)
	}
	rounds := o.p.rounds()
	// A round that doesn't sell costs a relist when the listing was undercut, or ran out.
	expire := math.Min(1, o.p.CheckHours/o.p.ListHours)
	if !o.p.Relists {
		expire = 0
	}
	// Stock left at the end of the window goes at what buyers lately pay (or what it cost,
	// when that's unknown), after the undercuts it went through, and at least one market move.
	going := upTo
	if s.Clearing > 0 {
		going = s.Clearing
	}
	going = min(going, int64(float64(sellAt)*math.Pow(undercutRatio, math.Max(1, under*float64(rounds)))))
	leftover := int64(math.Round(float64(going) * (1 - sell.Cut) * float64(units)))
	expected, visits, soldOut := expectedProfit(gross, cost, depositAtRisk, chance, under, expire, rounds, leftover)
	// The deposit comes back on a sale, but profit_if_sold is quoted with one lost: the
	// expected profit is never more than that.
	expected = min(expected, profit)
	if expected < o.MinProfit {
		return Flip{}, "deposit" // likely relists, undercuts and leftover stock eat it
	}
	// Gold is tied up until the next visit after it sells, and no sooner than it can sell. A
	// hold's gold comes back by mail as it sells, or not until the listing ends.
	hold := math.Max(visits*o.p.CheckHours/24, days)
	if !o.p.Relists {
		listed := o.p.ListHours / 24
		hold = soldOut*math.Min(listed, math.Max(days, 1.0/24)) + (1-soldOut)*listed
	}
	f := Flip{ItemID: it.Item, Variant: it.Variant, Name: it.Name, Icon: it.Icon, Quality: it.Quality, Kind: kind, BuyHouse: buy.Faction, SellHouse: sell.Faction,
		BuyUnits: units, BuyUpTo: upTo, BuyCost: cost, SellAt: sellAt, Play: o.play, ListHours: int(o.p.ListHours), Profit: profit, ROI: roi,
		Expected: expected, SellChance: chance, Undercut: under, SoldOut: soldOut, GoneSoon: goneSoon(buy.Faction, b),
		DepositAtRisk: depositAtRisk, Demand: rate,
		DaysToSell: days, Competing: competing,
		ProfitPerDay: int64(float64(expected) / math.Max(hold, 1.0/24)),
		SalesPerDay:  salesDay, RecentAbove: proof, Clearing: s.Clearing, Trend: s.Trend,
		AlliancePrice: allyPrice, Why: why}
	if o.p.Relists {
		f.CheckHours = int(o.p.CheckHours)
	}
	if o.play == PlayCheckIn {
		f.RelistQueue = RelistQueue(rate, o.p.ListHours-o.p.CheckHours, units)
	}
	return f, ""
}

// RelistQueue is the check-in rule: with hoursLeft on the listing, relist when more units than
// this are listed cheaper than your unsold ones. Buyers take the cheapest first at perDay units
// a day, so beyond it they wouldn't reach yours before the listing runs out. 0: relist whenever
// undercut.
func RelistQueue(perDay, hoursLeft float64, unsold int64) int64 {
	return max(0, int64(math.Floor(perDay*hoursLeft/24))-unsold)
}

// blackwaterHomeCap is the most a Blackwater buyer would pay: the higher of the Horde and
// Alliance prices, since each buys at home when that's cheaper. 0 means no cap: a faction with
// none listed at home (or an untracked Alliance) can only buy it on Blackwater.
func blackwaterHomeCap(it FlipItem, allianceKnown bool) int64 {
	homes := []string{"horde"}
	if allianceKnown {
		homes = append(homes, "alliance")
	}
	var c int64
	for _, f := range homes {
		st := it.Houses[f]
		if st == nil || st.Min <= 0 {
			return 0
		}
		c = max(c, st.Min)
	}
	return c
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// PlayTiming is a play's visit interval, window and listing duration in hours, its listing
// rounds, and whether it relists.
func PlayTiming(p Play) (checkHours, windowHours, listHours float64, rounds int, relists bool) {
	x := playProfiles[p]
	return x.CheckHours, x.WindowHours, x.ListHours, x.rounds(), x.Relists
}

// StylePlays lists the plays a style is offered.
func StylePlays(s Style) []Play { return styleProfiles[ParseStyle(string(s))].Plays }

// Styles lists the trading styles, most attentive first.
var Styles = []Style{StyleActive, StyleCasual, StyleAway}

// Plays lists the plays, least attentive first.
var Plays = []Play{PlayHold, PlayCheckIn, PlayCamp}

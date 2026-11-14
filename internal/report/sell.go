package report

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/stats"
	"github.com/herrfennessey/ah-scanner/internal/store"
	"github.com/herrfennessey/ah-scanner/internal/tracker"
)

// SellOptions configures the sell analysis: when to post an item and at what price.
type SellOptions struct {
	Options
	Item            string
	Variant         string // "" = the item's most-listed variant
	BlockHours      int    // size of time-of-day buckets (Berlin), default 3
	AmbiguousWeight float64
	MinSamples      int // ended auctions a bucket needs before it is ranked
}

// SellBlock is one time bucket (time of day, weekday, or both).
type SellBlock struct {
	Label           string  `json:"label"`
	MedianMin       int64   `json:"median_min"`
	MedianP20       int64   `json:"median_p20"`
	MedianListed    int64   `json:"median_listed"`
	Posted          int     `json:"posted"`
	Ended           int64   `json:"ended_units"`
	Samples         int     `json:"auctions_ended"`
	SellRate        float64 `json:"sell_rate"` // shrunk best estimate; -1 = no ended auctions
	SellLo          float64 `json:"sell_lo"`   // 80% interval, covering inference ambiguity and sample size
	SellHi          float64 `json:"sell_hi"`
	MedianSoldPrice int64   `json:"median_sold_price"`
	MedianSellSecs  int64   `json:"median_sell_secs"`
	Undercut2h      float64 `json:"undercut_2h"` // share undercut within 2 h; -1 = unknown
	SoldPerDay      float64 `json:"sold_per_day"`
	SoldPerDayLo    float64 `json:"sold_per_day_lo"`
	SoldPerDayHi    float64 `json:"sold_per_day_hi"`
	Expected        int64   `json:"expected_net"` // per listing, valid when Ranked
	Ranked          bool    `json:"ranked"`
}

type SellPosition struct {
	Label  string  `json:"label"`
	Posted int64   `json:"posted"`
	Rate   float64 `json:"rate"`
	Lo     float64 `json:"lo"`
	Hi     float64 `json:"hi"`
}

type SellNow struct {
	Block         string `json:"block"`
	Min           int64  `json:"min"`
	Listed        int64  `json:"listed"`
	TypicalMin    int64  `json:"typical_min"`
	TypicalListed int64  `json:"typical_listed"`
}

// SellAnalysis is everything `ahc sell` and the web item page show.
type SellAnalysis struct {
	ItemID       int64          `json:"item_id"`
	Variant      string         `json:"variant"`
	Name         string         `json:"name"`
	Days         int            `json:"days"`
	BlockHours   int            `json:"block_hours"`
	Snapshots    int            `json:"snapshots"`
	Auctions     int            `json:"auctions"`
	EndedUnits   int64          `json:"ended_units"`
	Thin         bool           `json:"thin"`
	Blocks       []SellBlock    `json:"blocks"`
	Weekdays     []SellBlock    `json:"weekdays"`
	Heat         [][]SellBlock  `json:"heat"` // [weekday Mon..Sun][block]
	Positions    []SellPosition `json:"positions"`
	Now          SellNow        `json:"now"`
	Best         string         `json:"best"`
	BestExpected int64          `json:"best_expected"`
	NowExpected  int64          `json:"now_expected"`
	NowRanked    bool           `json:"now_ranked"`
	Deposit      int64          `json:"deposit"`
	Verdict      []string       `json:"verdict"`
}

type snapPoint struct {
	t        int64
	min, p20 int64
	listed   int64
}

type sellAuction struct {
	first, closed int64 // closed 0 = still open
	outcome       string
	price, qty    int64 // qty = quantity when first seen
	qtyLast       int64
	position      string
	undercutAfter int64 // seconds until a cheaper new listing appeared; -1 = never while open
}

// split returns certain-sold and ambiguous units of an ended auction: partial commodity
// buys are certain, the remainder takes the closing outcome.
func (a sellAuction) split() (sold, amb int64) {
	sold = a.qty - a.qtyLast
	switch {
	case tracker.Certain(a.outcome):
		sold += a.qtyLast
	case tracker.Ambiguous(a.outcome):
		amb = a.qtyLast
	}
	return sold, amb
}

type bucket struct {
	label                      string
	mins, p20s, listed         []int64
	posted                     int
	soldUnits, ambUnits, ended int64
	endedN                     int // auctions (not units) that ended: the sample size
	soldPrices, sellSecs       []int64
	undercut2h, undercutKnown  int
	evSoldLo, evAmb            int64
	days                       map[string]bool
}

func newBucket(label string) *bucket { return &bucket{label: label, days: map[string]bool{}} }

func medianI(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// prior is what sparse buckets are shrunk toward: the item's overall sell rates, with
// strength m (pseudo-auctions) estimated by empirical Bayes across the bucket family.
type prior struct{ lo, est, hi, m float64 }

func (b *bucket) rates(w float64) (lo, est, hi float64, ok bool) {
	if b.ended == 0 {
		return 0, 0, 0, false
	}
	e := float64(b.ended)
	return float64(b.soldUnits) / e, (float64(b.soldUnits) + w*float64(b.ambUnits)) / e, float64(b.soldUnits+b.ambUnits) / e, true
}

// familyPrior builds the prior for a family of buckets (time blocks, weekdays or cells).
func familyPrior(set []*bucket, w float64) prior {
	var all bucket
	var rates, ns []float64
	for _, b := range set {
		all.ended += b.ended
		all.soldUnits += b.soldUnits
		all.ambUnits += b.ambUnits
		if _, est, _, ok := b.rates(w); ok {
			rates = append(rates, est)
			ns = append(ns, float64(b.endedN))
		}
	}
	lo, est, hi, ok := all.rates(w)
	if !ok {
		return prior{}
	}
	return prior{lo, est, hi, stats.EBStrength(rates, ns, est)}
}

func (b *bucket) finish(o SellOptions, cut float64, deposit int64, pr prior) SellBlock {
	sb := SellBlock{Label: b.label, MedianMin: medianI(b.mins), MedianP20: medianI(b.p20s), MedianListed: medianI(b.listed),
		Posted: b.posted, Ended: b.ended, Samples: b.endedN, SellRate: -1, SellLo: -1, SellHi: -1, Undercut2h: -1,
		MedianSoldPrice: medianI(b.soldPrices), MedianSellSecs: medianI(b.sellSecs)}
	if lo, est, hi, ok := b.rates(o.AmbiguousWeight); ok {
		n := float64(b.endedN)
		// Shrink toward the item's overall rate so one lucky auction can't make a block look great,
		// then report an 80% interval covering both "did it sell?" and "how many auctions?".
		sb.SellRate = stats.Shrink(est, n, pr.est, pr.m)
		sb.SellLo, sb.SellHi = stats.Interval(stats.Shrink(lo, n, pr.lo, pr.m), stats.Shrink(hi, n, pr.hi, pr.m), n+pr.m, 0.8)
	}
	if b.undercutKnown > 0 {
		sb.Undercut2h = float64(b.undercut2h) / float64(b.undercutKnown)
	}
	if d := float64(len(b.days)); d > 0 {
		sb.SoldPerDayLo = float64(b.evSoldLo) / d
		sb.SoldPerDayHi = float64(b.evSoldLo+b.evAmb) / d
		sb.SoldPerDay = (float64(b.evSoldLo) + o.AmbiguousWeight*float64(b.evAmb)) / d
	}
	if sb.SellRate >= 0 && sb.MedianSoldPrice > 0 && b.endedN >= o.MinSamples {
		p := sb.SellRate
		// The deposit is returned on a sale and lost otherwise.
		sb.Expected = int64(math.Round(p*float64(sb.MedianSoldPrice)*(1-cut) - (1-p)*float64(deposit)))
		sb.Ranked = true
	}
	return sb
}

// AnalyzeSell computes when and at what price to list one item.
func AnalyzeSell(ctx context.Context, st *store.Store, o SellOptions) (*SellAnalysis, error) {
	if o.BlockHours <= 0 || 24%o.BlockHours != 0 {
		o.BlockHours = 3
	}
	if o.MinSamples <= 0 {
		o.MinSamples = 3 // auctions; shrinkage handles the rest
	}
	if o.Days <= 0 {
		o.Days = 14
	}
	id, err := ResolveItem(ctx, st.DB, o.Item)
	if err != nil {
		return nil, err
	}
	names, err := LoadNames(ctx, st.DB)
	if err != nil {
		return nil, err
	}
	variant := o.Variant
	if variant == "" {
		if err := st.DB.QueryRowContext(ctx, `SELECT variant FROM auction WHERE house_id=? AND item_id=? GROUP BY variant ORDER BY count(*) DESC LIMIT 1`,
			o.HouseID, id).Scan(&variant); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
	}
	key := Key{id, variant}
	since := o.Now.AddDate(0, 0, -o.Days).Unix()
	loc := rollup.Berlin

	points, err := loadSnapPoints(ctx, st.DB, o.HouseID, key, since)
	if err != nil {
		return nil, err
	}
	auctions, err := loadSellAuctions(ctx, st.DB, o.HouseID, key, since, o.Now.Unix())
	if err != nil {
		return nil, err
	}
	var sellPrice int64
	_ = st.DB.QueryRowContext(ctx, `SELECT COALESCE(sell_price,0) FROM item WHERE id=?`, id).Scan(&sellPrice)
	deposit := int64(math.Round(float64(sellPrice) * o.DepositRate))

	nb := 24 / o.BlockHours
	blockLabel := func(i int) string { return fmt.Sprintf("%02d–%02d", i*o.BlockHours, (i+1)*o.BlockHours) }
	blocks := make([]*bucket, nb)
	for i := range blocks {
		blocks[i] = newBucket(blockLabel(i))
	}
	wdNames := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	weekdays := make([]*bucket, 7)
	heat := make([][]*bucket, 7)
	for d := range weekdays {
		weekdays[d] = newBucket(wdNames[d])
		heat[d] = make([]*bucket, nb)
		for i := range heat[d] {
			heat[d][i] = newBucket(wdNames[d] + " " + blockLabel(i))
		}
	}
	bucketsAt := func(t int64) []*bucket {
		lt := time.Unix(t, 0).In(loc)
		d, i := (int(lt.Weekday())+6)%7, lt.Hour()/o.BlockHours
		return []*bucket{blocks[i], weekdays[d], heat[d][i]}
	}
	dayOf := func(t int64) string { return time.Unix(t, 0).In(loc).Format("2006-01-02") }

	for _, p := range points {
		for _, b := range bucketsAt(p.t) {
			if p.min > 0 {
				b.mins = append(b.mins, p.min)
			}
			if p.p20 > 0 {
				b.p20s = append(b.p20s, p.p20)
			}
			b.listed = append(b.listed, p.listed)
			b.days[dayOf(p.t)] = true
		}
	}
	var endedUnits int64
	for _, a := range auctions {
		if a.closed > 0 {
			endedUnits += a.qty
		}
		for _, b := range bucketsAt(a.first) {
			b.posted++
			if a.undercutAfter >= 0 || a.closed > 0 || o.Now.Unix()-a.first >= 2*3600 {
				b.undercutKnown++
				if a.undercutAfter >= 0 && a.undercutAfter <= 2*3600 {
					b.undercut2h++
				}
			}
			if a.closed == 0 {
				continue
			}
			sold, amb := a.split()
			b.ended += a.qty
			b.endedN++
			b.soldUnits += sold
			b.ambUnits += amb
			if tracker.Certain(a.outcome) {
				b.soldPrices = append(b.soldPrices, a.price)
				b.sellSecs = append(b.sellSecs, a.closed-a.first)
			}
		}
	}
	if err := addSaleEvents(ctx, st.DB, o.HouseID, key, since, func(t int64, certain int, units int64) {
		for _, b := range bucketsAt(t) {
			switch certain {
			case 1:
				b.evSoldLo += units
			case 0:
				b.evAmb += units
			}
		}
	}); err != nil {
		return nil, err
	}

	a := &SellAnalysis{ItemID: id, Variant: variant, Name: ItemLabel(names, key), Days: o.Days, BlockHours: o.BlockHours,
		Snapshots: len(points), Auctions: len(auctions), EndedUnits: endedUnits, Thin: endedUnits < 20, Deposit: deposit}
	var cells []*bucket
	for _, row := range heat {
		cells = append(cells, row...)
	}
	blockPrior := familyPrior(blocks, o.AmbiguousWeight)
	weekdayPrior := familyPrior(weekdays, o.AmbiguousWeight)
	cellPrior := familyPrior(cells, o.AmbiguousWeight)
	for _, b := range blocks {
		a.Blocks = append(a.Blocks, b.finish(o, o.Cut, deposit, blockPrior))
	}
	for d := range weekdays {
		a.Weekdays = append(a.Weekdays, weekdays[d].finish(o, o.Cut, deposit, weekdayPrior))
		row := make([]SellBlock, nb)
		for i, b := range heat[d] {
			// Cells are sparse; shrinkage keeps them honest, so they rank with a lower bar.
			oc := o
			oc.MinSamples = max(2, o.MinSamples/2)
			row[i] = b.finish(oc, o.Cut, deposit, cellPrior)
		}
		a.Heat = append(a.Heat, row)
	}
	a.Positions = positions(o, auctions)

	nowIdx := time.Unix(o.Now.Unix(), 0).In(loc).Hour() / o.BlockHours
	cur := a.Blocks[nowIdx]
	a.Now = SellNow{Block: cur.Label, TypicalMin: cur.MedianMin, TypicalListed: cur.MedianListed}
	if len(points) > 0 {
		last := points[len(points)-1]
		a.Now.Min, a.Now.Listed = last.min, last.listed
	}
	for _, b := range a.Blocks {
		if b.Ranked && (a.Best == "" || b.Expected > a.BestExpected) {
			a.Best, a.BestExpected = b.Label, b.Expected
		}
	}
	a.NowExpected, a.NowRanked = cur.Expected, cur.Ranked
	a.Verdict = verdictLines(a, o)
	return a, nil
}

func positions(o SellOptions, auctions []sellAuction) []SellPosition {
	order := []string{"cheapest", "≤5% above min", "5–15% above", ">15% above"}
	type agg struct{ posted, sold, amb, ended, endedN int64 }
	m := map[string]*agg{}
	for _, a := range auctions {
		g := m[a.position]
		if g == nil {
			g = &agg{}
			m[a.position] = g
		}
		g.posted++
		if a.closed == 0 {
			continue
		}
		sold, amb := a.split()
		g.ended += a.qty
		g.endedN++
		g.sold += sold
		g.amb += amb
	}
	var out []SellPosition
	for _, p := range order {
		g := m[p]
		if g == nil {
			continue
		}
		sp := SellPosition{Label: p, Posted: g.posted, Rate: -1, Lo: -1, Hi: -1}
		if g.ended > 0 {
			e := float64(g.ended)
			sp.Rate = (float64(g.sold) + o.AmbiguousWeight*float64(g.amb)) / e
			sp.Lo, sp.Hi = stats.Interval(float64(g.sold)/e, float64(g.sold+g.amb)/e, float64(g.endedN), 0.8)
		}
		out = append(out, sp)
	}
	return out
}

func verdictLines(a *SellAnalysis, o SellOptions) []string {
	var lines []string
	n := a.Now
	switch {
	case a.Snapshots == 0:
		return []string{"No snapshots of this item yet. Once the collector has run for a while this fills in."}
	case n.Min > 0 && n.TypicalMin > 0:
		diff := float64(n.Min)/float64(n.TypicalMin) - 1
		line := fmt.Sprintf("Now: lowest %s with %d listed. Usual for %s is %s with %d listed (%+.0f%% price",
			Gold(n.Min), n.Listed, n.Block, Gold(n.TypicalMin), n.TypicalListed, diff*100)
		if n.TypicalListed > 0 {
			line += fmt.Sprintf(", %.1f× supply", float64(n.Listed)/float64(n.TypicalListed))
		}
		lines = append(lines, line+").")
		switch {
		case diff < -0.2 && n.TypicalListed > 0 && float64(n.Listed) > 1.5*float64(n.TypicalListed):
			lines = append(lines, "More is listed than usual and that is pushing the price down. Waiting is likely better than undercutting.")
		case diff < -0.2:
			lines = append(lines, "The price is unusually low without extra supply: someone is dumping or demand dipped. Check again next block.")
		case diff > 0.2:
			lines = append(lines, "The price is higher than usual for this time. A good moment to list.")
		}
	case n.Min == 0:
		lines = append(lines, "None listed right now, so you set the price. Use the median sold price as the reference.")
	}
	if a.Best != "" {
		best := a.Blocks[0]
		for _, b := range a.Blocks {
			if b.Label == a.Best {
				best = b
			}
		}
		lines = append(lines, fmt.Sprintf("Best time to post is %s Berlin: about %s net per listing (sells %s of the time at a median %s).",
			a.Best, Gold(a.BestExpected), Pct(best.SellRate), Gold(best.MedianSoldPrice)))
		if a.NowRanked && n.Block != a.Best {
			lines = append(lines, fmt.Sprintf("Posting now (%s) expects %s.", n.Block, Gold(a.NowExpected)))
		}
	} else {
		lines = append(lines, fmt.Sprintf("Not enough finished auctions to rank posting times yet (each time block needs %d).", o.MinSamples))
	}
	if a.Deposit > 0 {
		lines = append(lines, fmt.Sprintf("Deposit about %s per listing, lost only if it doesn't sell (%.0f%% of vendor price, unconfirmed for Forever).",
			Gold(a.Deposit), o.DepositRate*100))
	}
	return lines
}

// Sell renders the analysis for the terminal or as CSV (time-of-day table).
func Sell(ctx context.Context, st *store.Store, o SellOptions) error {
	a, err := AnalyzeSell(ctx, st, o)
	if err != nil {
		return err
	}
	Header(ctx, st, o.Options, fmt.Sprintf("Sell %s (%d)", a.Name, a.ItemID), nil)
	w := o.Out
	if !o.CSV {
		fmt.Fprintf(w, "history: %d snapshots, %d auctions posted, %d units ended, last %d days (times Europe/Berlin)\n",
			a.Snapshots, a.Auctions, a.EndedUnits, a.Days)
		if a.Thin {
			fmt.Fprintln(w, "thin history: rankings firm up after ~2 weeks of collection or ~20 ended auctions")
		}
		fmt.Fprintln(w)
	}
	header := []string{"posted (Berlin)", "median min", "median p20", "median listed", "posted", "sell rate est (lo–hi)",
		"median sold price", "median time to sell", "undercut ≤2h", "sold/day seen", "expected net/listing"}
	table := func(first string, set []SellBlock) *Table {
		h := append([]string(nil), header...)
		h[0] = first
		t := &Table{Header: h}
		for _, b := range set {
			rate, ev := "-", "-"
			if b.SellRate >= 0 {
				rate = fmt.Sprintf("%s (%s–%s)", Pct(b.SellRate), Pct(b.SellLo), Pct(b.SellHi))
			}
			if b.Ranked {
				ev = Gold(b.Expected)
			}
			t.Add(b.Label, Gold(b.MedianMin), Gold(b.MedianP20), fmt.Sprint(b.MedianListed), fmt.Sprint(b.Posted), rate,
				Gold(b.MedianSoldPrice), dur(b.MedianSellSecs), Pct(b.Undercut2h), Range(b.SoldPerDay, b.SoldPerDayLo, b.SoldPerDayHi), ev)
		}
		return t
	}
	if err := table("posted (Berlin)", a.Blocks).Write(o.Options); err != nil || o.CSV {
		return err
	}
	fmt.Fprintln(w)
	table("weekday", a.Weekdays).Write(o.Options)
	fmt.Fprintln(w)
	pt := &Table{Header: []string{"price when posted", "posted", "sell rate est (lo–hi)"}}
	for _, p := range a.Positions {
		rate := "-"
		if p.Rate >= 0 {
			rate = fmt.Sprintf("%s (%s–%s)", Pct(p.Rate), Pct(p.Lo), Pct(p.Hi))
		}
		pt.Add(p.Label, fmt.Sprint(p.Posted), rate)
	}
	pt.Write(o.Options)
	fmt.Fprintln(w)
	for _, l := range a.Verdict {
		fmt.Fprintln(w, l)
	}
	return nil
}

func dur(secs int64) string {
	if secs <= 0 {
		return "-"
	}
	d := time.Duration(secs) * time.Second
	if d >= time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func loadSnapPoints(ctx context.Context, db *sql.DB, houseID int64, k Key, since int64) ([]snapPoint, error) {
	// The item's kind decides which snapshots count (absent from a snapshot = 0 listed).
	var kind string
	if err := db.QueryRowContext(ctx, `SELECT kind FROM auction WHERE house_id=? AND item_id=? LIMIT 1`, houseID, k.Item).Scan(&kind); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(NULLIF(s.last_modified,0), s.fetched_at), COALESCE(i.min_unit,0), COALESCE(i.p20_unit,0), COALESCE(i.listed_qty,0)
		FROM snapshot s LEFT JOIN item_snapshot i ON i.snapshot_id=s.id AND i.item_id=? AND i.variant=?
		WHERE s.house_id=? AND s.kind=? AND s.fetched_at>=? ORDER BY s.fetched_at`, k.Item, k.Variant, houseID, kind, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []snapPoint
	for rows.Next() {
		var p snapPoint
		if err := rows.Scan(&p.t, &p.min, &p.p20, &p.listed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func loadSellAuctions(ctx context.Context, db *sql.DB, houseID int64, k Key, since, now int64) ([]sellAuction, error) {
	// Auctions already listed in the very first snapshot have an unknown post time: skip them.
	var firstSnap sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(COALESCE(NULLIF(last_modified,0), fetched_at)) FROM snapshot WHERE house_id=?
		AND kind=(SELECT kind FROM auction WHERE house_id=? AND item_id=? LIMIT 1)`, houseID, houseID, k.Item).Scan(&firstSnap); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT a.first_seen, COALESCE(a.closed_at,0), COALESCE(a.outcome,''), COALESCE(a.unit_price,0), a.qty_first, a.qty_last,
		COALESCE((SELECT i.min_unit FROM snapshot s JOIN item_snapshot i ON i.snapshot_id=s.id
		          WHERE s.house_id=a.house_id AND s.kind=a.kind AND COALESCE(NULLIF(s.last_modified,0), s.fetched_at)=a.first_seen
		          AND i.item_id=a.item_id AND i.variant=a.variant LIMIT 1),0)
		FROM auction a WHERE a.house_id=? AND a.item_id=? AND a.variant=? AND a.first_seen>=? AND a.first_seen>? AND a.unit_price>0
		ORDER BY a.first_seen`, houseID, k.Item, k.Variant, since, firstSnap.Int64)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sellAuction
	for rows.Next() {
		var a sellAuction
		var snapMin int64
		if err := rows.Scan(&a.first, &a.closed, &a.outcome, &a.price, &a.qty, &a.qtyLast, &snapMin); err != nil {
			return nil, err
		}
		switch r := float64(a.price) / float64(max(snapMin, 1)); {
		case snapMin == 0 || a.price <= snapMin:
			a.position = "cheapest"
		case r <= 1.05:
			a.position = "≤5% above min"
		case r <= 1.15:
			a.position = "5–15% above"
		default:
			a.position = ">15% above"
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Undercut: the first later listing priced below this one while it was still open.
	for i := range out {
		a := &out[i]
		a.undercutAfter = -1
		end := a.closed
		if end == 0 {
			end = now
		}
		for j := i + 1; j < len(out) && out[j].first <= end; j++ {
			if out[j].first > a.first && out[j].price < a.price {
				a.undercutAfter = out[j].first - a.first
				break
			}
		}
	}
	return out, nil
}

// addSaleEvents calls fn with certain = 1 (sold), 0 (ambiguous) or -1 (unsold) per event.
func addSaleEvents(ctx context.Context, db *sql.DB, houseID int64, k Key, since int64, fn func(t int64, certain int, units int64)) error {
	rows, err := db.QueryContext(ctx, `SELECT ts, outcome, units FROM sale_event WHERE house_id=? AND item_id=? AND variant=? AND ts>=?
		UNION ALL
		SELECT closed_at, outcome, qty_last FROM auction WHERE house_id=? AND item_id=? AND variant=? AND closed_at>=?`,
		houseID, k.Item, k.Variant, since, houseID, k.Item, k.Variant, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t, units int64
		var outcome string
		if err := rows.Scan(&t, &outcome, &units); err != nil {
			return err
		}
		c := -1
		if tracker.Certain(outcome) {
			c = 1
		} else if tracker.Ambiguous(outcome) {
			c = 0
		}
		fn(t, c, units)
	}
	return rows.Err()
}

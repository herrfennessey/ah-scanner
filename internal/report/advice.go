package report

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/stats"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

// The advisor answers the questions you have standing at the auction house:
// is this a good price, should I sell or hold, should I cancel and undercut, what can I flip.
// Every answer leads with a verdict and carries its reasons and a confidence label.

// AdviceHouse is one house as the advisor needs it.
type AdviceHouse struct {
	ID        int64
	Faction   string
	Cut       float64
	PriceOnly bool // reference house we can't trade on (Alliance from a Horde character)
}

// AdviceConfig holds the tunables shared by all answers.
type AdviceConfig struct {
	AmbiguousWeight float64
	DepositRate     float64
	Now             time.Time
}

// ItemContext is what we know about one item in one house right now.
type ItemContext struct {
	ItemID      int64   `json:"item_id"`
	Variant     string  `json:"variant,omitempty"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Min         int64   `json:"min"`          // lowest unit buyout now
	Listed      int64   `json:"listed"`       // units listed now
	Fair        int64   `json:"market_value"` // 7-day median p20 (or today's p20 early on)
	TypicalSold int64   `json:"typical_sold_price"`
	SoldEst     float64 `json:"sold_per_day"`
	SoldLo      float64 `json:"sold_per_day_lo"`
	SoldHi      float64 `json:"sold_per_day_hi"`
	Volatile    bool    `json:"volatile"`
	DepositUnit int64   `json:"deposit_per_unit"` // estimate
	DataDays    int     `json:"data_days"`
	Confidence  string  `json:"confidence"` // early | fair | good

	recentMins []int64           // lowest price per snapshot, last 7 days
	book       []listing.Listing // open listings, cheapest first (UnitPrice, Quantity only)
	profile    stats.Profile     // share of daily sales per Berlin hour
	now        time.Time
}

// prof returns the demand profile, flat when unknown.
func (ic *ItemContext) prof() stats.Profile {
	var sum float64
	for _, v := range ic.profile {
		sum += v
	}
	if sum == 0 {
		return stats.Flat()
	}
	return ic.profile
}

func (ic *ItemContext) clock() time.Time {
	if ic.now.IsZero() {
		return time.Now()
	}
	return ic.now
}

// lambda is the expected units bought over the next `hours`, following the hour-of-day profile.
func (ic *ItemContext) lambda(hours float64) float64 {
	return ic.prof().Lambda(ic.SoldEst, ic.clock(), hours, rollup.Berlin)
}

// sellsIn is how long until `units` would be bought, from now.
func (ic *ItemContext) sellsIn(units float64) string {
	h := ic.prof().HoursUntil(ic.SoldEst, ic.clock(), units, rollup.Berlin)
	if math.IsInf(h, 1) {
		return "a very long time"
	}
	return days(h / 24)
}

// profileCache holds house-wide hour-of-day profiles; they change slowly.
var profileCache = struct {
	sync.Mutex
	m map[string]profileEntry
}{m: map[string]profileEntry{}}

type profileEntry struct {
	at time.Time
	p  stats.Profile
}

// hourCounts sums sold units (certain + weighted ambiguous) per Berlin hour over the window.
// itemID 0 means the whole house (for the kind).
func hourCounts(ctx context.Context, db *sql.DB, houseID int64, kind string, itemID int64, since int64, w float64) ([24]float64, error) {
	var counts [24]float64
	filter := ""
	a1 := []any{w, houseID, kind, since}
	a2 := []any{houseID, kind, since}
	if itemID > 0 {
		filter = " AND item_id=?"
		a1 = append(a1, itemID)
		a2 = append(a2, itemID)
	}
	rows, err := db.QueryContext(ctx, `SELECT closed_at/3600, SUM(CASE WHEN outcome='sold' THEN qty_last
			WHEN outcome IN ('cancelled','expired_or_sold') THEN qty_last*? ELSE 0 END)
		FROM auction WHERE house_id=? AND kind=? AND closed_at>=?`+filter+` GROUP BY 1
		UNION ALL SELECT ts/3600, SUM(units) FROM sale_event WHERE house_id=? AND kind=? AND ts>=?`+filter+` GROUP BY 1`,
		append(a1, a2...)...)
	if err != nil {
		return counts, err
	}
	defer rows.Close()
	for rows.Next() {
		var hour int64
		var units float64
		if err := rows.Scan(&hour, &units); err != nil {
			return counts, err
		}
		counts[time.Unix(hour*3600, 0).In(rollup.Berlin).Hour()] += units
	}
	return counts, rows.Err()
}

// demandProfile is the item's hour-of-day profile, shrunk toward the house's, which is shrunk
// toward flat. Items with few sales effectively use the house pattern.
func demandProfile(ctx context.Context, db *sql.DB, houseID int64, kind string, itemID int64, now time.Time, w float64) stats.Profile {
	since := now.AddDate(0, 0, -14).Unix()
	key := fmt.Sprintf("%d/%s", houseID, kind)
	profileCache.Lock()
	e, ok := profileCache.m[key]
	profileCache.Unlock()
	if !ok || now.Sub(e.at) > time.Hour {
		counts, err := hourCounts(ctx, db, houseID, kind, 0, since, w)
		if err != nil {
			return stats.Flat()
		}
		e = profileEntry{at: now, p: stats.ProfileFrom(counts, stats.Flat(), 240)}
		profileCache.Lock()
		profileCache.m[key] = e
		profileCache.Unlock()
	}
	counts, err := hourCounts(ctx, db, houseID, kind, itemID, since, w)
	if err != nil {
		return e.p
	}
	return stats.ProfileFrom(counts, e.p, 48)
}

func confidence(days int) string {
	switch {
	case days < 3:
		return "early"
	case days < 10:
		return "fair"
	}
	return "good"
}

// LoadItemContext gathers prices, sales and the current order book for one item.
func LoadItemContext(ctx context.Context, st *store.Store, h AdviceHouse, cfg AdviceConfig, itemQuery, variant string) (*ItemContext, error) {
	id, err := ResolveItem(ctx, st.DB, itemQuery)
	if err != nil {
		return nil, err
	}
	ic := &ItemContext{ItemID: id, Variant: variant}
	var sellPrice int64
	st.DB.QueryRowContext(ctx, `SELECT COALESCE(name,''), COALESCE(sell_price,0) FROM item WHERE id=?`, id).Scan(&ic.Name, &sellPrice)
	if ic.Name == "" {
		ic.Name = fmt.Sprintf("Item #%d", id)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT kind FROM auction WHERE house_id=? AND item_id=? LIMIT 1`, h.ID, id).Scan(&ic.Kind); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	ic.DepositUnit = int64(math.Round(float64(sellPrice) * cfg.DepositRate))

	// Order book: open auctions, cheapest first.
	rows, err := st.DB.QueryContext(ctx, `SELECT unit_price, qty_last FROM auction WHERE house_id=? AND item_id=? AND variant=?
		AND closed_at IS NULL AND unit_price > 0 ORDER BY unit_price`, h.ID, id, variant)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l listing.Listing
		rows.Scan(&l.UnitPrice, &l.Quantity)
		ic.book = append(ic.book, l)
		ic.Listed += l.Quantity
	}
	rows.Close()
	if len(ic.book) > 0 {
		ic.Min = ic.book[0].UnitPrice
	}

	since := cfg.Now.AddDate(0, 0, -7).Unix()
	rows, err = st.DB.QueryContext(ctx, `SELECT COALESCE(i.min_unit,0), COALESCE(i.p20_unit,0) FROM snapshot s JOIN item_snapshot i ON i.snapshot_id=s.id
		WHERE s.house_id=? AND s.fetched_at>=? AND i.item_id=? AND i.variant=? ORDER BY s.fetched_at`, h.ID, since, id, variant)
	if err != nil {
		return nil, err
	}
	var p20s []int64
	for rows.Next() {
		var mn, p20 int64
		rows.Scan(&mn, &p20)
		if mn > 0 {
			ic.recentMins = append(ic.recentMins, mn)
		}
		if p20 > 0 {
			p20s = append(p20s, p20)
		}
	}
	rows.Close()

	daily, w, err := LoadDaily(ctx, st.DB, h.ID, 7, cfg.Now)
	if err != nil {
		return nil, err
	}
	ic.DataDays = w.Days
	if w.Partial {
		ic.DataDays = 0
	}
	if d := daily[Key{id, variant}]; d != nil {
		ic.Fair, ic.SoldEst, ic.SoldLo, ic.SoldHi, ic.Volatile = d.Fair, d.SoldEst, d.SoldLo, d.SoldHi, d.Volatile()
	}
	if ic.Fair == 0 {
		ic.Fair = medianI(p20s)
	}
	ic.Confidence = confidence(ic.DataDays)
	ic.now = cfg.Now
	if ic.Kind != "" {
		ic.profile = demandProfile(ctx, st.DB, h.ID, ic.Kind, id, cfg.Now, cfg.AmbiguousWeight)
	}

	// Typical sold price: median unit price of auctions that certainly sold, 14 days.
	rows, err = st.DB.QueryContext(ctx, `SELECT unit_price FROM auction WHERE house_id=? AND item_id=? AND variant=? AND outcome IN ('sold')
		AND closed_at >= ? AND unit_price > 0
		UNION ALL SELECT unit_price FROM sale_event WHERE house_id=? AND item_id=? AND variant=? AND ts >= ? AND unit_price > 0`,
		h.ID, id, variant, cfg.Now.AddDate(0, 0, -14).Unix(), h.ID, id, variant, cfg.Now.AddDate(0, 0, -14).Unix())
	if err != nil {
		return nil, err
	}
	var sold []int64
	for rows.Next() {
		var p int64
		rows.Scan(&p)
		sold = append(sold, p)
	}
	rows.Close()
	ic.TypicalSold = medianI(sold)
	return ic, nil
}

// unitsBelow counts listed units strictly cheaper than price, and units at exactly price.
func (ic *ItemContext) unitsBelow(price int64) (below, at int64) {
	for _, l := range ic.book {
		switch {
		case l.UnitPrice < price:
			below += l.Quantity
		case l.UnitPrice == price:
			at += l.Quantity
		}
	}
	return below, at
}

// percentileOf returns the share of recent snapshot minimums at or below price.
func (ic *ItemContext) percentileOf(price int64) float64 {
	if len(ic.recentMins) == 0 {
		return -1
	}
	n := 0
	for _, m := range ic.recentMins {
		if m <= price {
			n++
		}
	}
	return float64(n) / float64(len(ic.recentMins))
}

// expectedSold returns E[min(qty, max(0, N-ahead))] for N ~ Poisson(lambda): the units of a
// listing that sell when `ahead` cheaper units must go first (the commodity AH fills
// cheapest-first; for gear it is a fair approximation of buyers picking the cheapest).
func expectedSold(lambda float64, ahead, qty int64) float64 {
	if lambda <= 0 || qty <= 0 {
		return 0
	}
	// Sum over k of P(N=k) * min(qty, max(0, k-ahead)); stop once the tail is negligible.
	p := math.Exp(-lambda)
	var e, cum float64
	limit := int64(lambda+10*math.Sqrt(lambda)+20) + ahead + qty
	for k := int64(0); k <= limit; k++ {
		if k > 0 {
			p *= lambda / float64(k)
		}
		cum += p
		if got := k - ahead; got > 0 {
			e += p * float64(min(got, qty))
		}
	}
	// Mass beyond the limit sells everything.
	if cum < 1 {
		e += (1 - cum) * float64(qty)
	}
	return e
}

// hoursLeft is the expected remaining time for a time_left bucket.
func hoursLeft(tl string) float64 {
	switch tl {
	case listing.Short:
		return 0.25
	case listing.Medium:
		return 1.25
	case listing.Long:
		return 7
	case listing.VeryLong:
		return 30
	}
	return 7
}

// ---------------------------------------------------------------- price check

type PriceCheck struct {
	Item        *ItemContext `json:"item"`
	Price       int64        `json:"price"`
	Verdict     string       `json:"verdict"` // GOOD BUY | FAIR | EXPENSIVE | NO DATA
	SellVerdict string       `json:"sell_verdict"`
	Percentile  float64      `json:"percentile_7d"` // share of the last 7 days' lowest prices at or below this price; -1 unknown
	UnitsBelow  int64        `json:"units_listed_cheaper"`
	Reasons     []string     `json:"reasons"`
}

func CheckPrice(ic *ItemContext, price int64) *PriceCheck {
	pc := &PriceCheck{Item: ic, Price: price, Percentile: ic.percentileOf(price)}
	pc.UnitsBelow, _ = ic.unitsBelow(price)
	ref := ic.Fair
	if ref == 0 {
		ref = ic.Min
	}
	if ref == 0 {
		pc.Verdict, pc.SellVerdict = "NO DATA", "NO DATA"
		pc.Reasons = []string{"This item has not been seen on this auction house yet."}
		return pc
	}
	ratio := float64(price) / float64(ref)
	switch {
	case ratio <= 0.85:
		pc.Verdict = "GOOD BUY"
	case ratio <= 1.05:
		pc.Verdict = "FAIR"
	default:
		pc.Verdict = "EXPENSIVE"
	}
	pc.Reasons = append(pc.Reasons, fmt.Sprintf("%s is %s market value (%s).", Gold(price), relative(ratio), Gold(ref)))
	switch {
	case pc.Percentile == 0:
		pc.Reasons = append(pc.Reasons, "That's cheaper than the lowest listing in every scan of the last 7 days.")
	case pc.Percentile >= 1:
		pc.Reasons = append(pc.Reasons, "Something was listed cheaper than this in every scan of the last 7 days.")
	case pc.Percentile > 0:
		pc.Reasons = append(pc.Reasons, fmt.Sprintf("The lowest listing was this cheap or cheaper in %s of scans over the last 7 days.", Pct(pc.Percentile)))
	}
	if ic.TypicalSold > 0 {
		pc.Reasons = append(pc.Reasons, fmt.Sprintf("Auctions that actually sold went for a median %s.", Gold(ic.TypicalSold)))
	}
	switch {
	case pc.UnitsBelow == 0 && ic.Listed > 0:
		pc.Reasons = append(pc.Reasons, "Nothing is listed cheaper right now.")
	case pc.UnitsBelow > 0:
		pc.Reasons = append(pc.Reasons, fmt.Sprintf("%d units are listed cheaper right now (lowest %s).", pc.UnitsBelow, Gold(ic.Min)))
	}

	// As a seller: how long until it sells at this price?
	switch {
	case ic.SoldEst <= 0:
		pc.SellVerdict = "UNKNOWN"
		pc.Reasons = append(pc.Reasons, "No sales seen yet, so time to sell is unknown.")
	case ratio > 1.15:
		pc.SellVerdict = "TOO HIGH"
		pc.Reasons = append(pc.Reasons, fmt.Sprintf("To sell at this price, %d cheaper units would have to go first: about %s at %s sold per day.",
			pc.UnitsBelow, ic.sellsIn(float64(pc.UnitsBelow+1)), num(ic.SoldEst)))
	default:
		pc.SellVerdict = "GOOD TO SELL"
		pc.Reasons = append(pc.Reasons, fmt.Sprintf("Listed at this price, one unit would take %s to sell.", ic.sellsIn(float64(pc.UnitsBelow+1))))
	}
	return pc
}

func relative(ratio float64) string {
	switch d := ratio - 1; {
	case math.Abs(d) < 0.02:
		return "right at"
	case d < 0:
		return fmt.Sprintf("%.0f%% below", -d*100)
	default:
		return fmt.Sprintf("%.0f%% above", d*100)
	}
}

func days(d float64) string {
	h := d * 24
	switch {
	case h < 1:
		return "under an hour"
	case h < 36:
		return fmt.Sprintf("about %.0f h", h)
	}
	return fmt.Sprintf("about %.0f days", d)
}

// ---------------------------------------------------------------- sell or hold

type SellAdvice struct {
	Item     *ItemContext `json:"item"`
	Qty      int64        `json:"qty"`
	Verdict  string       `json:"verdict"` // SELL NOW | SELL IN PARTS | HOLD | TEST THE MARKET
	Price    int64        `json:"suggested_price"`
	ListNow  int64        `json:"list_now"` // units to list now
	SellsIn  string       `json:"sells_in"`
	NetNow   int64        `json:"net_if_all_sell"` // listed units, after cut
	BestTime string       `json:"best_time,omitempty"`
	Reasons  []string     `json:"reasons"`
}

// AdviseSell decides what to do with qty units of an item. now/typical come from the sell
// analysis for the current time block (typicalMin 0 = unknown).
func AdviseSell(ic *ItemContext, qty int64, cut float64, typicalMin, typicalListed int64, bestTime string) *SellAdvice {
	sa := &SellAdvice{Item: ic, Qty: qty, BestTime: bestTime}
	if qty <= 0 {
		qty, sa.Qty = 1, 1
	}
	ref := ic.Fair
	switch {
	case ic.Min == 0 && ref == 0:
		sa.Verdict = "TEST THE MARKET"
		sa.Reasons = []string{"Nobody lists this here and there is no price history. List one at a price you'd be happy with and see if it sells."}
		return sa
	case ic.Min == 0:
		sa.Verdict, sa.Price, sa.ListNow = "SELL NOW", ref, min(qty, max(1, int64(math.Ceil(ic.SoldEst))))
		sa.Reasons = append(sa.Reasons, fmt.Sprintf("None listed right now: you set the price. Market value is %s.", Gold(ref)))
	default:
		sa.Price = ic.Min - 1
		if ic.Kind == string(listing.Gear) && ic.Min > 100 {
			sa.Price = ic.Min - ic.Min%100 - 1 // undercut to the silver for gear, cleaner to read
			if sa.Price <= 0 {
				sa.Price = ic.Min - 1
			}
		}
	}
	if ref == 0 {
		ref = ic.Min
	}
	low := ic.Min > 0 && float64(ic.Min) < 0.8*float64(ref)
	glut := typicalListed > 0 && float64(ic.Listed) > 1.4*float64(typicalListed)
	switch {
	case sa.Verdict != "":
	case low && glut:
		sa.Verdict = "HOLD"
		sa.Reasons = append(sa.Reasons, fmt.Sprintf("Lowest price %s is %s market value, and %.1f× the usual supply is listed. A glut like this usually clears; undercutting into it gives money away.",
			Gold(ic.Min), relative(float64(ic.Min)/float64(ref)), float64(ic.Listed)/float64(typicalListed)))
	case low:
		sa.Verdict = "HOLD"
		sa.Reasons = append(sa.Reasons, fmt.Sprintf("Lowest price %s is %s market value without extra supply: one seller is probably dumping. Check again in a few hours.",
			Gold(ic.Min), relative(float64(ic.Min)/float64(ref))))
	case ic.SoldEst <= 0:
		sa.Verdict = "TEST THE MARKET"
		sa.Reasons = append(sa.Reasons, "No sales seen yet. List one just under the lowest price and see if it moves.")
		sa.ListNow = 1
	default:
		sa.Verdict = "SELL NOW"
	}
	if typicalMin > 0 && ic.Min > 0 && sa.Verdict == "SELL NOW" {
		sa.Reasons = append(sa.Reasons, fmt.Sprintf("Price now is %s what's usual for this time of day (%s).", relative(float64(ic.Min)/float64(typicalMin)), Gold(typicalMin)))
	}
	if sa.Verdict == "SELL NOW" && ic.SoldEst > 0 {
		perDay := ic.SoldLo
		if perDay < 1 {
			perDay = ic.SoldEst
		}
		if float64(qty) > 1.5*math.Max(perDay, 1) {
			sa.Verdict = "SELL IN PARTS"
			sa.ListNow = int64(math.Max(1, math.Round(perDay)))
			sa.Reasons = append(sa.Reasons, fmt.Sprintf("Only about %s sell per day here. Listing all %d pushes you behind your own stack and invites undercuts; list %d now and the rest over the next days.",
				num(perDay), qty, sa.ListNow))
		}
	}
	if sa.ListNow == 0 {
		sa.ListNow = qty
	}
	if ic.SoldEst > 0 && sa.Price > 0 {
		below, _ := ic.unitsBelow(sa.Price + 1)
		if sa.Price < ic.Min {
			below = 0
		}
		sa.SellsIn = ic.sellsIn(float64(below + sa.ListNow))
		sa.Reasons = append(sa.Reasons, fmt.Sprintf("About %s sell per day (range %s–%s). Counting when buyers are actually active, %d at %s would take %s.",
			num(ic.SoldEst), num(ic.SoldLo), num(ic.SoldHi), sa.ListNow, Gold(sa.Price), sa.SellsIn))
	}
	sa.NetNow = int64(math.Round(float64(sa.Price*sa.ListNow) * (1 - cut)))
	if sa.Verdict == "HOLD" && bestTime != "" {
		sa.Reasons = append(sa.Reasons, fmt.Sprintf("When you do sell, %s Berlin has been the best time to post.", bestTime))
	}
	if ic.Volatile {
		sa.Reasons = append(sa.Reasons, "This item's price swings a lot; treat the numbers as rough.")
	}
	return sa
}

// ---------------------------------------------------------------- cancel or ride

type CancelAdvice struct {
	Item        *ItemContext `json:"item"`
	MyPrice     int64        `json:"my_price"`
	Qty         int64        `json:"qty"`
	TimeLeft    string       `json:"time_left"`
	FoundMine   int          `json:"matching_listings"` // listings in the latest scan at my price and quantity
	Ahead       int64        `json:"units_ahead"`
	Verdict     string       `json:"verdict"` // LET IT RIDE | CANCEL AND RELIST | ALREADY CHEAPEST
	RelistPrice int64        `json:"relist_price"`
	RideSells   float64      `json:"ride_expected_units_sold"`
	RelistSells float64      `json:"relist_expected_units_sold"`
	RideValue   int64        `json:"ride_expected_value"`
	RelistValue int64        `json:"relist_expected_value"`
	Reasons     []string     `json:"reasons"`
}

// AdviseCancel compares letting a listing run against cancelling and relisting just under the
// lowest price. Expected values count proceeds after cut, deposit refunds on sale, the new
// deposit, and the units you keep if they don't sell (valued at market value after cut, less 10%
// for the wait). The old deposit is only in the "ride" value, as a refund if it sells.
func AdviseCancel(ic *ItemContext, myPrice, qty int64, timeLeft string, cut float64, durationHours float64) *CancelAdvice {
	if qty <= 0 {
		qty = 1
	}
	if durationHours <= 0 {
		durationHours = 24
	}
	ca := &CancelAdvice{Item: ic, MyPrice: myPrice, Qty: qty, TimeLeft: timeLeft}
	// Which listing is mine? Matches at my price and quantity; take mine out of the book.
	below, at := ic.unitsBelow(myPrice)
	for _, l := range ic.book {
		if l.UnitPrice == myPrice && l.Quantity == qty {
			ca.FoundMine++
		}
	}
	// Units at the same price: order unknown, assume half are ahead.
	ca.Ahead = below + max(0, at-qty)/2
	if timeLeft == "" {
		timeLeft = listing.Long
		ca.TimeLeft = timeLeft
	}
	ref := ic.Fair
	if ref == 0 {
		ref = ic.Min
	}
	keepValue := float64(ref) * (1 - cut) * 0.9
	ride := expectedSold(ic.lambda(hoursLeft(timeLeft)), ca.Ahead, qty)
	ca.RideSells = ride
	ca.RideValue = int64(math.Round(ride*(float64(myPrice)*(1-cut)+float64(ic.DepositUnit)) + (float64(qty)-ride)*keepValue))

	if ca.Ahead == 0 {
		ca.Verdict = "ALREADY CHEAPEST"
		ca.Reasons = append(ca.Reasons, "Nothing is listed cheaper than you, so there is nothing to undercut. Let it ride.")
		ca.RelistValue = ca.RideValue
		return ca
	}
	// Relist one copper (gear: to the silver) under the cheapest listing that isn't mine.
	ca.RelistPrice = ic.Min - 1
	if ic.Kind == string(listing.Gear) && ic.Min > 100 {
		ca.RelistPrice = ic.Min - ic.Min%100 - 1
	}
	if ca.RelistPrice <= 0 {
		ca.RelistPrice = ic.Min
	}
	relist := expectedSold(ic.lambda(durationHours), 0, qty)
	ca.RelistSells = relist
	newDeposit := float64(ic.DepositUnit * qty)
	ca.RelistValue = int64(math.Round(relist*(float64(ca.RelistPrice)*(1-cut)+float64(ic.DepositUnit)) - newDeposit + (float64(qty)-relist)*keepValue))

	gain := ca.RelistValue - ca.RideValue
	threshold := max(int64(1000), int64(0.02*float64(ca.RideValue))) // 10s or 2%: not worth the click below that
	if gain > threshold {
		ca.Verdict = "CANCEL AND RELIST"
	} else {
		ca.Verdict = "LET IT RIDE"
		if gain > 0 {
			ca.Reasons = append(ca.Reasons, fmt.Sprintf("Relisting comes out only %s ahead, under the %s it takes to be worth cancelling, paying a new deposit and risking another undercut. If you'd rather have the gold now than hold the items, relisting is still reasonable.",
				signedGold(gain), Gold(threshold)))
		}
	}
	if ca.Verdict == "LET IT RIDE" && ride < 0.3*float64(qty) && ref > 0 && float64(ca.RelistPrice) < 0.9*float64(ref) {
		ca.Reasons = append(ca.Reasons, fmt.Sprintf("Your listing probably won't sell soon either way, but undercutting means selling at %s, %s market value (%s). Holding beats dumping: let it ride or expire, then relist when the cheap listings have cleared.",
			Gold(ca.RelistPrice), relative(float64(ca.RelistPrice)/float64(ref)), Gold(ref)))
	}
	ca.Reasons = append(ca.Reasons,
		fmt.Sprintf("%d units are listed ahead of you. With about %s selling per day and ~%s left on your listing, you can expect %.1f of your %d to sell.",
			ca.Ahead, num(ic.SoldEst), hoursText(hoursLeft(timeLeft)), ride, qty),
		fmt.Sprintf("Relisting at %s puts you first in line: about %.1f of %d sell over %s.", Gold(ca.RelistPrice), relist, qty, hoursText(durationHours)),
		fmt.Sprintf("Expected value: let it ride %s, cancel and relist %s (%s).", Gold(ca.RideValue), Gold(ca.RelistValue), signedGold(gain)),
	)
	if ic.DepositUnit > 0 {
		ca.Reasons = append(ca.Reasons, fmt.Sprintf("About the deposit: you only get your current %s back if it sells. Cancelling forfeits it, which is already counted above, so it's no reason to stay on its own. The relist costs a new %s deposit (estimate).",
			Gold(ic.DepositUnit*qty), Gold(ic.DepositUnit*qty)))
	}
	switch ca.FoundMine {
	case 0:
		ca.Reasons = append(ca.Reasons, "No listing at exactly that price and quantity in the latest scan; check the numbers or wait for the next scan.")
	case 1:
	default:
		ca.Reasons = append(ca.Reasons, fmt.Sprintf("%d listings match that price and quantity, so your place in line is approximate.", ca.FoundMine))
	}
	ca.Reasons = append(ca.Reasons, "This doesn't account for being undercut again after relisting.")
	return ca
}

func hoursText(h float64) string {
	if h < 1 {
		return fmt.Sprintf("%.0f min", h*60)
	}
	return fmt.Sprintf("%.0f h", h)
}

func signedGold(c int64) string {
	if c >= 0 {
		return "+" + Gold(c)
	}
	return "−" + Gold(-c)
}

// ---------------------------------------------------------------- flips

type Flip struct {
	ItemID     int64   `json:"item_id"`
	Variant    string  `json:"variant,omitempty"`
	Name       string  `json:"name"`
	BuyHouse   string  `json:"buy_house"`
	SellHouse  string  `json:"sell_house"`
	BuyUnits   int64   `json:"buy_units"`
	BuyUpTo    int64   `json:"buy_up_to_price"`
	BuyCost    int64   `json:"buy_cost"`
	SellAt     int64   `json:"sell_at"`
	Profit     int64   `json:"expected_profit"` // after cut and deposit, if all sell at sell_at
	ROI        float64 `json:"roi"`
	DaysToSell float64 `json:"days_to_sell"` // at the sold_lo rate
	SoldLo     float64 `json:"sold_per_day_lo"`
	SalesSeen  int64   `json:"sales_seen" jsonschema:"separate auctions seen selling in the sell house, last 7 days"`
	// Alliance check (Horde → Blackwater only).
	AlliancePrice  int64  `json:"alliance_price,omitempty" jsonschema:"what Alliance players pay at home now; 0 = not listed there"`
	AllianceMargin int64  `json:"alliance_flipper_margin,omitempty" jsonschema:"what an Alliance player makes buying your listing and reselling at home after a 5% cut"`
	AllianceNote   string `json:"alliance_note,omitempty"`
}

type FlipOptions struct {
	MinProfit      int64   // copper, default 2g
	MinROI         float64 // default 0.15
	MaxDays        float64 // max days to sell at the floor rate, default 3
	MinSalesPerDay float64 // separate sales per day seen in the sell house, default 2
	AllianceEdge   float64 // Alliance price must beat the Blackwater resale by this much, default 0.05
	Limit          int
	// Funnel, if non-nil, receives per-route counts of why items were rejected.
	Funnel map[string]*FlipFunnel
}

// FlipFunnel counts items at each stage of the flip search for one route (e.g. horde → blackwater).
type FlipFunnel struct {
	Candidates     int `json:"candidates"`          // items with a market value and sales in the sell house
	NotInBuyHouse  int `json:"not_listed_to_buy"`   // nothing listed in the buy house
	TooFewSales    int `json:"too_few_sales"`       // rule 2: under 5 separate sales or under 2 a day
	NoProfitGap    int `json:"no_profitable_gap"`   // rule 1: nothing cheap enough to clear cut, deposit and margin
	AllianceBlocks int `json:"blocked_by_alliance"` // rule 3: Alliance pays too little at home
	Passed         int `json:"passed"`
}

type refPrice struct{ min, fair int64 }

// allianceGate is rule 3 for Horde → Blackwater flips: Alliance players should pay more at home
// than your Blackwater price, so buying from you is their better deal. An item not listed on the
// Alliance AH passes: they can't buy it at home at all.
func allianceGate(sellAt int64, ref refPrice, haveRef bool, edge float64) (ok bool, price, margin int64, note string) {
	if !haveRef || ref.min == 0 {
		if !haveRef {
			return true, 0, 0, "no Alliance data for this item"
		}
		return true, 0, 0, "not listed on the Alliance AH: Alliance players can only get it here"
	}
	price = ref.min
	if ref.fair > 0 && ref.fair < price {
		price = ref.fair // don't trust a momentary spike
	}
	margin = int64(float64(price)*0.95) - sellAt
	if float64(price) < float64(sellAt)*(1+edge) {
		return false, price, margin, fmt.Sprintf("Alliance pays %s at home, not enough above your %s", Gold(price), Gold(sellAt))
	}
	if margin > 0 {
		note = fmt.Sprintf("Alliance pays %s at home; their flippers make %s a unit buying yours", Gold(price), Gold(margin))
	} else {
		note = fmt.Sprintf("Alliance pays %s at home, so buying yours saves them money", Gold(price))
	}
	return true, price, margin, note
}

type houseBook struct {
	h     AdviceHouse
	daily map[Key]*Daily
	books map[Key][]listing.Listing
	sales map[Key]saleEvidence
	days  float64
}

// saleEvidence is how many separate auctions certainly sold, and how big the biggest one was.
type saleEvidence struct {
	auctions       int64
	units, largest int64
}

func loadHouseBook(ctx context.Context, st *store.Store, h AdviceHouse, now time.Time) (*houseBook, int, error) {
	daily, w, err := LoadDaily(ctx, st.DB, h.ID, 7, now)
	if err != nil {
		return nil, 0, err
	}
	hb := &houseBook{h: h, daily: daily, books: map[Key][]listing.Listing{}, sales: map[Key]saleEvidence{}, days: math.Max(1, float64(w.Days))}
	since := now.AddDate(0, 0, -7).Unix()
	ev, err := st.DB.QueryContext(ctx, `SELECT item_id, variant, count(*), SUM(u), MAX(u) FROM (
			SELECT item_id, variant, qty_last u FROM auction WHERE house_id=? AND outcome='sold' AND closed_at>=?
			UNION ALL SELECT item_id, variant, units FROM sale_event WHERE house_id=? AND ts>=?)
		GROUP BY item_id, variant`, h.ID, since, h.ID, since)
	if err != nil {
		return nil, 0, err
	}
	for ev.Next() {
		var k Key
		var e saleEvidence
		ev.Scan(&k.Item, &k.Variant, &e.auctions, &e.units, &e.largest)
		hb.sales[k] = e
	}
	ev.Close()
	rows, err := st.DB.QueryContext(ctx, `SELECT item_id, variant, unit_price, qty_last FROM auction WHERE house_id=? AND closed_at IS NULL AND unit_price > 0`, h.ID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var k Key
		var l listing.Listing
		rows.Scan(&k.Item, &k.Variant, &l.UnitPrice, &l.Quantity)
		hb.books[k] = append(hb.books[k], l)
	}
	for _, b := range hb.books {
		sort.Slice(b, func(i, j int) bool { return b[i].UnitPrice < b[j].UnitPrice })
	}
	days := w.Days
	if w.Partial {
		days = 0
	}
	return hb, days, rows.Err()
}

// FindFlips looks for items listed below what they reliably sell for, within each house and
// across houses (one character can use both). Profit assumes reselling at market value in the
// sell house, after its cut and deposit, and only buys what the floor sales rate absorbs in
// MaxDays.
func FindFlips(ctx context.Context, st *store.Store, houses []AdviceHouse, cfg AdviceConfig, o FlipOptions) ([]Flip, string, error) {
	if o.MinProfit <= 0 {
		o.MinProfit = 20000
	}
	if o.MinROI <= 0 {
		o.MinROI = 0.15
	}
	if o.MaxDays <= 0 {
		o.MaxDays = 3
	}
	if o.MinSalesPerDay <= 0 {
		o.MinSalesPerDay = 2
	}
	if o.AllianceEdge <= 0 {
		o.AllianceEdge = 0.05
	}
	books := make([]*houseBook, 0, len(houses))
	dataDays := 99
	var ally map[Key]refPrice
	for _, h := range houses {
		if h.PriceOnly {
			var err error
			if ally, err = loadRefPrices(ctx, st, h, cfg.Now); err != nil {
				return nil, "", err
			}
			continue
		}
		hb, d, err := loadHouseBook(ctx, st, h, cfg.Now)
		if err != nil {
			return nil, "", err
		}
		books = append(books, hb)
		dataDays = min(dataDays, d)
	}
	sellPrices := map[int64]int64{}
	rows, err := st.DB.QueryContext(ctx, `SELECT id, COALESCE(sell_price,0) FROM item`)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var id, sp int64
		rows.Scan(&id, &sp)
		sellPrices[id] = sp
	}
	rows.Close()
	names, err := LoadNames(ctx, st.DB)
	if err != nil {
		return nil, "", err
	}

	var flips []Flip
	for _, buy := range books {
		for _, sell := range books {
			fn := &FlipFunnel{}
			if o.Funnel != nil {
				o.Funnel[buy.h.Faction+" → "+sell.h.Faction] = fn
			}
			for k, d := range sell.daily {
				if d.Fair == 0 || d.SoldLo < 0.5 || d.Volatile() {
					continue
				}
				fn.Candidates++
				book := buy.books[k]
				if len(book) == 0 {
					fn.NotInBuyHouse++
					continue
				}
				deposit := int64(math.Round(float64(sellPrices[k.Item]) * cfg.DepositRate))
				// Evidence: several separate certain sales, and buy only what sold without the
				// single largest sale (one whale or one misread cancel can't justify a flip).
				e := sell.sales[k]
				if e.auctions < 5 || float64(e.auctions)/sell.days < o.MinSalesPerDay {
					fn.TooFewSales++
					continue // rule 2: it must sell often in the sell house
				}
				robustPerDay := float64(e.units-e.largest) / sell.days
				capUnits := int64(math.Min(d.SoldLo, robustPerDay) * o.MaxDays)
				if capUnits < 1 {
					fn.TooFewSales++
					continue
				}
				units, cost, upTo, sellAt := planFlip(book, sell.books[k], buy == sell, d.Fair, sell.h.Cut, deposit, o.MinROI, capUnits)
				if units == 0 {
					fn.NoProfitGap++
					continue
				}
				netPerUnit := float64(sellAt)*(1-sell.h.Cut) - float64(deposit)
				profit := int64(math.Round(netPerUnit*float64(units))) - cost
				roi := float64(profit) / float64(cost)
				if profit < o.MinProfit || roi < o.MinROI {
					fn.NoProfitGap++
					continue
				}
				f := Flip{ItemID: k.Item, Variant: k.Variant, Name: ItemLabel(names, k), BuyHouse: buy.h.Faction, SellHouse: sell.h.Faction,
					BuyUnits: units, BuyUpTo: upTo, BuyCost: cost, SellAt: sellAt, Profit: profit, ROI: roi,
					DaysToSell: float64(units) / math.Min(d.SoldLo, robustPerDay), SoldLo: d.SoldLo, SalesSeen: e.auctions}
				if sell.h.Faction == "blackwater" && buy != sell && ally != nil {
					ref, have := ally[k]
					ok, price, margin, note := allianceGate(sellAt, ref, have, o.AllianceEdge)
					if !ok {
						fn.AllianceBlocks++
						continue // rule 3: Alliance must pay more at home
					}
					f.AlliancePrice, f.AllianceMargin, f.AllianceNote = price, margin, note
				}
				fn.Passed++
				flips = append(flips, f)
			}
		}
	}
	sort.Slice(flips, func(i, j int) bool { return flips[i].Profit > flips[j].Profit })
	if o.Limit > 0 && len(flips) > o.Limit {
		flips = flips[:o.Limit]
	}
	return flips, confidence(dataDays), nil
}

// loadRefPrices reads a price-only house: current lowest price and 7-day market value per item.
func loadRefPrices(ctx context.Context, st *store.Store, h AdviceHouse, now time.Time) (map[Key]refPrice, error) {
	out := map[Key]refPrice{}
	rows, err := st.DB.QueryContext(ctx, `SELECT item_id, variant, COALESCE(min_unit,0), COALESCE(p20_unit,0) FROM item_snapshot
		WHERE snapshot_id IN (SELECT MAX(id) FROM snapshot WHERE house_id=? GROUP BY kind)`, h.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k Key
		var r refPrice
		var p20 int64
		rows.Scan(&k.Item, &k.Variant, &r.min, &p20)
		r.fair = p20
		out[k] = r
	}
	rows.Close()
	daily, _, err := LoadDaily(ctx, st.DB, h.ID, 7, now)
	if err != nil {
		return nil, err
	}
	for k, d := range daily {
		if r, ok := out[k]; ok && d.Fair > 0 {
			r.fair = d.Fair
			out[k] = r
		}
	}
	return out, nil
}

// planFlip decides how many units to buy and at what price they can realistically be resold:
// market value, or just under the cheapest competing listing in the sell house if that is lower.
// In the same house the competition is whatever is left after your own purchases, so the plan
// is refined until buying and resale price agree.
func planFlip(book, sellBook []listing.Listing, sameHouse bool, fair int64, cut float64, deposit int64, minROI float64, capUnits int64) (units, cost, upTo, sellAt int64) {
	sellAt = fair
	if !sameHouse && len(sellBook) > 0 && sellBook[0].UnitPrice-1 < sellAt {
		sellAt = sellBook[0].UnitPrice - 1
	}
	for iter := 0; iter < 4; iter++ {
		net := float64(sellAt)*(1-cut) - float64(deposit)
		maxBuy := int64(net / (1 + minROI))
		units, cost, upTo = 0, 0, 0
		var left int64 // units left at the last level we bought from
		for _, l := range book {
			if l.UnitPrice > maxBuy || units >= capUnits {
				break
			}
			take := min(l.Quantity, capUnits-units) // commodities can be bought partially; gear is qty 1
			units += take
			cost += take * l.UnitPrice
			upTo = l.UnitPrice
			left = l.Quantity - take
		}
		if !sameHouse || units == 0 {
			return units, cost, upTo, sellAt
		}
		// Competition after buying: the rest of a partly bought level, else the next listing.
		next := int64(0)
		if left > 0 {
			next = upTo
		} else {
			var seen int64
			for _, l := range book {
				seen += l.Quantity
				if seen > units {
					next = l.UnitPrice
					break
				}
			}
		}
		want := fair
		if next > 0 && next-1 < want {
			want = next - 1
		}
		if want == sellAt {
			return units, cost, upTo, sellAt
		}
		sellAt = want
	}
	return units, cost, upTo, sellAt
}

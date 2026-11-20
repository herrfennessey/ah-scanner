// Package tracker diffs a snapshot against the auctions that were open before it and
// classifies every change according to the sell-through rules (spec section 5).
package tracker

import "github.com/herrfennessey/ah-scanner/internal/listing"

// Outcomes stored on auctions and sale events.
const (
	Sold          = "sold"            // rule 5: vanished with time to spare and no cancel signal
	Partial       = "partial"         // rule 1: commodity quantity dropped on the same auction
	Cancelled     = "cancelled"       // rule 4a: vanished with time to spare, reposted cheaper (probably not sold); rules 4c, 4d for gear
	ExpiredOrSold = "expired_or_sold" // rule 3: vanished when it could have expired
	// Unsold: a commodity that vanished while cheaper units of the same item, listed before it,
	// stayed untouched. The commodity AH fills buys cheapest-first, so nobody bought it: it was
	// cancelled or expired. Certain, so it widens neither bound (extends rule 4b to any time left).
	Unsold = "unsold"
)

// Certain reports whether units with this outcome count toward sold_lo.
func Certain(outcome string) bool { return outcome == Sold || outcome == Partial }

// Ambiguous reports whether units with this outcome widen the range up to sold_hi.
func Ambiguous(outcome string) bool { return outcome == Cancelled || outcome == ExpiredOrSold }

// Open is an auction that was open after the previous snapshot.
type Open struct {
	AuctionID int64
	ItemID    int64
	Variant   string
	UnitPrice int64
	Qty       int64
	TimeLeft  string
	LastSeen  int64
}

// Event is units leaving the market for one auction.
type Event struct {
	AuctionID int64
	ItemID    int64
	Variant   string
	UnitPrice int64
	Units     int64
	Outcome   string
}

type Result struct {
	New    []listing.Listing // first seen in this snapshot
	Seen   []listing.Listing // still listed (possibly with lower quantity)
	Events []Event           // partial drops and closures
	// Closed maps auction ID → final outcome for auctions that vanished.
	Closed map[int64]string
}

type itemKey struct {
	item    int64
	variant string
}

type repostKey struct {
	item    int64
	variant string
	qty     int64
}

// Diff classifies cur against open. now is the snapshot time (Unix seconds).
func Diff(kind listing.Kind, open map[int64]Open, cur []listing.Listing, now int64) Result {
	res := Result{Closed: map[int64]string{}}
	present := make(map[int64]struct{}, len(cur))

	// Cheapest new repost per (item, variant, qty) and cheapest untouched old listing per item.
	newMin := map[repostKey]int64{}
	untouchedMin := map[itemKey]int64{}

	for _, l := range cur {
		present[l.AuctionID] = struct{}{}
		o, ok := open[l.AuctionID]
		if !ok {
			res.New = append(res.New, l)
			if l.UnitPrice > 0 {
				k := repostKey{l.ItemID, l.Variant, l.Quantity}
				if p, ok := newMin[k]; !ok || l.UnitPrice < p {
					newMin[k] = l.UnitPrice
				}
			}
			continue
		}
		res.Seen = append(res.Seen, l)
		if kind == listing.Commodity && l.Quantity < o.Qty {
			res.Events = append(res.Events, Event{
				AuctionID: o.AuctionID, ItemID: o.ItemID, Variant: o.Variant,
				UnitPrice: o.UnitPrice, Units: o.Qty - l.Quantity, Outcome: Partial,
			})
		} else if l.Quantity == o.Qty && l.UnitPrice > 0 {
			k := itemKey{l.ItemID, l.Variant}
			if p, ok := untouchedMin[k]; !ok || l.UnitPrice < p {
				untouchedMin[k] = l.UnitPrice
			}
		}
	}

	for id, o := range open {
		if _, ok := present[id]; ok {
			continue
		}
		outcome := classifyVanished(kind, o, now, newMin, untouchedMin)
		res.Closed[id] = outcome
		res.Events = append(res.Events, Event{
			AuctionID: o.AuctionID, ItemID: o.ItemID, Variant: o.Variant,
			UnitPrice: o.UnitPrice, Units: o.Qty, Outcome: outcome,
		})
	}
	return res
}

func classifyVanished(kind listing.Kind, o Open, now int64, newMin map[repostKey]int64, untouchedMin map[itemKey]int64) string {
	// Rule 4b, applied whatever the time left: commodities fill cheapest-first, so a buyer
	// could not have skipped cheaper units that were listed before and are still untouched.
	if kind == listing.Commodity {
		if p, ok := untouchedMin[itemKey{o.ItemID, o.Variant}]; ok && p < o.UnitPrice {
			return Unsold
		}
	}
	// Rules 2/3: if the gap since we last saw it is at least the minimum time it had left,
	// it may simply have expired.
	gap := now - o.LastSeen
	if gap >= listing.MinRemaining(o.TimeLeft) {
		return ExpiredOrSold
	}
	// Rule 4a: reposted cheaper — a new auction of the same item and quantity at a lower price.
	// Rule 4d: gear reposted at the same price too. A copy that vanishes as an identical one
	// appears is a cancel and repost as often as a restock after a sale (a fifth of gear
	// "sales" on Horde, 2026-11-14 to 19), so it's ambiguous, not a certain sale.
	if p, ok := newMin[repostKey{o.ItemID, o.Variant, o.Qty}]; ok && o.UnitPrice > 0 &&
		(p < o.UnitPrice || (kind == listing.Gear && p == o.UnitPrice)) {
		return Cancelled
	}
	// Rule 4c: gear that vanished while a cheaper copy, listed before, is still there. Buyers
	// take the cheapest copy, so it was most likely cancelled (on a busy house, often to be
	// reposted from another character); ambiguous, not a certain sale.
	if kind == listing.Gear && o.UnitPrice > 0 {
		if p, ok := untouchedMin[itemKey{o.ItemID, o.Variant}]; ok && p < o.UnitPrice {
			return Cancelled
		}
	}
	return Sold
}

package supply

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// crateTierFavor is Merchant's Favor for a sealed crate of raw materials (Textiles, Ore, Herbs,
// Curiosities); crafted families (Fabrics, Ingots, Parts) pay double. Artisan is not known yet.
// Source: https://www.wow-professions.com/forever/merchants-favor-durotar-supply-azeroth-commerce
var crateTierFavor = map[string]int{"Apprentice": 5, "Journeyman": 10, "Expert": 15}
var craftedFamily = map[string]bool{"Fabrics": true, "Ingots": true, "Parts": true}

// CrateFavor returns the Favor a filled crate is worth, from its name, or 0 if unknown.
func CrateFavor(name string) int {
	words := strings.Fields(strings.TrimPrefix(name, "Waylaid Crate: "))
	if len(words) < 2 {
		return 0
	}
	favor := 0
	for _, w := range words {
		if f, ok := crateTierFavor[w]; ok {
			favor = f
		}
	}
	if craftedFamily[words[len(words)-1]] {
		favor *= 2
	}
	return favor
}

// Level is one price level of an order book: unit price and units, cheapest first.
type Level struct{ P, Q int64 }

// BuyCost is the cost of the cheapest qty units in book, and whether the book has that many.
func BuyCost(book []Level, qty int64) (int64, bool) {
	var cost int64
	for _, l := range book {
		take := min(l.Q, qty)
		cost += take * l.P
		qty -= take
		if qty == 0 {
			return cost, true
		}
	}
	return cost, qty <= 0
}

// Bundle is one fill option of a crate.
type Bundle struct {
	Qty  int64  `json:"qty"`
	Name string `json:"name"`
}

var bundleLine = regexp.MustCompile(`(?m)^\s*-\s*(\d+)\s+(.+?)\s*$`)

// ParseFill reads the fill options from a crate's "Use: Fill the crate with any bundle from
// the following list:" tooltip text.
func ParseFill(desc string) []Bundle {
	if !strings.Contains(desc, "Fill the crate") {
		return nil
	}
	var out []Bundle
	for _, m := range bundleLine.FindAllStringSubmatch(desc, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		out = append(out, Bundle{Qty: n, Name: m[2]})
	}
	return out
}

// Prices answers order-book and name lookups for the planner.
type Prices interface {
	Book(itemID int64) []Level
	ItemID(name string) (int64, bool)
}

// BookPrices is Prices over in-memory order books.
type BookPrices struct {
	Books  map[int64][]Level
	ByName map[string][]int64
}

func (p *BookPrices) Book(id int64) []Level { return p.Books[id] }

// ItemID resolves a name, preferring an item with listings, else the lowest id: names can be
// shared (a quest copy of a potion, say), and the listed one is the one you can buy.
func (p *BookPrices) ItemID(name string) (int64, bool) {
	ids := p.ByName[name]
	if len(ids) == 0 {
		return 0, false
	}
	best, listed := int64(0), false
	for _, id := range ids {
		has := len(p.Books[id]) > 0
		switch {
		case best == 0, has && !listed, has == listed && id < best:
			best, listed = id, has
		}
	}
	return best, true
}

// FillCost is one fill option priced on the AH.
type FillCost struct {
	Bundle
	ItemID int64 `json:"item_id"`
	Cost   int64 `json:"cost"`
	OK     bool  `json:"ok"` // enough listed to buy the bundle
}

// Crate is one Waylaid Crate type, priced.
type Crate struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Favor     int        `json:"favor"` // 0 = not known
	CrateCost int64      `json:"crate_cost"`
	Best      *FillCost  `json:"best,omitempty"`
	Options   []FillCost `json:"options"`
	Total     int64      `json:"total"`
	PerFavor  float64    `json:"per_favor"` // 0 = can't be ranked (see Note)
	Note      string     `json:"note,omitempty"`
}

// Writ is one named writ (or a sealed writ's expected order), priced.
type Writ struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Item      string   `json:"item"`
	ItemID    int64    `json:"item_id"`
	Qty       int64    `json:"qty"`
	Tier      string   `json:"tier"`
	Rep       int      `json:"rep"`
	WritCost  int64    `json:"writ_cost"`
	ItemCost  int64    `json:"item_cost"`
	Pouch     int64    `json:"pouch"`
	Net       int64    `json:"net"`     // pouch − writ − item: positive means you earn gold
	PerRep    float64  `json:"per_rep"` // gold spent per rep (negative = earned); 0 when rep unknown
	OK        bool     `json:"ok"`      // writ and item both buyable
	Customer  Customer `json:"customer"`
	Continent string   `json:"continent"`
	itemOK    bool     // enough of the requested item listed
}

// Inputs is everything the planner needs.
type Inputs struct {
	Prices Prices
	Fills  map[int64]string // crate id → its "Use: Fill the crate…" tooltip text
	Names  map[int64]string // crate, writ and sealed writ ids → name (only these are planned)
	Rep    map[int64]int    // writ item id → quest reputation, when fetched
	Pouch  map[string]int64 // tier → coin pouch estimate, copper
}

// Receipt prices the Forged Delivery Receipt route: 15 Favor (at the best crate's gold per
// Favor) plus the cheapest empty crate, for 50 reputation.
type Receipt struct {
	PerRep float64 `json:"per_rep"`
	OK     bool    `json:"ok"`
}

type Result struct {
	Crates  []Crate `json:"crates"`
	Writs   []Writ  `json:"writs"`
	Sealed  []Writ  `json:"sealed"`
	Receipt Receipt `json:"receipt"`
}

// Plan prices every crate and writ named in in.Names.
func Plan(in Inputs) Result {
	var r Result
	cheapestCrate := int64(0)
	for _, id := range CrateIDs {
		name, ok := in.Names[id]
		if !ok {
			continue
		}
		c := planCrate(in, id, name)
		if c.CrateCost > 0 && (cheapestCrate == 0 || c.CrateCost < cheapestCrate) {
			cheapestCrate = c.CrateCost
		}
		r.Crates = append(r.Crates, c)
	}
	sort.SliceStable(r.Crates, func(i, j int) bool {
		a, b := r.Crates[i], r.Crates[j]
		if (a.PerFavor > 0) != (b.PerFavor > 0) {
			return a.PerFavor > 0
		}
		return a.PerFavor < b.PerFavor
	})
	if len(r.Crates) > 0 && r.Crates[0].PerFavor > 0 && cheapestCrate > 0 {
		r.Receipt = Receipt{PerRep: (ReceiptFavor*r.Crates[0].PerFavor + float64(cheapestCrate)) / ReceiptRep, OK: true}
	}

	for _, id := range WritIDs {
		name, ok := in.Names[id]
		if !ok {
			continue
		}
		r.Writs = append(r.Writs, planWrit(in, id, name))
	}
	sort.SliceStable(r.Writs, func(i, j int) bool {
		a, b := r.Writs[i], r.Writs[j]
		if a.OK != b.OK {
			return a.OK
		}
		if a.Net != b.Net {
			return a.Net > b.Net
		}
		return a.Rep > b.Rep
	})

	for _, id := range []int64{262888, 262889, 262890} {
		name, ok := in.Names[id]
		if !ok {
			continue
		}
		r.Sealed = append(r.Sealed, planSealed(in, id, name, SealedWrits[id], r.Writs))
	}
	return r
}

func planCrate(in Inputs, id int64, name string) Crate {
	c := Crate{ID: id, Name: name, Favor: CrateFavor(name)}
	cost, ok := BuyCost(in.Prices.Book(id), 1)
	if ok {
		c.CrateCost = cost
	}
	for _, b := range ParseFill(in.Fills[id]) {
		f := FillCost{Bundle: b}
		if iid, found := in.Prices.ItemID(b.Name); found {
			f.ItemID = iid
			f.Cost, f.OK = BuyCost(in.Prices.Book(iid), b.Qty)
		}
		c.Options = append(c.Options, f)
		if f.OK && (c.Best == nil || f.Cost < c.Best.Cost) {
			best := f
			c.Best = &best
		}
	}
	switch {
	case len(c.Options) == 0:
		c.Note = "fill list not known yet"
	case c.Best == nil:
		c.Note = "not enough of any fill on the AH"
	case !ok:
		c.Note = "no empty crate listed"
	case c.Favor == 0:
		c.Note = "Favor not known yet"
	}
	if c.Best != nil && ok {
		c.Total = c.CrateCost + c.Best.Cost
		if c.Favor > 0 {
			c.PerFavor = float64(c.Total) / float64(c.Favor)
		}
	}
	return c
}

func planWrit(in Inputs, id int64, name string) Writ {
	item := strings.TrimPrefix(name, "Craftsman's Writ: ")
	w := Writ{ID: id, Name: name, Item: item, Qty: RequestedQty(item), Customer: Customers[item]}
	w.Continent = Continents[w.Customer.Zone]
	w.Rep = in.Rep[id]
	w.Tier = TierForRep(w.Rep)
	if w.Tier == "" {
		w.Tier = w.Customer.Tier
		w.Rep = tierRep[w.Tier]
	}
	w.Pouch = in.Pouch[w.Tier]
	writCost, writOK := BuyCost(in.Prices.Book(id), 1)
	w.WritCost = writCost
	itemOK := false
	if iid, found := in.Prices.ItemID(item); found {
		w.ItemID = iid
		w.ItemCost, itemOK = BuyCost(in.Prices.Book(iid), w.Qty)
	}
	w.itemOK = itemOK
	w.OK = writOK && itemOK
	w.Net = w.Pouch - w.WritCost - w.ItemCost
	if w.Rep > 0 {
		w.PerRep = round2(-float64(w.Net) / float64(w.Rep))
	}
	return w
}

// planSealed values a sealed writ as an average order of its tier: it opens into one at random.
func planSealed(in Inputs, id int64, name, tier string, writs []Writ) Writ {
	w := Writ{ID: id, Name: name, Item: "random " + strings.ToLower(tier) + " order", Qty: 1, Tier: tier, Rep: tierRep[tier], Pouch: in.Pouch[tier]}
	cost, ok := BuyCost(in.Prices.Book(id), 1)
	w.WritCost = cost
	var sum, n int64
	for _, o := range writs {
		if o.Tier == tier && o.itemOK {
			sum += o.ItemCost
			n++
		}
	}
	w.OK = ok && n > 0
	if n > 0 {
		w.ItemCost = sum / n
	}
	w.Net = w.Pouch - w.WritCost - w.ItemCost
	if w.Rep > 0 {
		w.PerRep = round2(-float64(w.Net) / float64(w.Rep))
	}
	return w
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

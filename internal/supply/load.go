package supply

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ItemInfo is display data for an item the Supply page shows (icon, name, quality).
type ItemInfo struct {
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Quality int    `json:"quality"`
}

// View is the Supply page's data: the plan at one scan plus what's needed to draw it.
type View struct {
	Result
	Items      map[int64]ItemInfo `json:"items"` // every crate, reagent, writ and requested item shown
	Pouch      map[string]int64   `json:"pouch"`
	ComputedAt int64              `json:"computed_at"` // the Horde scan the prices come from
}

// Point is the best price of each route at one scan, for the Supply charts. A zero value with
// its OK flag false means nothing was buyable then.
type Point struct {
	T          int64   `json:"t"`
	PerFavor   float64 `json:"per_favor"`   // cheapest crate's copper per Favor
	FavorOK    bool    `json:"favor_ok"`    //
	PerRep     float64 `json:"per_rep"`     // cheapest buyable writ's copper per rep (negative: it pays you)
	RepOK      bool    `json:"rep_ok"`      //
	ReceiptRep float64 `json:"receipt_rep"` // Forged Delivery Receipt's copper per rep
	FreeWrits  int     `json:"free_writs"`  // buyable writs whose coin pouch covers their cost
}

// PointOf summarises a plan as a chart point.
func PointOf(t int64, r Result) Point {
	p := Point{T: t}
	for _, c := range r.Crates {
		if c.PerFavor > 0 && (!p.FavorOK || c.PerFavor < p.PerFavor) {
			p.PerFavor, p.FavorOK = c.PerFavor, true
		}
	}
	for _, w := range r.Writs {
		if !w.OK || w.Rep == 0 {
			continue
		}
		if !p.RepOK || w.PerRep < p.PerRep {
			p.PerRep, p.RepOK = w.PerRep, true
		}
		if w.Net >= 0 {
			p.FreeWrits++
		}
	}
	if r.Receipt.OK {
		p.ReceiptRep = r.Receipt.PerRep
	}
	return p
}

// Pouch merges coin pouch overrides (tier names in any case) over the guide defaults.
func Pouch(overrides map[string]int64) map[string]int64 {
	p := map[string]int64{}
	for t, v := range DefaultPouch {
		p[t] = v
	}
	for t, v := range overrides {
		if v > 0 && t != "" {
			p[strings.ToUpper(t[:1])+strings.ToLower(t[1:])] = v
		}
	}
	return p
}

// auc is one listing's life on the house, for rebuilding the order book at any scan.
type auc struct {
	item, price, qty int64
	opened, closed   int64 // closed 0 = still open
	partials         []sale
}

type sale struct{ ts, units int64 }

// Source holds a house's supply-relevant listings and static data, so the plan can be priced
// at any scan since it was loaded without going back to the database.
type Source struct {
	names    map[int64]string
	meta     map[int64]ItemInfo
	byName   map[string][]int64
	fills    map[int64]string
	reps     map[int64]int
	pouch    map[string]int64
	auctions []auc
}

// Load reads everything needed to price the plan at scans after since (0 = all retained
// history) on one house.
func Load(ctx context.Context, db *sql.DB, houseID, since int64, pouch map[string]int64) (*Source, error) {
	s := &Source{names: map[int64]string{}, meta: map[int64]ItemInfo{}, byName: map[string][]int64{},
		fills: map[int64]string{}, reps: map[int64]int{}, pouch: pouch}

	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(name,''), COALESCE(icon,''), COALESCE(quality,1) FROM item`)
	if err != nil {
		return nil, fmt.Errorf("items: %w", err)
	}
	for rows.Next() {
		var id int64
		var m ItemInfo
		if err := rows.Scan(&id, &m.Name, &m.Icon, &m.Quality); err != nil {
			rows.Close()
			return nil, err
		}
		if m.Name != "" {
			s.meta[id] = m
			s.byName[m.Name] = append(s.byName[m.Name], id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	planned := append(append([]int64{}, CrateIDs...), WritIDs...)
	for id := range SealedWrits {
		planned = append(planned, id)
	}
	for _, id := range planned {
		if m, ok := s.meta[id]; ok {
			s.names[id] = m.Name
		}
	}

	if err := s.loadFills(ctx, db); err != nil {
		return nil, err
	}
	reps, err := db.QueryContext(ctx, `SELECT item_id, rep FROM writ_quest`)
	if err != nil {
		return nil, fmt.Errorf("writ reps: %w", err)
	}
	for reps.Next() {
		var id int64
		var rep int
		if err := reps.Scan(&id, &rep); err != nil {
			reps.Close()
			return nil, err
		}
		s.reps[id] = rep
	}
	reps.Close()
	if err := reps.Err(); err != nil {
		return nil, err
	}

	// Every item the plan can price: crates, writs, their fills and requested items (all ids
	// sharing those names, since the planner picks among them).
	want := map[int64]bool{}
	for _, id := range planned {
		want[id] = true
	}
	addName := func(name string) {
		for _, id := range s.byName[name] {
			want[id] = true
		}
	}
	for _, desc := range s.fills {
		for _, b := range ParseFill(desc) {
			addName(b.Name)
		}
	}
	for _, id := range WritIDs {
		if n, ok := s.names[id]; ok {
			addName(strings.TrimPrefix(n, "Craftsman's Writ: "))
		}
	}
	return s, s.loadAuctions(ctx, db, houseID, since, want)
}

func (s *Source) loadFills(ctx context.Context, db *sql.DB) error {
	for _, id := range CrateIDs {
		var tt sql.NullString
		err := db.QueryRowContext(ctx, `SELECT tooltip FROM item WHERE id=?`, id).Scan(&tt)
		if err == sql.ErrNoRows || !tt.Valid {
			continue
		}
		if err != nil {
			return fmt.Errorf("crate tooltip: %w", err)
		}
		var p struct {
			Spells []struct {
				Description string `json:"description"`
			} `json:"spells"`
		}
		if json.Unmarshal([]byte(tt.String), &p) == nil {
			for _, sp := range p.Spells {
				if strings.Contains(sp.Description, "Fill the crate") {
					s.fills[id] = sp.Description
				}
			}
		}
	}
	return nil
}

func (s *Source) loadAuctions(ctx context.Context, db *sql.DB, houseID, since int64, want map[int64]bool) error {
	if len(want) == 0 {
		return nil
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, fmt.Sprint(id))
	}
	in := strings.Join(ids, ",") // integers only: safe to inline
	rows, err := db.QueryContext(ctx, `SELECT kind, auction_id, item_id, unit_price, qty_first, first_seen, COALESCE(closed_at,0)
		FROM auction WHERE house_id=? AND variant='' AND unit_price > 0 AND item_id IN (`+in+`)
		AND (closed_at IS NULL OR closed_at > ?)`, houseID, since)
	if err != nil {
		return fmt.Errorf("auctions: %w", err)
	}
	type key struct {
		kind string
		id   int64
	}
	index := map[key]int{}
	for rows.Next() {
		var k key
		var a auc
		if err := rows.Scan(&k.kind, &k.id, &a.item, &a.price, &a.qty, &a.opened, &a.closed); err != nil {
			rows.Close()
			return err
		}
		index[k] = len(s.auctions)
		s.auctions = append(s.auctions, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	ev, err := db.QueryContext(ctx, `SELECT kind, auction_id, ts, units FROM sale_event
		WHERE house_id=? AND outcome='partial' AND item_id IN (`+in+`)`, houseID)
	if err != nil {
		return fmt.Errorf("partial sales: %w", err)
	}
	defer ev.Close()
	for ev.Next() {
		var k key
		var e sale
		if err := ev.Scan(&k.kind, &k.id, &e.ts, &e.units); err != nil {
			return err
		}
		if i, ok := index[k]; ok {
			s.auctions[i].partials = append(s.auctions[i].partials, e)
		}
	}
	return ev.Err()
}

// At returns the planner inputs for the order book as it stood at scan time t: listings seen by
// then and not yet gone, less any units bought from them by then.
func (s *Source) At(t int64) Inputs {
	type lv struct{ item, price int64 }
	qty := map[lv]int64{}
	for _, a := range s.auctions {
		if a.opened > t || (a.closed != 0 && a.closed <= t) {
			continue
		}
		q := a.qty
		for _, e := range a.partials {
			if e.ts <= t {
				q -= e.units
			}
		}
		if q > 0 {
			qty[lv{a.item, a.price}] += q
		}
	}
	books := map[int64][]Level{}
	for k, q := range qty {
		books[k.item] = append(books[k.item], Level{P: k.price, Q: q})
	}
	for _, b := range books {
		sort.Slice(b, func(i, j int) bool { return b[i].P < b[j].P })
	}
	return Inputs{Prices: &BookPrices{Books: books, ByName: s.byName}, Fills: s.fills, Names: s.names, Rep: s.reps, Pouch: s.pouch}
}

// ViewAt prices the plan at scan time t with the display data the page needs.
func (s *Source) ViewAt(t int64) *View {
	v := &View{Result: Plan(s.At(t)), Items: map[int64]ItemInfo{}, Pouch: s.pouch, ComputedAt: t}
	add := func(id int64) {
		if m, ok := s.meta[id]; ok && id != 0 {
			v.Items[id] = m
		}
	}
	for _, c := range v.Crates {
		add(c.ID)
		for _, o := range c.Options {
			add(o.ItemID)
		}
	}
	for _, w := range append(append([]Writ{}, v.Writs...), v.Sealed...) {
		add(w.ID)
		add(w.ItemID)
	}
	return v
}

// ScanTimes lists a house's scans after since, as the data times auctions are stamped with. Gear
// and commodities publish separately, a few seconds to minutes apart; scans within 10 minutes
// count as one, at the later time, so both halves of the order book are in.
func ScanTimes(ctx context.Context, db *sql.DB, houseID, since int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT COALESCE(NULLIF(last_modified,0), fetched_at) AS t FROM snapshot
		WHERE house_id=? AND COALESCE(NULLIF(last_modified,0), fetched_at) > ? ORDER BY t`, houseID, since)
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
		if n := len(out); n > 0 && t-out[n-1] < 600 {
			out[n-1] = t
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

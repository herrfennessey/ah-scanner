package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/herrfennessey/ah-scanner/internal/supply"
)

// supplyItem is display data for an item the Supply page shows (icon, name, quality).
type supplyItem struct {
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Quality int    `json:"quality"`
}

type supplyOut struct {
	supply.Result
	Items      map[int64]supplyItem `json:"items"` // every crate, reagent, writ and requested item shown
	Pouch      map[string]int64     `json:"pouch"`
	ComputedAt int64                `json:"computed_at"` // the Horde scan the prices come from
}

func (s *Server) apiSupply(w http.ResponseWriter, r *http.Request) {
	out, err := s.supplyPlan(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, out)
}

// pouch merges the configured coin pouch estimates over the guide defaults.
func (s *Server) pouch() map[string]int64 {
	p := map[string]int64{}
	for t, v := range supply.DefaultPouch {
		p[t] = v
	}
	for t, v := range s.Cfg.Supply.CoinPouch {
		if v > 0 && t != "" {
			p[strings.ToUpper(t[:1])+strings.ToLower(t[1:])] = v
		}
	}
	return p
}

func (s *Server) supplyPlan(ctx context.Context) (*supplyOut, error) {
	h, err := s.houseByName(ctx, "horde")
	if err != nil {
		return nil, err
	}
	meta, err := loadMeta(ctx, s.Store.DB)
	if err != nil {
		return nil, err
	}
	prices := &supply.BookPrices{Books: map[int64][]supply.Level{}, ByName: map[string][]int64{}}
	for id, m := range meta {
		if m.name != "" {
			prices.ByName[m.name] = append(prices.ByName[m.name], id)
		}
	}
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT item_id, unit_price, SUM(qty_last) FROM auction
		WHERE house_id=? AND closed_at IS NULL AND unit_price > 0 AND variant='' GROUP BY item_id, unit_price ORDER BY item_id, unit_price`, h.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var l supply.Level
		if err := rows.Scan(&id, &l.P, &l.Q); err != nil {
			rows.Close()
			return nil, err
		}
		prices.Books[id] = append(prices.Books[id], l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	in := supply.Inputs{Prices: prices, Fills: map[int64]string{}, Names: map[int64]string{}, Pouch: s.pouch()}
	ids := append(append([]int64{}, supply.CrateIDs...), supply.WritIDs...)
	for id := range supply.SealedWrits {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if m, ok := meta[id]; ok && m.name != "" {
			in.Names[id] = m.name
		}
	}
	for _, id := range supply.CrateIDs {
		tt, err := s.Store.ItemTooltip(ctx, id)
		if err != nil {
			return nil, err
		}
		var p struct {
			Spells []struct {
				Description string `json:"description"`
			} `json:"spells"`
		}
		if json.Unmarshal(tt, &p) == nil {
			for _, sp := range p.Spells {
				if strings.Contains(sp.Description, "Fill the crate") {
					in.Fills[id] = sp.Description
				}
			}
		}
	}
	if in.Rep, err = s.Store.WritReps(ctx); err != nil {
		return nil, err
	}
	out := &supplyOut{Result: supply.Plan(in), Items: map[int64]supplyItem{}, Pouch: in.Pouch, ComputedAt: h.LastScan}
	add := func(id int64) {
		if m, ok := meta[id]; ok && id != 0 {
			out.Items[id] = supplyItem{Name: m.name, Icon: m.icon, Quality: m.quality}
		}
	}
	for _, c := range out.Crates {
		add(c.ID)
		for _, o := range c.Options {
			add(o.ItemID)
		}
	}
	for _, w := range append(append([]supply.Writ{}, out.Writs...), out.Sealed...) {
		add(w.ID)
		add(w.ItemID)
	}
	return out, nil
}

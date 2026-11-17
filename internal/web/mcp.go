package web

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/herrfennessey/ah-scanner/internal/collector"
	"github.com/herrfennessey/ah-scanner/internal/report"
)

const mcpInstructions = `Private auction house data for WoW Forever (EU, PvP ruleset), collected from Blizzard's API every 30 minutes.
Houses: "horde" (5% AH cut) and "blackwater" (neutral, 15% cut) are tradeable; "alliance" is view-only because a Horde character on a PvP realm can't use it, but Alliance players buy and sell on Blackwater, so its prices and sales matter. Prices are copper (1g = 10000c); *_text fields show them as gold.
Sales are inferred from listings that vanish, so sold figures come as est with a lo–hi range: treat lo as the floor and hi as the ceiling, never as exact.
History started when the collector was first run; time-of-day advice needs one to two weeks of data and says so when it is thin.
Times are Europe/Berlin.`

// mcpHandler serves the read-only MCP tools over streamable HTTP at /mcp.
func (s *Server) mcpHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ahc", Title: "WoW Forever AH Ledger", Version: "1.0.0"},
		&mcp.ServerOptions{Instructions: mcpInstructions})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}

	mcp.AddTool(srv, &mcp.Tool{Name: "search_items", Annotations: ro,
		Description: "Find items on an auction house by name (substring) or item ID, with current price, market value and sold per day."}, s.toolSearch)
	mcp.AddTool(srv, &mcp.Tool{Name: "item_summary", Annotations: ro,
		Description: "One item across both houses: current lowest buyout, market value, supply, sold per day, plus its daily history."}, s.toolItem)
	mcp.AddTool(srv, &mcp.Tool{Name: "sell_advice", Annotations: ro,
		Description: "When and at what price to list an item: price and supply now vs usual for this time, sell rate and expected net per listing by posting time (3-hour blocks and weekday), and how sell rate depends on undercutting."}, s.toolSell)
	mcp.AddTool(srv, &mcp.Tool{Name: "movers", Annotations: ro,
		Description: "Items that sell reliably, ranked by gold traded per day (sold per day × market value). Filter by item class, required level and minimum sales."}, s.toolMovers)
	mcp.AddTool(srv, &mcp.Tool{Name: "flips", Annotations: ro,
		Description: "Small trades for the player's gold (default 40g), for how they play. Three kinds: relist (buy listings dumped well under the next price in a house and relist just under it), cross (buy on Blackwater, sell on Horde) and alliance (buy on Horde, sell on Blackwater under what Alliance players pay at home, only for items the Alliance buys several times a day). The style (casual by default) is how often the player can come back, and decides which plays they're offered; each flip names its play: hold (everyone: post 24h and leave it even when undercut, since the commodity AH sells the cheaper listings first, then yours), check_in (casual and active: look every check_hours and relist just under the cheapest only if more than relist_queue units are listed cheaper than yours at the first visit; the queue buyers get through shrinks as the listing runs down, at demand_per_day) or camp (active only: check hourly and relist whenever undercut; the only play allowed volatile or sliding markets). Each item is planned with every play the style allows and the one with the most expected profit is kept, so active sees everything casual does and casual everything away does. Away is Horde only. Each flip must pass on evidence, not asking prices: the resale is capped at the median price buyers actually paid over the last 24h, at least 2 purchases at or above it in the last 12h (recent_sales_above), its size fits the recent purchases at or above that price, and markets whose asking price falls more than 10% a day are left out (except for camping). sell_chance is the measured share of this item's cheapest-when-posted listings that sold within one round of the play, and undercut_chance how often a cheaper one turned up in that time; expected_profit weighs the play's rounds by them (a round that doesn't sell costs a relist, the deposit and ~3%, only when undercut, or the deposit when the listing runs out; holds never relist), and values stock still unsold at the end of the window at the going price. Gear counts as sold only if no cheaper copy outlasted it, since a vanished listing may have been cancelled; buys within one scan of the listing appearing are never seen. Deposits are 20% of vendor price, 1s minimum per auction, 5x on Blackwater. gone_soon is the measured chance the cheap listing is gone within an hour: act fast. Ranked by expected profit. Never resells on Blackwater above the dearer of the Horde and Alliance prices. An empty list means nothing qualifies right now; the funnel says why."}, s.toolFlips)
	mcp.AddTool(srv, &mcp.Tool{Name: "collector_status", Annotations: ro,
		Description: "Health of the data: houses, listing counts, last scan time and any collector problems (failing or frozen feeds)."}, s.toolStatus)

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	if s.Cfg.MCPToken == "" {
		return h
	}
	want := []byte("Bearer " + s.Cfg.MCPToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "missing or wrong bearer token", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func ptr[T any](v T) *T { return &v }

// MCPItem is an item row shaped for a model: copper integers plus readable gold text.
type MCPItem struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Variant         string   `json:"variant,omitempty" jsonschema:"random suffix / bonus key; empty for the base item"`
	Quality         string   `json:"quality"`
	ItemLevel       int      `json:"item_level,omitempty"`
	RequiredLevel   int      `json:"required_level,omitempty"`
	Class           string   `json:"class,omitempty"`
	Subclass        string   `json:"subclass,omitempty"`
	MinBuyout       int64    `json:"min_buyout"`
	MinBuyoutText   string   `json:"min_buyout_text"`
	MarketValue     int64    `json:"market_value" jsonschema:"7-day median of the price at which the cheapest 20% of units clear"`
	MarketValueText string   `json:"market_value_text"`
	Available       int64    `json:"available"`
	Auctions        int64    `json:"auctions"`
	SoldPerDay      Sold     `json:"sold_per_day"`
	SellThrough     *float64 `json:"sell_through,omitempty" jsonschema:"share of ended units that sold (estimate)"`
	Change24h       *float64 `json:"change_24h,omitempty" jsonschema:"market value change over 24 h, as a fraction"`
	Volatile        bool     `json:"volatile,omitempty"`
}

type Sold struct {
	Est float64 `json:"est"`
	Lo  float64 `json:"lo"`
	Hi  float64 `json:"hi"`
}

var qualityNames = []string{"Poor", "Common", "Uncommon", "Rare", "Epic", "Legendary", "Artifact", "Heirloom"}

func toMCP(r ItemRow) MCPItem {
	q := "Common"
	if r.Quality >= 0 && r.Quality < len(qualityNames) {
		q = qualityNames[r.Quality]
	}
	m := MCPItem{ID: r.ID, Name: r.Name, Variant: r.Variant, Quality: q, ItemLevel: r.ItemLevel, RequiredLevel: r.ReqLevel,
		Class: r.Class, Subclass: r.Subclass, MinBuyout: r.Min, MinBuyoutText: report.Gold(r.Min), MarketValue: r.Fair,
		MarketValueText: report.Gold(r.Fair), Available: r.Available, Auctions: r.Auctions,
		SoldPerDay: Sold{round1(r.SoldEst), round1(r.SoldLo), round1(r.SoldHi)}, Change24h: r.Change24, Volatile: r.Volatile}
	if r.SellThru >= 0 {
		v := round2(r.SellThru)
		m.SellThrough = &v
	}
	return m
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// tool inputs and outputs

type SearchIn struct {
	Query string `json:"query" jsonschema:"item name (substring, case-insensitive) or item ID"`
	House string `json:"house,omitempty" jsonschema:"horde or blackwater; default horde"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results, default 20, max 100"`
}

type ItemsOut struct {
	House string    `json:"house"`
	Total int       `json:"total_matches"`
	Items []MCPItem `json:"items"`
}

func (s *Server) toolSearch(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, ItemsOut, error) {
	h, err := s.houseByName(ctx, in.House)
	if err != nil {
		return nil, ItemsOut{}, err
	}
	rows, err := s.buildRows(ctx, h.ID)
	if err != nil {
		return nil, ItemsOut{}, err
	}
	q := strings.ToLower(strings.TrimSpace(in.Query))
	var hits []ItemRow
	for _, r := range rows {
		if q == "" || strings.Contains(strings.ToLower(r.Name), q) || fmt.Sprint(r.ID) == q {
			hits = append(hits, r)
		}
	}
	// Exact names first, then the most traded.
	sort.SliceStable(hits, func(i, j int) bool {
		ei, ej := strings.EqualFold(hits[i].Name, q), strings.EqualFold(hits[j].Name, q)
		if ei != ej {
			return ei
		}
		return hits[i].SoldEst*float64(hits[i].Fair) > hits[j].SoldEst*float64(hits[j].Fair)
	})
	out := ItemsOut{House: h.Faction, Total: len(hits)}
	for _, r := range hits[:min(len(hits), clampLimit(in.Limit, 20, 100))] {
		out.Items = append(out.Items, toMCP(r))
	}
	return nil, out, nil
}

type ItemIn struct {
	Item string `json:"item" jsonschema:"item ID or name"`
	Days int    `json:"days,omitempty" jsonschema:"days of daily history, default 14"`
}

type HouseItem struct {
	House     string   `json:"house"`
	AHCut     float64  `json:"ah_cut"`
	Listed    bool     `json:"listed"`
	Item      *MCPItem `json:"item,omitempty"`
	DailyRows []Daily  `json:"daily"`
}

type Daily struct {
	Day         string  `json:"day"`
	MarketValue int64   `json:"market_value"`
	MinBuyout   int64   `json:"min_buyout"`
	AvgListed   int64   `json:"avg_listed"`
	Sold        Sold    `json:"sold"`
	PriceCV     float64 `json:"price_cv,omitempty"`
}

type ItemOut struct {
	ID     int64       `json:"id"`
	Name   string      `json:"name"`
	Houses []HouseItem `json:"houses"`
}

func (s *Server) toolItem(ctx context.Context, _ *mcp.CallToolRequest, in ItemIn) (*mcp.CallToolResult, ItemOut, error) {
	id, err := report.ResolveItem(ctx, s.Store.DB, in.Item)
	if err != nil {
		return nil, ItemOut{}, err
	}
	days := clampLimit(in.Days, 14, 90)
	hs, err := s.houseList(ctx)
	if err != nil {
		return nil, ItemOut{}, err
	}
	out := ItemOut{ID: id}
	s.Store.DB.QueryRowContext(ctx, `SELECT COALESCE(name,'') FROM item WHERE id=?`, id).Scan(&out.Name)
	from := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	for _, h := range hs {
		hi := HouseItem{House: h.Faction, AHCut: h.Cut, DailyRows: []Daily{}}
		rows, err := s.buildRows(ctx, h.ID)
		if err != nil {
			return nil, out, err
		}
		for _, r := range rows {
			if r.ID == id && r.Variant == "" {
				m := toMCP(r)
				hi.Item, hi.Listed = &m, r.Available > 0
			}
		}
		dr, err := s.Store.DB.QueryContext(ctx, `SELECT day, COALESCE(fair_unit,0), COALESCE(min_unit,0), avg_listed, sold_est, sold_lo, sold_hi, COALESCE(price_cv,0)
			FROM item_daily WHERE house_id=? AND item_id=? AND variant='' AND day>=? ORDER BY day`, h.ID, id, from)
		if err != nil {
			return nil, out, err
		}
		for dr.Next() {
			var d Daily
			var est, lo, hi2 int64
			dr.Scan(&d.Day, &d.MarketValue, &d.MinBuyout, &d.AvgListed, &est, &lo, &hi2, &d.PriceCV)
			d.Sold = Sold{float64(est), float64(lo), float64(hi2)}
			d.PriceCV = round2(d.PriceCV)
			hi.DailyRows = append(hi.DailyRows, d)
		}
		dr.Close()
		out.Houses = append(out.Houses, hi)
	}
	return nil, out, nil
}

type SellIn struct {
	Item    string `json:"item" jsonschema:"item ID or name"`
	House   string `json:"house,omitempty" jsonschema:"horde or blackwater; default horde"`
	Days    int    `json:"days,omitempty" jsonschema:"days of history to analyse, default 14"`
	Variant string `json:"variant,omitempty" jsonschema:"variant key for random-suffix gear; default the most listed"`
}

type SellOut struct {
	House    string               `json:"house"`
	Analysis *report.SellAnalysis `json:"analysis"`
	TopCells []report.SellBlock   `json:"best_weekday_time_cells" jsonschema:"up to 5 best weekday × time-of-day cells by expected net"`
}

func (s *Server) toolSell(ctx context.Context, _ *mcp.CallToolRequest, in SellIn) (*mcp.CallToolResult, SellOut, error) {
	h, err := s.houseByName(ctx, in.House)
	if err != nil {
		return nil, SellOut{}, err
	}
	a, err := report.AnalyzeSell(ctx, s.Store, report.SellOptions{
		Options: report.Options{HouseID: h.ID, HouseName: h.Name, Cut: h.Cut, DepositRate: s.Cfg.DepositRate, Days: in.Days, Now: time.Now()},
		Item:    in.Item, Variant: in.Variant, BlockHours: 3, AmbiguousWeight: s.Cfg.AmbiguousWeight,
	})
	if err != nil {
		return nil, SellOut{}, err
	}
	var cells []report.SellBlock
	for _, row := range a.Heat {
		for _, c := range row {
			if c.Ranked {
				cells = append(cells, c)
			}
		}
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].Expected > cells[j].Expected })
	out := SellOut{House: h.Faction, TopCells: cells[:min(5, len(cells))]}
	a.Heat = nil // 56 cells is a lot of tokens; the blocks, weekdays and top cells carry the signal
	out.Analysis = a
	return nil, out, nil
}

type MoversIn struct {
	House        string  `json:"house,omitempty" jsonschema:"horde or blackwater; default horde"`
	MinSoldLo    float64 `json:"min_sold_lo,omitempty" jsonschema:"minimum sold per day floor (lo), default 5"`
	Class        string  `json:"class,omitempty" jsonschema:"item class, e.g. Trade Goods, Consumable, Armor, Weapon, Recipe"`
	MaxReqLevel  int     `json:"max_required_level,omitempty"`
	MaxItemLevel int     `json:"max_item_level,omitempty" jsonschema:"useful to find goods a low-level character can farm"`
	Limit        int     `json:"limit,omitempty" jsonschema:"default 25, max 100"`
}

func (s *Server) toolMovers(ctx context.Context, _ *mcp.CallToolRequest, in MoversIn) (*mcp.CallToolResult, ItemsOut, error) {
	h, err := s.houseByName(ctx, in.House)
	if err != nil {
		return nil, ItemsOut{}, err
	}
	rows, err := s.buildRows(ctx, h.ID)
	if err != nil {
		return nil, ItemsOut{}, err
	}
	minLo := in.MinSoldLo
	if minLo == 0 {
		minLo = 5
	}
	var hits []ItemRow
	for _, r := range rows {
		if r.SoldLo < minLo || (in.Class != "" && !strings.EqualFold(r.Class, in.Class)) ||
			(in.MaxReqLevel > 0 && r.ReqLevel > in.MaxReqLevel) || (in.MaxItemLevel > 0 && r.ItemLevel > in.MaxItemLevel) {
			continue
		}
		hits = append(hits, r)
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].SoldEst*float64(hits[i].Fair) > hits[j].SoldEst*float64(hits[j].Fair)
	})
	out := ItemsOut{House: h.Faction, Total: len(hits)}
	for _, r := range hits[:min(len(hits), clampLimit(in.Limit, 25, 100))] {
		out.Items = append(out.Items, toMCP(r))
	}
	return nil, out, nil
}

type FlipsIn struct {
	Gold  float64 `json:"gold,omitempty" jsonschema:"how much gold the player has to invest, default 40"`
	Style string  `json:"style,omitempty" jsonschema:"active (checks about hourly), casual (default: 4-5 times a day; regular is accepted too) or away (back once or twice a day; holds only)"`
	Risk  string  `json:"risk,omitempty" jsonschema:"deprecated, use style: low, medium or high map to away, casual or active"`
	Limit int     `json:"limit,omitempty" jsonschema:"default 15, max 50"`
}

func (s *Server) toolFlips(ctx context.Context, _ *mcp.CallToolRequest, in FlipsIn) (*mcp.CallToolResult, *flipsResult, error) {
	style := in.Style
	if style == "" {
		style = in.Risk // the old parameter, still accepted like ?risk= on /api/flips
	}
	res, err := s.flips(ctx, int64(in.Gold*10000), style, clampLimit(in.Limit, 15, 50))
	return nil, res, err
}

type StatusIn struct{}

type StatusOut struct {
	Houses   []House  `json:"houses"`
	Problems []string `json:"problems"`
}

func (s *Server) toolStatus(ctx context.Context, _ *mcp.CallToolRequest, _ StatusIn) (*mcp.CallToolResult, StatusOut, error) {
	hs, err := s.houseList(ctx)
	if err != nil {
		return nil, StatusOut{}, err
	}
	st := collector.LoadStatus(s.Cfg.StatusPath())
	probs := st.Problems(s.Cfg.Alert.AfterFail, s.Cfg.StaleAfter, time.Now())
	if probs == nil {
		probs = []string{}
	}
	return nil, StatusOut{Houses: hs, Problems: probs}, nil
}

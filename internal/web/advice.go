package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/report"
)

func (s *Server) adviceHouse(h House) report.AdviceHouse {
	return report.AdviceHouse{ID: h.ID, Faction: h.Faction, Cut: h.Cut, PriceOnly: h.ViewOnly}
}

// tradeHouse resolves a house for the advisor, which reads tracked auctions: a price-only
// house has none, so its verdicts would all be computed from an empty order book.
func (s *Server) tradeHouse(ctx context.Context, house string) (House, error) {
	h, err := s.houseByName(ctx, house)
	if err == nil && h.ViewOnly {
		err = errors.New(h.Faction + " is a view-only house (prices only, no auction tracking); ask about a tradeable house")
	}
	return h, err
}

func (s *Server) adviceCfg() report.AdviceConfig {
	return report.AdviceConfig{AmbiguousWeight: s.Cfg.AmbiguousWeight, DepositRate: s.Cfg.DepositRate, Now: time.Now()}
}

// The advisor operations, shared by the HTTP API and the MCP tools.

func (s *Server) priceCheck(ctx context.Context, house, item, price string) (*report.PriceCheck, error) {
	h, err := s.tradeHouse(ctx, house)
	if err != nil {
		return nil, err
	}
	p, err := report.ParseGold(price)
	if err != nil {
		return nil, err
	}
	ic, err := report.LoadItemContext(ctx, s.Store, s.adviceHouse(h), s.adviceCfg(), item, "")
	if err != nil {
		return nil, err
	}
	return report.CheckPrice(ic, p), nil
}

func (s *Server) sellOrHold(ctx context.Context, house, item string, qty int64) (*report.SellAdvice, error) {
	h, err := s.tradeHouse(ctx, house)
	if err != nil {
		return nil, err
	}
	ic, err := report.LoadItemContext(ctx, s.Store, s.adviceHouse(h), s.adviceCfg(), item, "")
	if err != nil {
		return nil, err
	}
	var typMin, typListed int64
	var best string
	if a, err := report.AnalyzeSell(ctx, s.Store, report.SellOptions{
		Options: report.Options{HouseID: h.ID, HouseName: h.Name, Cut: h.Cut, DepositRate: s.Cfg.DepositRate, Days: 14, Now: time.Now()},
		Item:    strconv.FormatInt(ic.ItemID, 10), BlockHours: 3, AmbiguousWeight: s.Cfg.AmbiguousWeight,
	}); err == nil {
		typMin, typListed, best = a.Now.TypicalMin, a.Now.TypicalListed, a.Best
	}
	return report.AdviseSell(ic, qty, h.Cut, typMin, typListed, best), nil
}

func (s *Server) cancelOrRide(ctx context.Context, house, item, price string, qty int64, timeLeft string, duration float64) (*report.CancelAdvice, error) {
	h, err := s.tradeHouse(ctx, house)
	if err != nil {
		return nil, err
	}
	p, err := report.ParseGold(price)
	if err != nil {
		return nil, err
	}
	ic, err := report.LoadItemContext(ctx, s.Store, s.adviceHouse(h), s.adviceCfg(), item, "")
	if err != nil {
		return nil, err
	}
	return report.AdviseCancel(ic, p, qty, strings.ToUpper(timeLeft), h.Cut, duration), nil
}

type flipsResult struct {
	Flips      []report.Flip                 `json:"flips"`
	Confidence string                        `json:"confidence"`
	Funnel     map[string]*report.FlipFunnel `json:"funnel" jsonschema:"per route: how many items each rule rejected"`
}

func (s *Server) flips(ctx context.Context, limit int, minProfit int64) (*flipsResult, error) {
	hs, err := s.houseList(ctx)
	if err != nil {
		return nil, err
	}
	var ahs []report.AdviceHouse
	for _, h := range hs {
		ahs = append(ahs, s.adviceHouse(h))
	}
	funnel := map[string]*report.FlipFunnel{}
	f, conf, err := report.FindFlips(ctx, s.Store, ahs, s.adviceCfg(), report.FlipOptions{Limit: limit, MinProfit: minProfit, Funnel: funnel})
	if err != nil {
		return nil, err
	}
	if f == nil {
		f = []report.Flip{}
	}
	return &flipsResult{Flips: f, Confidence: conf, Funnel: funnel}, nil
}

// HTTP handlers

func qint(r *http.Request, k string, def int64) int64 {
	if v, err := strconv.ParseInt(r.URL.Query().Get(k), 10, 64); err == nil && v > 0 {
		return v
	}
	return def
}

func (s *Server) apiPriceCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.priceCheck(r.Context(), q.Get("house"), q.Get("item"), q.Get("price"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) apiSell(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.sellOrHold(r.Context(), q.Get("house"), q.Get("item"), qint(r, "qty", 1))
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) apiCancel(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.cancelOrRide(r.Context(), q.Get("house"), q.Get("item"), q.Get("price"), qint(r, "qty", 1), q.Get("time_left"), float64(qint(r, "duration", 24)))
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) apiFlips(w http.ResponseWriter, r *http.Request) {
	res, err := s.flips(r.Context(), int(qint(r, "limit", 15)), qint(r, "min_profit", 20000))
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, res)
}

type suggestion struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Quality int    `json:"quality"`
}

// apiSuggest powers item autocomplete: names containing the query, most traded first.
func (s *Server) apiSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := []suggestion{}
	if len(q) < 2 {
		writeJSON(w, out)
		return
	}
	h, err := s.houseParam(r)
	if err != nil {
		writeJSON(w, out)
		return
	}
	rows, err := s.buildRows(r.Context(), h.ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	seen := map[int64]bool{}
	type scored struct {
		sg    suggestion
		score float64
	}
	var hits []scored
	for _, row := range rows {
		name := strings.ToLower(row.Name)
		if seen[row.ID] || !strings.Contains(name, q) {
			continue
		}
		seen[row.ID] = true
		score := row.SoldEst*float64(row.Fair) + float64(row.Available)
		if strings.HasPrefix(name, q) {
			score *= 10
		}
		hits = append(hits, scored{suggestion{row.ID, row.Name, row.Quality}, score})
	}
	for i := 0; i < len(hits) && i < 8; i++ {
		best := i
		for j := i + 1; j < len(hits); j++ {
			if hits[j].score > hits[best].score {
				best = j
			}
		}
		hits[i], hits[best] = hits[best], hits[i]
		out = append(out, hits[i].sg)
	}
	writeJSON(w, out)
}

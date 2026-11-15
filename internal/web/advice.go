package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/report"
)

// Flips are planned per request from inputs the collector precomputes after each publish, so
// a request only applies the player's gold: no database scans.

type flipsResult struct {
	Flips      []report.Flip                 `json:"flips"`
	Gold       int64                         `json:"gold"`
	Risk       report.Risk                   `json:"risk" jsonschema:"low, medium or high: how long gold may sit in stock"`
	ComputedAt int64                         `json:"computed_at"`
	Confidence string                        `json:"confidence" jsonschema:"early (<3 days of history), fair (<10 days) or good"`
	Funnel     map[string]*report.FlipFunnel `json:"funnel" jsonschema:"per route: how many items each rule rejected"`
	Alliance   string                        `json:"alliance" jsonschema:"tracked, price_only or missing: whether Alliance sales can be judged"`
}

// flipInputs returns the stored inputs, decoding them only when they change.
func (s *Server) flipInputs(ctx context.Context) (*report.FlipInputs, error) {
	t, err := s.Store.FlipCacheTime(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.flipCache != nil && s.flipCache.ComputedAt == t {
		in := s.flipCache
		s.mu.Unlock()
		return in, nil
	}
	s.mu.Unlock()
	if t == 0 {
		return nil, errors.New("flips aren't computed yet: they appear after the collector's first scan")
	}
	_, b, err := s.Store.LoadFlipCache(ctx)
	if err != nil {
		return nil, err
	}
	in := &report.FlipInputs{}
	if err := json.Unmarshal(b, in); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.flipCache = in
	s.mu.Unlock()
	return in, nil
}

func (s *Server) flips(ctx context.Context, gold int64, risk string, limit int) (*flipsResult, error) {
	in, err := s.flipInputs(ctx)
	if err != nil {
		return nil, err
	}
	f, funnel, gold := report.PlanFlips(in, report.FlipOptions{Gold: gold, Risk: report.ParseRisk(risk), Limit: limit})
	if f == nil {
		f = []report.Flip{}
	}
	ally := "missing"
	for _, h := range in.Houses {
		if h.Faction == "alliance" {
			ally = "price_only"
			if h.Tracked {
				ally = "tracked"
			}
		}
	}
	return &flipsResult{Flips: f, Gold: gold, Risk: report.ParseRisk(risk), ComputedAt: in.ComputedAt, Confidence: in.Confidence(), Funnel: funnel, Alliance: ally}, nil
}

func (s *Server) apiFlips(w http.ResponseWriter, r *http.Request) {
	gold, _ := strconv.ParseFloat(r.URL.Query().Get("gold"), 64)
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 15
	}
	start := time.Now()
	res, err := s.flips(r.Context(), int64(gold*10000), r.URL.Query().Get("risk"), max(1, min(limit, 100)))
	if err != nil {
		fail(w, 503, err)
		return
	}
	w.Header().Set("Server-Timing", "plan;dur="+strconv.FormatInt(time.Since(start).Milliseconds(), 10))
	writeJSON(w, res)
}

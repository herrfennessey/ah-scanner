package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/supply"
)

// supplyHistoryDays is how far back the Supply charts reach.
const supplyHistoryDays = 30

// apiSupply serves the Supply page as precomputed after the latest Horde scan, plus the chart
// history. Before the collector's first refresh it prices the current order book itself.
func (s *Server) apiSupply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h, err := s.houseByName(ctx, "horde")
	if err != nil {
		fail(w, 500, err)
		return
	}
	_, view, err := s.Store.LoadSupplyCache(ctx)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if view == nil {
		v, err := s.supplyNow(ctx, h.ID, h.LastScan)
		if err != nil {
			fail(w, 500, err)
			return
		}
		if view, err = json.Marshal(v); err != nil {
			fail(w, 500, err)
			return
		}
	}
	hist, err := s.Store.SupplyPoints(ctx, h.ID, time.Now().Unix()-supplyHistoryDays*86400)
	if err != nil {
		fail(w, 500, err)
		return
	}
	// The cached view is already JSON: splice the history in rather than decode and re-encode it.
	hb, err := json.Marshal(hist)
	if err != nil {
		fail(w, 500, err)
		return
	}
	view = bytes.TrimRight(view, " \n")
	out := append(append(append([]byte{}, view[:len(view)-1]...), `,"history":`...), hb...)
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(out, '}'))
}

// supplyNow prices the plan from the open order book at the house's latest scan.
func (s *Server) supplyNow(ctx context.Context, houseID, lastScan int64) (*supply.View, error) {
	src, err := supply.Load(ctx, s.Store.DB, houseID, lastScan, supply.Pouch(s.Cfg.Supply.CoinPouch))
	if err != nil {
		return nil, err
	}
	return src.ViewAt(lastScan), nil
}

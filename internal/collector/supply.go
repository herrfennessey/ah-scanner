package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/supply"
)

// SupplyHouse is the house the Supply page prices from: the tracked Horde house, or 0.
func (c *Collector) SupplyHouse() int64 {
	for _, h := range c.Houses {
		if h.Faction == "horde" && !h.ViewOnly && !h.PriceOnly {
			return h.ID
		}
	}
	return 0
}

// RefreshSupply precomputes the Supply page at the latest Horde scan and adds a chart point for
// every scan since the last one. The first run backfills the charts from the auction history:
// each scan's order book is rebuilt from when listings appeared, sold down and went.
func (c *Collector) RefreshSupply(ctx context.Context) error {
	house := c.SupplyHouse()
	if house == 0 {
		return nil
	}
	start := time.Now()
	last, err := c.Store.LastSupplyPoint(ctx, house)
	if err != nil {
		return err
	}
	// Redo the last hour too: a scan's gear and commodity halves can land in separate batches.
	since := max(last-3600, 0)
	// Older order books can't be rebuilt: closed auctions are deleted after the retention
	// (0 keeps them forever).
	if days := c.Cfg.AuctionRetentionDays; days > 0 {
		since = max(since, start.Unix()-int64(days)*86400)
	}
	times, err := supply.ScanTimes(ctx, c.Store.DB, house, since)
	if err != nil {
		return fmt.Errorf("supply scans: %w", err)
	}
	if len(times) == 0 {
		return nil
	}
	src, err := supply.Load(ctx, c.Store.DB, house, since, supply.Pouch(c.Cfg.Supply.CoinPouch))
	if err != nil {
		return fmt.Errorf("supply: %w", err)
	}
	points := map[int64][]byte{}
	var plan supply.Result
	for _, t := range times {
		plan = supply.Plan(src.At(t))
		b, err := json.Marshal(supply.PointOf(t, plan))
		if err != nil {
			return err
		}
		points[t] = b
	}
	if err := c.Store.ReplaceSupplyPoints(ctx, house, since, points); err != nil {
		return fmt.Errorf("supply points: %w", err)
	}
	now := times[len(times)-1]
	b, err := json.Marshal(src.ViewOf(now, plan)) // plan is the last scan's
	if err != nil {
		return err
	}
	if err := c.Store.SaveSupplyCache(ctx, now, b); err != nil {
		return fmt.Errorf("supply cache: %w", err)
	}
	c.Log.Info("supply refreshed", "points", len(points), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

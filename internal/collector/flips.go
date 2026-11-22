package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

// FlipHouses maps the resolved houses to what the flip planner needs.
func (c *Collector) FlipHouses() []report.FlipHouse {
	var out []report.FlipHouse
	for _, h := range c.Houses {
		out = append(out, report.FlipHouse{ID: h.ID, Faction: h.Faction, Cut: h.Cut, DepositMult: h.Deposit, ViewOnly: h.ViewOnly, Tracked: !h.PriceOnly})
	}
	return out
}

// RefreshAfterPublish brings today's daily rows up to date and precomputes the flip inputs and
// the Supply page, so the pages and MCP answer instantly from fresh data. Runs after every batch
// with new data.
func (c *Collector) RefreshAfterPublish(ctx context.Context) error {
	start := time.Now()
	local := start.In(rollup.Berlin)
	if _, err := rollup.Range(ctx, c.Store.DB, local, local, rollup.Options{AmbiguousWeight: c.Cfg.AmbiguousWeight}); err != nil {
		return fmt.Errorf("rollup: %w", err)
	}
	in, err := report.BuildFlipInputs(ctx, c.Store, c.FlipHouses(), c.Cfg.DepositRate, time.Now())
	if err != nil {
		return fmt.Errorf("flip inputs: %w", err)
	}
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("flip inputs: %w", err)
	}
	if err := c.Store.SaveFlipCache(ctx, in.ComputedAt, b); err != nil {
		return fmt.Errorf("flip cache: %w", err)
	}
	days := max(0, c.Cfg.FlipHistoryDays)
	if days > 0 {
		// Kept per scan so the backtest replays what the planner saw instead of rebuilding it.
		if err := c.Store.SaveFlipHistory(ctx, in.ComputedAt, store.FlipHistoryLive, report.InputsVersion, report.CompressInputs(b)); err != nil {
			return fmt.Errorf("flip history: %w", err)
		}
	}
	// Pruned even at 0 days, so turning the history off drops what was kept.
	cutoff := in.ComputedAt - int64(days)*86400
	if days == 0 {
		cutoff = in.ComputedAt + 1
	}
	if _, err := c.Store.PruneFlipHistory(ctx, cutoff); err != nil {
		return fmt.Errorf("flip history: %w", err)
	}
	c.Log.Info("flips refreshed", "items", len(in.Items), "bytes", len(b), "took", time.Since(start).Round(time.Millisecond))
	return c.RefreshSupply(ctx)
}

// refresh runs RefreshAfterPublish from the collect loops, where a failure is only logged.
func (c *Collector) refresh(ctx context.Context) {
	if err := c.RefreshAfterPublish(ctx); err != nil {
		c.Log.Error("refresh after publish", "err", err)
	}
}

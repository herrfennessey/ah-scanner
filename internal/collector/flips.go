package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
)

// FlipHouses maps the resolved houses to what the flip planner needs.
func (c *Collector) FlipHouses() []report.FlipHouse {
	var out []report.FlipHouse
	for _, h := range c.Houses {
		out = append(out, report.FlipHouse{ID: h.ID, Faction: h.Faction, Cut: h.Cut, ViewOnly: h.ViewOnly, Tracked: !h.PriceOnly})
	}
	return out
}

// RefreshAfterPublish brings today's daily rows up to date and precomputes the flip inputs, so
// the Flips page and MCP answer instantly from fresh data. Runs after every batch with new data.
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
	c.Log.Info("flips refreshed", "items", len(in.Items), "bytes", len(b), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// refresh runs RefreshAfterPublish from the collect loops, where a failure is only logged.
func (c *Collector) refresh(ctx context.Context) {
	if err := c.RefreshAfterPublish(ctx); err != nil {
		c.Log.Error("refresh after publish", "err", err)
	}
}

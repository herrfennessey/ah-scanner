package collector

import (
	"context"
	"encoding/json"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
)

// FlipHouses maps the resolved houses to what the flip planner needs.
func (c *Collector) FlipHouses() []report.FlipHouse {
	var out []report.FlipHouse
	for _, h := range c.Houses {
		cut := 0.05
		for _, ch := range c.Cfg.Houses {
			if ch.ID == h.ID || (ch.ID == 0 && ch.Faction == h.Faction) {
				cut = ch.AHCut()
			}
		}
		out = append(out, report.FlipHouse{ID: h.ID, Faction: h.Faction, Cut: cut, ViewOnly: h.ViewOnly, Tracked: !h.PriceOnly})
	}
	return out
}

// RefreshAfterPublish brings today's daily rows up to date and precomputes the flip inputs, so
// the Flips page and MCP answer instantly from fresh data. Runs after every batch with new data.
func (c *Collector) RefreshAfterPublish(ctx context.Context) {
	start := time.Now()
	local := time.Now().In(rollup.Berlin)
	if _, err := rollup.Range(ctx, c.Store.DB, local, local, rollup.Options{AmbiguousWeight: c.Cfg.AmbiguousWeight}); err != nil {
		c.Log.Error("rollup", "err", err)
		return
	}
	c.lastRollupHour = local.Format("2006-01-02T15")
	in, err := report.BuildFlipInputs(ctx, c.Store, c.FlipHouses(), c.Cfg.DepositRate, time.Now())
	if err != nil {
		c.Log.Error("flip inputs", "err", err)
		return
	}
	b, err := json.Marshal(in)
	if err != nil {
		c.Log.Error("flip inputs", "err", err)
		return
	}
	if err := c.Store.SaveFlipCache(ctx, in.ComputedAt, b); err != nil {
		c.Log.Error("flip cache", "err", err)
		return
	}
	c.Log.Info("flips refreshed", "items", len(in.Items), "bytes", len(b), "took", time.Since(start).Round(time.Millisecond))
}

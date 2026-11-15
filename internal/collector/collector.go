// Package collector runs the fetch → parse → track → aggregate cycle on a schedule and
// does the nightly rollup, pruning and backup.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/blizzard"
	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

type House struct {
	ID        int64
	Name      string
	Faction   string
	PriceOnly bool
	ViewOnly  bool
	Cut       float64 // AH cut on sales
}

type Collector struct {
	Cfg    config.Config
	Store  *store.Store
	API    *blizzard.Client
	Log    *slog.Logger
	Houses []House
	Status *Status
}

var kinds = []listing.Kind{listing.Gear, listing.Commodity}

// ResolveHouses reads the index and maps each configured house to a live house.
// A configured id must exist and have the configured faction. Without an id, the faction
// must match exactly one house; several matches are an error listing the candidates.
func ResolveHouses(index []blizzard.HouseRef, want []config.House) ([]House, error) {
	var out []House
	for _, w := range want {
		var cands []blizzard.HouseRef
		for _, h := range index {
			if w.ID != 0 && h.ID != w.ID {
				continue
			}
			if h.Faction != w.Faction {
				continue
			}
			if w.Name != "" && !strings.EqualFold(h.Name, w.Name) {
				continue
			}
			cands = append(cands, h)
		}
		switch {
		case len(cands) == 1:
			out = append(out, House{ID: cands[0].ID, Name: cands[0].Name, Faction: w.Faction, PriceOnly: w.PriceOnly, ViewOnly: !w.Tradeable(), Cut: w.AHCut()})
		case len(cands) == 0 && w.ID != 0:
			return nil, fmt.Errorf("house id %d (%s) not in the index, or its faction changed", w.ID, w.Faction)
		case len(cands) == 0:
			return nil, fmt.Errorf("no %s house in the index", w.Faction)
		default:
			ids := make([]string, len(cands))
			for i, c := range cands {
				ids[i] = fmt.Sprint(c.ID)
			}
			return nil, fmt.Errorf("%d %s houses in the index (ids %s); set houses[].id in config.yaml (run `ahc probe` to compare them)",
				len(cands), w.Faction, strings.Join(ids, ", "))
		}
	}
	return out, nil
}

// Setup resolves houses against the live index.
func (c *Collector) Setup(ctx context.Context) error {
	idx, _, err := c.API.Index(ctx, c.Cfg.RulesetID, c.Cfg.DynamicNamespace)
	if err != nil {
		return fmt.Errorf("house index: %w", err)
	}
	houses, err := ResolveHouses(idx, c.Cfg.Houses)
	if err != nil {
		return err
	}
	for _, h := range houses {
		if err := c.Store.UpsertHouse(ctx, h.ID, h.Name, h.Faction); err != nil {
			return err
		}
		c.Log.Info("house resolved", "id", h.ID, "name", h.Name, "faction", h.Faction, "price_only", h.PriceOnly, "view_only", h.ViewOnly)
	}
	c.Houses = houses
	// Drop feeds of houses no longer configured, or they stay stale in status.json forever and
	// keep `ahc health` and the alert failing.
	c.Status.KeepFeeds(houses)
	// Daily rows rolled up before the sales columns existed read as zero sales: recompute the
	// last week once so flips have evidence straight away. Recorded in the database, so it runs
	// exactly once whichever process opened the database first.
	const backfillKey = "backfill.item_daily.sales"
	if done, err := c.Store.Meta(ctx, backfillKey); err != nil {
		return err
	} else if done == "" {
		now := time.Now().In(rollup.Berlin)
		// Like `ahc rollup`: stay a day inside retention, or the oldest day is rebuilt from
		// partly pruned detail; 0 means keep forever.
		days := 7
		for _, keep := range []int{c.Cfg.AuctionRetentionDays, c.Cfg.SnapshotFullDays} {
			if keep > 0 {
				days = min(days, keep-1)
			}
		}
		days = max(days, 1)
		c.Log.Info("backfilling daily sales counts", "days", days)
		if _, err := rollup.Range(ctx, c.Store.DB, now.AddDate(0, 0, -(days-1)), now, rollup.Options{AmbiguousWeight: c.Cfg.AmbiguousWeight}); err != nil {
			c.Log.Error("backfill", "err", err)
		} else if err := c.Store.SetMeta(ctx, backfillKey, now.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

// Run loops forever on the cron schedules in config: a collect cycle (± jitter) and the
// nightly job. A nightly run missed while the process was down is caught up at startup.
func (c *Collector) Run(ctx context.Context) error {
	nightly, err := config.ParseCron(c.Cfg.Schedule.Nightly)
	if err != nil {
		return fmt.Errorf("schedule.nightly: %w", err)
	}
	c.collectCycle(ctx)
	if last := c.Status.LastMaintenance; last.IsZero() || time.Since(last) > 26*time.Hour {
		c.Log.Info("nightly job overdue, running it now", "last", last)
		c.Nightly(ctx, time.Now())
	}
	if FollowEnabled(c.Cfg) {
		return c.runFollow(ctx, nightly)
	}
	collect, err := config.ParseCron(c.Cfg.Schedule.Collect)
	if err != nil {
		return fmt.Errorf("schedule.collect: %w", err)
	}
	nextCollect := c.jittered(collect.Next(time.Now().In(rollup.Berlin)))
	nextNightly := nightly.Next(time.Now().In(rollup.Berlin))
	for {
		next, job := nextCollect, "collect"
		if nextNightly.Before(next) {
			next, job = nextNightly, "nightly"
		}
		c.Log.Info("sleeping", "job", job, "at", next.In(rollup.Berlin).Format(time.DateTime))
		if err := waitUntil(ctx, next); err != nil {
			return err
		}
		now := time.Now().In(rollup.Berlin)
		if job == "nightly" {
			c.Nightly(ctx, now)
			nextNightly = nightly.Next(now)
		} else {
			c.collectCycle(ctx)
			nextCollect = c.jittered(collect.Next(now))
		}
	}
}

// jittered offsets a scheduled time by a random ± Jitter, never into the past.
func (c *Collector) jittered(t time.Time) time.Time {
	if c.Cfg.Jitter <= 0 {
		return t
	}
	t = t.Add(time.Duration(rand.Int63n(int64(2*c.Cfg.Jitter))) - c.Cfg.Jitter)
	if now := time.Now(); t.Before(now) {
		return now
	}
	return t
}

func (c *Collector) collectCycle(ctx context.Context) {
	if err := c.Cycle(ctx); err != nil {
		c.Log.Error("cycle failed", "err", err)
	}
	c.refresh(ctx)
}

// Cycle fetches every house and kind once. It returns an error if any fetch failed.
func (c *Collector) Cycle(ctx context.Context) error {
	start := time.Now()
	reqs := c.API.Requests.Load()
	var errs []error
	for _, h := range c.Houses {
		for _, k := range kinds {
			res, err := c.fetchAndApply(ctx, h, k)
			c.Status.Record(h, k, res, err)
			if err != nil {
				c.Log.Error("snapshot failed", "house", h.ID, "kind", k, "err", err)
				errs = append(errs, fmt.Errorf("%d/%s: %w", h.ID, k, err))
			}
		}
	}
	c.lookupItems(ctx)
	c.pruneRaw()
	err := errors.Join(errs...)
	c.Status.EndCycle(err)
	if err := c.Status.Save(c.Cfg.StatusPath()); err != nil {
		c.Log.Warn("status.json", "err", err)
	}
	c.maybeAlert(ctx)
	c.Log.Info("cycle done", "took", time.Since(start).Round(time.Millisecond), "requests", c.API.Requests.Load()-reqs, "failed", len(errs))
	return err
}

type Result struct {
	LastModified int64  // server Last-Modified of the body, when one was received
	Outcome      string // applied | not_modified | identical
	NAuctions    int
	New, Seen    int
	Units        map[string]int64
}

func (c *Collector) fetchAndApply(ctx context.Context, h House, kind listing.Kind) (Result, error) {
	var res Result
	last, ok, err := c.Store.LastSnapshot(ctx, h.ID, kind)
	if err != nil {
		return res, err
	}
	ims := ""
	if ok && last.LastModified > 0 {
		ims = time.Unix(last.LastModified, 0).UTC().Format(http.TimeFormat)
	}
	fetchedAt := time.Now().Unix()
	dst := filepath.Join(c.Cfg.RawDir(), fmt.Sprintf("%d-%s-%d.json.zst", h.ID, kind, fetchedAt))
	fr, err := c.API.FetchToFile(ctx, blizzard.AuctionsPath(c.Cfg.RulesetID, h.ID, kind == listing.Commodity),
		urlNS(c.Cfg.DynamicNamespace), ims, dst)
	if err != nil {
		return res, err
	}
	if fr.NotModified {
		res.Outcome = "not_modified"
		return res, nil
	}
	if dup, err := c.Store.HasSnapshotHash(ctx, h.ID, kind, fr.SHA256); err != nil {
		return res, err
	} else if dup {
		os.Remove(fr.Path)
		res.Outcome = "identical"
		res.LastModified = blizzard.ParseHTTPTime(fr.LastModified)
		return res, nil
	}

	cur, err := parseRaw(fr.Path, kind)
	if errors.Is(err, listing.ErrNoAuctions) {
		// Empty houses omit the array. Only believe that if we had nothing open before.
		n, cerr := c.Store.OpenCount(ctx, h.ID, kind)
		if cerr != nil {
			return res, cerr
		}
		if n > 100 {
			return res, fmt.Errorf("house returned no auctions but %d were open; refusing to close them all (raw: %s)", n, fr.Path)
		}
		err = nil
	}
	if err != nil {
		return res, fmt.Errorf("parse %s: %w", fr.Path, err)
	}
	sn := store.Snapshot{
		HouseID: h.ID, Kind: kind, FetchedAt: fetchedAt,
		LastModified: blizzard.ParseHTTPTime(fr.LastModified), SHA256: fr.SHA256,
	}
	if h.PriceOnly {
		if _, err := c.Store.ApplyPriceSnapshot(ctx, sn, cur); err != nil {
			return res, err
		}
		c.Log.Info("price snapshot applied", "house", h.ID, "kind", kind, "auctions", len(cur))
		return Result{Outcome: "applied", NAuctions: len(cur), LastModified: sn.LastModified}, nil
	}
	ar, err := c.Store.ApplySnapshot(ctx, sn, cur)
	if err != nil {
		return res, err
	}
	res = Result{Outcome: "applied", NAuctions: len(cur), New: ar.New, Seen: ar.Seen, Units: ar.Events, LastModified: sn.LastModified}
	c.Log.Info("snapshot applied", "house", h.ID, "kind", kind, "auctions", len(cur), "new", ar.New,
		"sold", ar.Events["sold"]+ar.Events["partial"], "cancelled", ar.Events["cancelled"], "expired_or_sold", ar.Events["expired_or_sold"])
	return res, nil
}

func parseRaw(path string, kind listing.Kind) ([]listing.Listing, error) {
	r, err := blizzard.OpenRaw(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return listing.ParseAll(r, kind)
}

func (c *Collector) lookupItems(ctx context.Context) {
	if c.Cfg.ItemLookupsPerCycle <= 0 {
		return
	}
	ids, err := c.Store.ItemsNeedingNames(ctx, c.Cfg.ItemLookupsPerCycle, 24*time.Hour)
	if err != nil {
		c.Log.Warn("item lookup query", "err", err)
		return
	}
	var found atomic.Int32
	work := make(chan int64)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				it, err := c.API.Item(ctx, id, c.Cfg.StaticNamespaces)
				if err != nil && !blizzard.IsNotFound(err) {
					c.Log.Warn("item lookup", "item", id, "err", err)
				}
				if it.Name != "" {
					found.Add(1)
				}
				if err := c.Store.SetItem(ctx, it); err != nil {
					c.Log.Warn("item store", "item", id, "err", err)
				}
			}
		}()
	}
	for _, id := range ids {
		work <- id
	}
	close(work)
	wg.Wait()
	if len(ids) > 0 {
		c.Log.Info("item names", "looked_up", len(ids), "found", found.Load())
	}
}

func (c *Collector) pruneRaw() {
	entries, err := os.ReadDir(c.Cfg.RawDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-c.Cfg.RawRetention)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(c.Cfg.RawDir(), e.Name()))
		}
	}
}

// Nightly rolls up the last three days, applies retention and writes a compressed backup.
func (c *Collector) Nightly(ctx context.Context, now time.Time) {
	local := now.In(rollup.Berlin)
	t := time.Now()
	if n, err := rollup.Range(ctx, c.Store.DB, local.AddDate(0, 0, -2), local, rollup.Options{AmbiguousWeight: c.Cfg.AmbiguousWeight}); err != nil {
		c.Log.Error("nightly rollup", "err", err)
	} else {
		c.Log.Info("nightly rollup", "rows", n, "took", time.Since(t).Round(time.Millisecond))
	}
	t = time.Now()
	deleted, err := c.Store.Retain(ctx, now, store.Retention{
		AuctionDays: c.Cfg.AuctionRetentionDays, FullDays: c.Cfg.SnapshotFullDays,
		ThinHours: c.Cfg.SnapshotThinHours, SnapshotDays: c.Cfg.SnapshotMaxDays,
	})
	if err != nil {
		c.Log.Error("retention", "err", err)
	} else {
		c.Log.Info("retention applied", "deleted", deleted, "took", time.Since(t).Round(time.Millisecond))
	}
	t = time.Now()
	if p, err := c.Store.Backup(ctx, c.Cfg.BackupDir(), local, c.Cfg.BackupKeep); err != nil {
		c.Log.Error("backup", "err", err)
	} else {
		c.Log.Info("backup written", "path", p, "took", time.Since(t).Round(time.Millisecond))
	}
	c.Status.SetMaintenance(now)
	if err := c.Status.Save(c.Cfg.StatusPath()); err != nil {
		c.Log.Warn("status.json", "err", err)
	}
}

func (c *Collector) maybeAlert(ctx context.Context) {
	if c.Cfg.Alert.URL == "" {
		return
	}
	msg := c.Status.AlertMessage(c.Cfg.Alert.AfterFail, c.Cfg.StaleAfter, time.Now())
	if msg == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Cfg.Alert.URL, strings.NewReader(msg))
	if err != nil {
		c.Status.UndoAlert()
		return
	}
	req.Header.Set("Title", "ahc collector")
	// A hung alert endpoint must not stall the collect loop.
	resp, err := alertClient.Do(req)
	if err != nil {
		c.Log.Warn("alert failed, will retry next cycle", "err", err)
		c.Status.UndoAlert()
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		c.Log.Warn("alert rejected, will retry next cycle", "status", resp.StatusCode)
		c.Status.UndoAlert()
	}
}

var alertClient = &http.Client{Timeout: 15 * time.Second}

// waitUntil sleeps until the wall-clock time t. It waits in chunks of at most 30 s and re-reads
// the clock each time: Go timers run on a monotonic clock that stops while the host is
// suspended, so one long timer would fire late by however long the machine slept.
func waitUntil(ctx context.Context, t time.Time) error {
	for {
		d := time.Until(t)
		if d <= 0 {
			return nil
		}
		tm := time.NewTimer(min(d, 30*time.Second))
		select {
		case <-ctx.Done():
			tm.Stop()
			return ctx.Err()
		case <-tm.C:
		}
	}
}

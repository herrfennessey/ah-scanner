package collector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
)

// Follow mode: instead of polling on a fixed clock, poll each feed just after Blizzard is
// expected to publish it. Houses publish hourly at a stable second past the hour (observed
// :18:49–:18:56), which each feed learns from its own Last-Modified history.

const hour = int64(3600)

// learnOffset returns the typical publish second past the hour from past Last-Modified times,
// or def when there is no history. It is a circular median, so offsets near :00 don't split.
func learnOffset(lms []int64, def int64) int64 {
	if len(lms) == 0 {
		return def
	}
	ref := lms[len(lms)-1] % hour
	rel := make([]int64, len(lms))
	for i, lm := range lms {
		d := (lm%hour - ref + hour + hour/2) % hour // in [0, hour), centred on ref
		rel[i] = d - hour/2
	}
	sort.Slice(rel, func(i, j int) bool { return rel[i] < rel[j] })
	return (ref + rel[len(rel)/2] + hour) % hour
}

// nextPublish is the first expected publish strictly after the last one we have (lastLM),
// given the offset past the hour. With no data yet (lastLM 0) the feed is due immediately.
func nextPublish(lastLM, offset int64) int64 {
	if lastLM <= 0 {
		return 0
	}
	t := (lastLM-offset)/hour*hour + offset
	for t <= lastLM+60 { // a minute of slack: the same publish can carry a few seconds' jitter
		t += hour
	}
	return t
}

// feedState is where one feed is in its publish cycle.
type feedState struct {
	house    House
	kind     listing.Kind
	offset   int64 // learned publish second past the hour
	lastLM   int64 // Last-Modified of the newest snapshot we have
	expected int64 // next expected publish (Unix seconds; 0 = now)
	nextPoll time.Time
	lastErr  error
}

type followParams struct {
	lead, retry, window, catchUp time.Duration
}

func (p followParams) defaults() followParams {
	if p.lead <= 0 {
		p.lead = 15 * time.Second
	}
	if p.retry <= 0 {
		p.retry = 30 * time.Second
	}
	if p.window <= 0 {
		p.window = 10 * time.Minute
	}
	if p.catchUp <= 0 {
		p.catchUp = 15 * time.Minute
	}
	return p
}

// schedule sets nextPoll after a poll. fresh means the server published new data.
func (f *feedState) schedule(now time.Time, fresh bool, p followParams) {
	if fresh || f.expected == 0 {
		f.expected = nextPublish(f.lastLM, f.offset)
		f.nextPoll = time.Unix(f.expected, 0).Add(p.lead)
		if !f.nextPoll.After(now) {
			f.nextPoll = now.Add(p.retry)
		}
		return
	}
	late := now.Sub(time.Unix(f.expected, 0))
	switch {
	case late < p.window:
		f.nextPoll = now.Add(p.retry) // publish due any moment: keep checking
	default:
		f.nextPoll = now.Add(p.catchUp) // late or moved: poll slowly until it shows up
	}
}

// inWindow reports whether the feed is waiting for an overdue publish within the window.
func (f *feedState) inWindow(now time.Time, p followParams) bool {
	return f.expected > 0 && now.Unix() >= f.expected && now.Sub(time.Unix(f.expected, 0)) < p.window
}

func (c *Collector) followParams() followParams {
	return followParams{lead: c.Cfg.Follow.Lead, retry: c.Cfg.Follow.Retry, window: c.Cfg.Follow.Window, catchUp: c.Cfg.Follow.CatchUp}.defaults()
}

// initFollow builds feed states from the database: last Last-Modified and learned offset.
func (c *Collector) initFollow(ctx context.Context) ([]*feedState, error) {
	since := time.Now().Add(-48 * time.Hour).Unix()
	hist, err := c.Store.PublishTimes(ctx, since)
	if err != nil {
		return nil, err
	}
	def := int64(c.Cfg.Follow.DefaultOffset / time.Second)
	var feeds []*feedState
	for _, h := range c.Houses {
		for _, k := range kinds {
			f := &feedState{house: h, kind: k}
			lms := hist[fmt.Sprintf("%d/%s", h.ID, k)]
			f.offset = learnOffset(lms, def)
			if len(lms) > 0 {
				f.lastLM = lms[len(lms)-1]
			}
			f.schedule(time.Now(), true, c.followParams())
			feeds = append(feeds, f)
		}
	}
	return feeds, nil
}

// runFollow is Run for schedule.collect: follow.
func (c *Collector) runFollow(ctx context.Context, nightly interface{ Next(time.Time) time.Time }) error {
	p := c.followParams()
	feeds, err := c.initFollow(ctx)
	if err != nil {
		return err
	}
	nextNightly := nightly.Next(time.Now().In(rollup.Berlin))
	slotErrs := map[string]error{} // last error per feed in the current publish slot
	slotPolled := false
	relearn := time.Now().Add(time.Hour)

	for {
		next := nextNightly
		for _, f := range feeds {
			if f.nextPoll.Before(next) {
				next = f.nextPoll
			}
		}
		c.Log.Info("sleeping", "until", next.In(rollup.Berlin).Format(time.DateTime))
		if err := waitUntil(ctx, next); err != nil {
			return err
		}
		now := time.Now()
		if !nextNightly.After(now) {
			c.Nightly(ctx, now)
			nextNightly = nightly.Next(now.In(rollup.Berlin))
			continue
		}

		// Poll every feed that is due, in one batch.
		start := now
		reqs := c.API.Requests.Load()
		applied := false
		for _, f := range feeds {
			if f.nextPoll.After(now) {
				continue
			}
			slotPolled = true
			res, err := c.fetchAndApply(ctx, f.house, f.kind)
			c.Status.Record(f.house, f.kind, res, err)
			key := FeedKey(f.house, f.kind)
			fresh := err == nil && (res.Outcome == "applied" || res.Outcome == "identical")
			if err != nil {
				c.Log.Error("snapshot failed", "house", f.house.ID, "kind", f.kind, "err", err)
				slotErrs[key] = err
			} else {
				delete(slotErrs, key)
			}
			if fresh {
				applied = applied || res.Outcome == "applied"
				if res.LastModified > 0 {
					f.lastLM = res.LastModified
					c.Status.RecordLag(f.house, f.kind, time.Now().Unix()-res.LastModified)
				} else {
					f.lastLM = time.Now().Unix()
				}
			}
			f.schedule(time.Now(), fresh, p)
		}
		if applied {
			c.lookupItems(ctx)
			c.hourlyRollup(ctx)
		}
		c.pruneRaw()

		// A publish slot is settled once no feed is still waiting inside its window: count it
		// as one cycle for failure alerts (retries every 30 s must not trip them).
		waiting := false
		for _, f := range feeds {
			if f.inWindow(time.Now(), p) {
				waiting = true
			}
		}
		if slotPolled && !waiting {
			var errs []error
			for k, e := range slotErrs {
				errs = append(errs, fmt.Errorf("%s: %w", k, e))
			}
			c.Status.EndCycle(errors.Join(errs...))
			slotErrs, slotPolled = map[string]error{}, false
		}
		if err := c.Status.Save(c.Cfg.StatusPath()); err != nil {
			c.Log.Warn("status.json", "err", err)
		}
		c.maybeAlert(ctx)
		if applied {
			c.Log.Info("publish caught", "took", time.Since(start).Round(time.Millisecond), "requests", c.API.Requests.Load()-reqs)
		}

		// Re-learn offsets hourly so a moved publish time is picked up.
		if time.Now().After(relearn) {
			relearn = time.Now().Add(time.Hour)
			if hist, err := c.Store.PublishTimes(ctx, time.Now().Add(-48*time.Hour).Unix()); err == nil {
				def := int64(c.Cfg.Follow.DefaultOffset / time.Second)
				for _, f := range feeds {
					if o := learnOffset(hist[fmt.Sprintf("%d/%s", f.house.ID, f.kind)], def); o != f.offset {
						c.Log.Info("publish time re-learned", "house", f.house.ID, "kind", f.kind,
							"was", fmtOffset(f.offset), "now", fmtOffset(o))
						f.offset = o
					}
				}
			}
		}
	}
}

func fmtOffset(o int64) string { return fmt.Sprintf(":%02d:%02d", o/60, o%60) }

// FollowEnabled reports whether the collect schedule is the publish-following mode.
func FollowEnabled(cfg config.Config) bool { return config.IsFollow(cfg.Schedule.Collect) }

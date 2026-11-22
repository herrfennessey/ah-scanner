package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/backtest"
	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

func runBacktest(ctx context.Context, cfg config.Config, st *store.Store, f *flags) error {
	now := time.Now()
	// The sell rates need auction.front; databases from before it was tracked lack it.
	if n, err := st.BackfillFront(ctx, now.AddDate(0, 0, -f.days-4).Unix()); err != nil {
		return err
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "ahc: marked %d older auctions as cheapest-when-posted or not\n", n)
	}
	// And gear "sales" from before tracker rules 4c and 4d (a cheaper copy outlasted it, or an
	// identical one replaced it): reclassify the window and roll its days up again, as the
	// collector does once at startup.
	// Never further back than the detail retention, or good daily rows would be overwritten.
	days := cfg.RollupDays(f.days + 2)
	from := now.AddDate(0, 0, -(days - 1))
	fromDay, _, _ := rollup.DayBounds(rollup.DayString(from, rollup.Berlin), rollup.Berlin)
	if n, err := st.ReclassifyGearSales(ctx, fromDay); err != nil {
		return err
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "ahc: %d gear sales were cancels by rules 4c/4d; rolling up again\n", n)
		if _, err := rollup.Range(ctx, st.DB, from, now, rollup.Options{AmbiguousWeight: cfg.AmbiguousWeight}); err != nil {
			return err
		}
	}
	// Rebuilding inputs allocates heavily; collecting garbage less often is ~10% faster.
	debug.SetGCPercent(400)
	// A read-only pool beside the writer, so missing inputs rebuild on several cores at once.
	read, err := store.OpenReadOnly(cfg.DBPath(), f.workers)
	if err != nil {
		return err
	}
	defer read.Close()
	start := time.Now()
	trades, err := backtest.Run(ctx, st, backtest.Options{From: now.AddDate(0, 0, -f.days), To: now,
		Every: f.every, Gold: int64(f.gold * 10000), DepositRate: cfg.DepositRate, QueueScale: f.queue,
		Inputs: f.inputs, Read: read, Workers: f.workers,
		Log: func(format string, args ...any) { fmt.Fprintf(os.Stderr, "ahc: "+format+"\n", args...) }})
	fmt.Fprintf(os.Stderr, "ahc: backtest took %s\n", time.Since(start).Round(time.Second))
	if err != nil {
		return err
	}
	if f.csv {
		w := csv.NewWriter(os.Stdout)
		w.Write([]string{"at", "style", "play", "item", "kind", "buy_house", "sell_house", "units", "cost", "sell_at", "expected", "sold", "cash", "marked", "floor", "hours", "relists"})
		for _, t := range trades {
			x := t.Flip
			w.Write([]string{time.Unix(t.At, 0).UTC().Format(time.RFC3339), string(t.Style), string(x.Play), x.Name, x.Kind, x.BuyHouse, x.SellHouse,
				fmt.Sprint(x.BuyUnits), fmt.Sprint(x.BuyCost), fmt.Sprint(x.SellAt), fmt.Sprint(x.Expected),
				fmt.Sprint(t.Sold), fmt.Sprint(t.Cash), fmt.Sprint(t.Marked), fmt.Sprint(t.Floor), fmt.Sprintf("%.1f", t.Hours), fmt.Sprint(t.Relists)})
		}
		w.Flush()
		return w.Error()
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "style\tplay\ttrades\tcost\tpromised\tcash\tmarked\tunits sold\tsold out\tlosing\tmedian h\t\n")
	for _, s := range backtest.Summarize(trades) {
		play := string(s.Play)
		if play == "" {
			play = "all"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.0f%%\t%.1f\t\n", s.Style, play, s.Trades, report.Gold(s.Cost), report.Gold(s.Expected),
			report.Gold(s.Cash), report.Gold(s.Marked), 100*s.UnitsSold, 100*s.AllSold, 100*s.Losing, s.MedianHold)
	}
	tw.Flush()
	// The items that cost the most, per style.
	byItem := map[string]int64{}
	for _, t := range trades {
		byItem[string(t.Style)+"\t"+t.Flip.Name] += t.Marked
	}
	type kv struct {
		k string
		v int64
	}
	var all []kv
	for k, v := range byItem {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })
	fmt.Println("\nWorst items (marked):")
	for i := 0; i < len(all) && i < 10 && all[i].v < 0; i++ {
		fmt.Printf("  %s\t%s\n", all[i].k, report.Gold(all[i].v))
	}
	return nil
}

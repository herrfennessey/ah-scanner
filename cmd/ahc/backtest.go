package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
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
	// And gear "sales" from before tracker rule 4c (a cheaper copy outlasted it): reclassify the
	// window and roll its days up again, as the collector does once at startup.
	from := now.AddDate(0, 0, -f.days-1)
	fromDay, _, _ := rollup.DayBounds(rollup.DayString(from, rollup.Berlin), rollup.Berlin)
	if n, err := st.ReclassifyGearSales(ctx, fromDay); err != nil {
		return err
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "ahc: %d gear sales were cancels by rule 4c; rolling up again\n", n)
		if _, err := rollup.Range(ctx, st.DB, from, now, rollup.Options{AmbiguousWeight: cfg.AmbiguousWeight}); err != nil {
			return err
		}
	}
	trades, err := backtest.Run(ctx, st, backtest.Options{From: now.AddDate(0, 0, -f.days), To: now,
		Hold: f.hold, Gold: int64(f.gold * 10000), DepositRate: cfg.DepositRate})
	if err != nil {
		return err
	}
	if f.csv {
		w := csv.NewWriter(os.Stdout)
		w.Write([]string{"at", "risk", "item", "kind", "buy_house", "sell_house", "units", "cost", "sell_at", "expected", "sold", "cash", "marked", "floor", "hours"})
		for _, t := range trades {
			x := t.Flip
			w.Write([]string{time.Unix(t.At, 0).UTC().Format(time.RFC3339), string(t.Risk), x.Name, x.Kind, x.BuyHouse, x.SellHouse,
				fmt.Sprint(x.BuyUnits), fmt.Sprint(x.BuyCost), fmt.Sprint(x.SellAt), fmt.Sprint(x.Expected),
				fmt.Sprint(t.Sold), fmt.Sprint(t.Cash), fmt.Sprint(t.Marked), fmt.Sprint(t.Floor), fmt.Sprintf("%.1f", t.Hours)})
		}
		w.Flush()
		return w.Error()
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "risk\ttrades\tcost\tpromised\tcash\tmarked\tunits sold\tsold out\tlosing\tmedian h\t\n")
	for _, s := range backtest.Summarize(trades) {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%.0f%%\t%.0f%%\t%.0f%%\t%.1f\t\n", s.Risk, s.Trades, report.Gold(s.Cost), report.Gold(s.Expected),
			report.Gold(s.Cash), report.Gold(s.Marked), 100*s.UnitsSold, 100*s.AllSold, 100*s.Losing, s.MedianHold)
	}
	tw.Flush()
	// The items that cost the most, per risk level.
	byItem := map[string]int64{}
	for _, t := range trades {
		byItem[string(t.Risk)+"\t"+t.Flip.Name] += t.Marked
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

// flipdiag rebuilds the flip inputs at past scans and prints what PlanFlips offers and why
// items were rejected, per route. Build it from two checkouts to compare planner versions.
//
//	go run ./research/flipdiag -db data/ahc.db -hours 48 -every 4 -gold 100
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

func main() {
	db := flag.String("db", "data/ahc.db", "database")
	hours := flag.Int("hours", 48, "look back this many hours")
	every := flag.Int("every", 4, "plan every n hours")
	gold := flag.Float64("gold", 100, "gold")
	rate := flag.Float64("deposit-rate", 0.2, "deposit rate")
	out := flag.String("json", "", "write per-time results as JSON here")
	item := flag.Int64("item", 0, "only this item: print its inputs and why each route rejected it")
	flag.Parse()
	ctx := context.Background()
	st, err := store.Open(*db)
	if err != nil {
		panic(err)
	}
	houses, err := report.CachedHouses(ctx, st)
	if err != nil {
		panic(err)
	}
	var last sql.NullInt64
	if err := st.DB.QueryRowContext(ctx, `SELECT MAX(fetched_at) FROM snapshot`).Scan(&last); err != nil {
		panic(err)
	}
	if !last.Valid {
		panic("no snapshots in " + *db)
	}
	type row struct {
		At     string                        `json:"at"`
		Style  string                        `json:"style"`
		Flips  []report.Flip                 `json:"flips"`
		Funnel map[string]*report.FlipFunnel `json:"funnel"`
	}
	var all []row
	for h := *hours; h >= 0; h -= *every {
		at := time.Unix(last.Int64, 0).Add(-time.Duration(h) * time.Hour)
		in, err := report.BuildFlipInputsAt(ctx, st, houses, *rate, at)
		if err != nil {
			panic(err)
		}
		if *item != 0 {
			var keep []report.FlipItem
			for _, it := range in.Items {
				if it.Item == *item {
					keep = append(keep, it)
				}
			}
			in.Items = keep
			for _, it := range keep {
				for f, s := range it.Houses {
					fmt.Printf("%s %s %-10s fair %d min %d clearing %d trend %.3f sales %d/day %.1f robust %.0f volatile %v evidence %v book %v\n",
						at.UTC().Format("01-02 15:04"), it.Name, f, s.Fair, s.Min, s.Clearing, s.Trend, s.Sales, s.SalesPerDay, s.RobustPerDay, s.Volatile, s.Evidence, s.Book[:min(len(s.Book), 3)])
				}
			}
		}
		for _, s := range []report.Style{report.StyleCasual, report.StyleActive} {
			f, fn, _ := report.PlanFlips(in, report.FlipOptions{Gold: int64(*gold * 10000), Style: s})
			all = append(all, row{at.UTC().Format("01-02 15:04"), string(s), f, fn})
			var exp int64
			byRoute := map[string]int{}
			for _, x := range f {
				exp += x.Expected
				byRoute[x.BuyHouse+"→"+x.SellHouse]++
			}
			keys := make([]string, 0, len(byRoute))
			for k := range byRoute {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Printf("%s %-6s flips %3d expected %6.1fg ", at.UTC().Format("01-02 15:04"), s, len(f), float64(exp)/1e4)
			for _, k := range keys {
				fmt.Printf(" %s:%d", k, byRoute[k])
			}
			fmt.Println()
			if *item != 0 {
				for r, fn := range fn {
					if fn.Candidates > 0 {
						fmt.Printf("    %s %+v\n", r, *fn)
					}
				}
			}
		}
	}
	if *out != "" {
		b, err := json.Marshal(all)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			panic(err)
		}
	}
}

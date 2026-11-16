// Command ahc collects WoW Forever auction house snapshots and answers questions about them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo; reports use Europe/Berlin

	"github.com/herrfennessey/ah-scanner/internal/blizzard"
	"github.com/herrfennessey/ah-scanner/internal/collector"
	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
	"github.com/herrfennessey/ah-scanner/internal/version"
	"github.com/herrfennessey/ah-scanner/internal/web"
)

const usage = `ahc — WoW Forever auction house collector

Collecting:
  ahc collect            run forever on config schedules (collect, nightly), serving the web UI and /mcp
  ahc once               run one collection cycle and today's rollup, then exit
  ahc probe [-all]       fetch the house index and raw samples, print the fields seen (writes no tables)
  ahc rollup [-days N]   recompute daily rows for the last N days (default 3)
  ahc serve              web UI only (reads the database the collector writes)
  ahc health             exit 1 if the collector is stale or failing (Docker healthcheck)
  ahc version            print the build version
  ahc replay -out x.db   rebuild a database from the raw bodies (last 48 h) with the current rules

Reports (common flags: -house horde|blackwater, -days N, -min-sold N, -csv, -limit N):
  ahc sell <item>        when and at what price to post an item
  ahc movers             what sells reliably
  ahc deals              what is listed below its fair price right now
  ahc item <item>        daily history of one item

<item> is an item ID or (part of) its name. Config: -config path (default $AHC_CONFIG or /config.yaml).
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "version" || cmd == "-version" || cmd == "--version" {
		fmt.Println(version.String())
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cmd, args); err != nil {
		fmt.Fprintln(os.Stderr, "ahc:", err)
		os.Exit(1)
	}
}

type flags struct {
	fs                     *flag.FlagSet
	config, house, variant string
	days, limit            int
	minSold, minDisc       float64
	csv, all, volatile     bool
	out                    string
}

func parseFlags(cmd string, args []string, defDays int, defMinSold float64) (*flags, []string, error) {
	f := &flags{fs: flag.NewFlagSet(cmd, flag.ContinueOnError)}
	def := os.Getenv("AHC_CONFIG")
	if def == "" {
		def = "/config.yaml"
	}
	f.fs.StringVar(&f.config, "config", def, "config file")
	f.fs.StringVar(&f.house, "house", "horde", "horde | blackwater | alliance, or a house id")
	f.fs.StringVar(&f.variant, "variant", "", "item variant key (random suffix / bonus list)")
	f.fs.IntVar(&f.days, "days", defDays, "days of history")
	f.fs.IntVar(&f.limit, "limit", 50, "max rows")
	f.fs.Float64Var(&f.minSold, "min-sold", defMinSold, "minimum sold per day")
	f.fs.Float64Var(&f.minDisc, "min-discount", 0.30, "deals: minimum discount to fair price")
	f.fs.BoolVar(&f.csv, "csv", false, "CSV output")
	f.fs.BoolVar(&f.all, "all", false, "probe: every house in the index, not just configured ones")
	f.fs.BoolVar(&f.volatile, "include-volatile", false, "deals: include volatile items")
	f.fs.StringVar(&f.out, "out", "", "replay: path of the database to build")
	// Allow flags after positional args: ahc sell "Heavy Quiver" -house blackwater
	var pos []string
	rest := args
	for len(rest) > 0 {
		if err := f.fs.Parse(rest); err != nil {
			return nil, nil, err
		}
		rest = f.fs.Args()
		if len(rest) > 0 {
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
	}
	return f, pos, nil
}

func run(ctx context.Context, cmd string, args []string) error {
	if cmd == "collect" || cmd == "once" || cmd == "serve" {
		// First, so a build that fails on config or the database still says which it was.
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Info("ahc starting", "version", version.String(), "cmd", cmd)
	}
	defaults := map[string][2]float64{"movers": {7, 20}, "deals": {7, 5}, "item": {14, 0}, "sell": {14, 0}, "rollup": {3, 0}}
	d := defaults[cmd]
	if d[0] == 0 {
		d[0] = 7
	}
	f, pos, err := parseFlags(cmd, args, int(d[0]), d[1])
	if err != nil {
		return err
	}
	cfg, err := config.Load(f.config)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if cmd != "collect" && cmd != "once" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}

	switch cmd {
	case "health":
		st := collector.LoadStatus(cfg.StatusPath())
		if probs := st.Problems(cfg.Alert.AfterFail, cfg.StaleAfter, time.Now()); len(probs) > 0 {
			return errors.New(strings.Join(probs, "; "))
		}
		fmt.Println("ok")
		return nil
	case "probe":
		return probe(ctx, cfg, f.all)
	case "replay":
		return replay(ctx, cfg, f.out)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	switch cmd {
	case "collect", "once":
		if err := cfg.RequireCredentials(); err != nil {
			return err
		}
		status := collector.LoadStatus(cfg.StatusPath())
		status.Version = version.String()
		// Now, not after the first cycle, so the Collector page shows the new build at once.
		if err := status.Save(cfg.StatusPath()); err != nil {
			log.Warn("status", "err", err)
		}
		c := &collector.Collector{Cfg: cfg, Store: st, Log: log, Status: status,
			API: blizzard.New(cfg.APIBase, cfg.TokenURL, cfg.ClientID, cfg.ClientSecret, cfg.Locale, log)}
		if err := c.Setup(ctx); err != nil {
			return err
		}
		if cmd == "once" {
			err := c.Cycle(ctx)
			return errors.Join(err, c.RefreshAfterPublish(ctx)) // today's rollup and the flip inputs
		}
		if cfg.Listen != "" {
			// Its own connection pool: the collector's single connection is held for whole
			// snapshot transactions and the nightly VACUUM INTO, which would stall the UI and /mcp.
			webStore, err := store.Open(cfg.DBPath())
			if err != nil {
				return err
			}
			defer webStore.Close()
			srv := &web.Server{Cfg: cfg, Store: webStore, Log: log}
			go func() {
				if err := srv.ListenAndServe(ctx, cfg.Listen); err != nil {
					log.Error("web UI stopped", "err", err)
				}
			}()
		}
		err := c.Run(ctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case "serve":
		addr := cfg.Listen
		if addr == "" {
			addr = ":4653"
		}
		return (&web.Server{Cfg: cfg, Store: st, Log: log}).ListenAndServe(ctx, addr)
	case "rollup":
		// Days older than the detail retention would be rebuilt from pruned auctions and thinned
		// snapshots, overwriting good item_daily rows (which are kept forever) with undercounts.
		days := f.days
		for _, keep := range []int{cfg.AuctionRetentionDays, cfg.SnapshotFullDays} {
			if keep > 0 && days > keep-1 {
				days = keep - 1
			}
		}
		if days < 1 {
			days = 1
		}
		if days != f.days {
			fmt.Fprintf(os.Stderr, "ahc: rolling up %d days, not %d: older detail has been pruned by retention\n", days, f.days)
		}
		now := time.Now()
		n, err := rollup.Range(ctx, st.DB, now.AddDate(0, 0, -(days-1)), now, rollup.Options{AmbiguousWeight: cfg.AmbiguousWeight})
		if err == nil {
			fmt.Printf("rolled up %d item-days\n", n)
		}
		return err
	}

	opt, err := reportOptions(ctx, cfg, st, f)
	if err != nil {
		return err
	}
	switch cmd {
	case "movers":
		return report.Movers(ctx, st, opt)
	case "deals":
		return report.Deals(ctx, st, report.DealsOptions{Options: opt, MinDiscount: f.minDisc, IncludeVolatile: f.volatile})
	case "item", "sell":
		if len(pos) == 0 {
			return fmt.Errorf("usage: ahc %s <item id or name>", cmd)
		}
		q := strings.Join(pos, " ")
		if cmd == "item" {
			return report.Item(ctx, st, opt, q)
		}
		return report.Sell(ctx, st, report.SellOptions{Options: opt, Item: q, Variant: f.variant, BlockHours: 3, AmbiguousWeight: cfg.AmbiguousWeight})
	}
	fmt.Print(usage)
	return fmt.Errorf("unknown command %q", cmd)
}

func reportOptions(ctx context.Context, cfg config.Config, st *store.Store, f *flags) (report.Options, error) {
	o := report.Options{Days: f.days, MinSold: f.minSold, CSV: f.csv, Limit: f.limit, Out: os.Stdout, Now: time.Now(),
		DepositRate: cfg.DepositRate}
	var h config.House
	found := false
	for _, ch := range cfg.Houses {
		if ch.Faction == strings.ToLower(f.house) || fmt.Sprint(ch.ID) == f.house {
			h, found = ch, true
		}
	}
	if !found {
		return o, fmt.Errorf("house %q is not in config.yaml", f.house)
	}
	o.Cut = h.AHCut()
	o.HouseID = h.ID
	if o.HouseID == 0 {
		id, _, err := st.HouseByFaction(ctx, h.Faction)
		if err != nil {
			return o, err
		}
		o.HouseID = id
	}
	o.HouseName = strings.ToUpper(h.Faction[:1]) + h.Faction[1:]
	return o, nil
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/blizzard"
	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/listing"
)

// probe checks credentials and endpoints, saves raw samples and prints the field shapes seen,
// without touching the database. Run it first whenever the API might have changed.
func probe(ctx context.Context, cfg config.Config, all bool) error {
	if err := cfg.RequireCredentials(); err != nil {
		return err
	}
	api := blizzard.New(cfg.APIBase, cfg.TokenURL, cfg.ClientID, cfg.ClientSecret, cfg.Locale, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if _, err := api.Token(ctx); err != nil {
		return fmt.Errorf("token: %w", err)
	}
	fmt.Println("token: ok")

	idx, raw, err := api.Index(ctx, cfg.RulesetID, cfg.DynamicNamespace)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.SamplesDir(), 0o755); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(cfg.SamplesDir(), "index.json"), raw, 0o644)
	fmt.Printf("index: %d houses (ruleset %d, %s)\n", len(idx), cfg.RulesetID, cfg.DynamicNamespace)
	want := map[int64]bool{}
	for _, h := range cfg.Houses {
		want[h.ID] = true
	}
	for _, h := range idx {
		mark := ""
		if want[h.ID] {
			mark = "  ← configured"
		}
		fmt.Printf("  %4d  %-10s %s%s\n", h.ID, h.Faction, h.Name, mark)
	}

	var targets []blizzard.HouseRef
	for _, h := range idx {
		if all || want[h.ID] {
			targets = append(targets, h)
		}
	}
	for _, h := range targets {
		for _, kind := range []listing.Kind{listing.Gear, listing.Commodity} {
			dst := filepath.Join(cfg.SamplesDir(), fmt.Sprintf("house%d-%s.json.zst", h.ID, kind))
			path := blizzard.AuctionsPath(cfg.RulesetID, h.ID, kind == listing.Commodity)
			ns := url.Values{"namespace": {cfg.DynamicNamespace}}
			start := time.Now()
			fr, err := api.FetchToFile(ctx, path, ns, "", dst)
			if err != nil {
				fmt.Printf("\n%d %s: %v\n", h.ID, kind, err)
				continue
			}
			fmt.Printf("\n%d %s %s: %.1f MB in %s, Last-Modified %q\n", h.ID, h.Faction, kind, float64(fr.Bytes)/1e6, time.Since(start).Round(time.Millisecond), fr.LastModified)
			if fr.LastModified != "" {
				fr2, err := api.FetchToFile(ctx, path, url.Values{"namespace": {cfg.DynamicNamespace}}, fr.LastModified, dst+".ims")
				os.Remove(dst + ".ims")
				switch {
				case err != nil:
					fmt.Printf("  If-Modified-Since: error %v\n", err)
				case fr2.NotModified:
					fmt.Println("  If-Modified-Since: honoured (304)")
				default:
					fmt.Println("  If-Modified-Since: ignored (200); hash comparison will dedupe")
				}
			}
			if err := describe(fr.Path, kind); err != nil {
				fmt.Printf("  PARSE FAILED: %v\n", err)
			}
		}
	}
	it, err := api.Item(ctx, 7069, cfg.StaticNamespaces, false)
	if err != nil {
		fmt.Printf("\nitem lookup: %v\n", err)
	} else {
		fmt.Printf("\nitem lookup: %d = %q (%s, ilvl %d, icon %t)\n", it.ID, it.Name, it.Class, it.ItemLevel, it.Icon != "")
	}
	fmt.Printf("requests: %d, samples in %s\n", api.Requests.Load(), cfg.SamplesDir())
	return nil
}

// describe prints field paths and counts, and checks the strict parser accepts the body.
func describe(path string, kind listing.Kind) error {
	r, err := blizzard.OpenRaw(path)
	if err != nil {
		return err
	}
	defer r.Close()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return err
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("  top-level keys: %s\n", strings.Join(keys, ", "))
	var auctions []map[string]any
	if a, ok := body["auctions"]; ok {
		if err := json.Unmarshal(a, &auctions); err != nil {
			return err
		}
	}
	counts := map[string]int{}
	tl := map[string]int{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				walk(prefix+"."+k, vv)
			}
		case []any:
			counts[prefix+"[]"]++
		default:
			counts[prefix]++
		}
	}
	for _, a := range auctions {
		walk("", a)
		if s, ok := a["time_left"].(string); ok {
			tl[s]++
		}
	}
	paths := make([]string, 0, len(counts))
	for p := range counts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	fmt.Printf("  %d auctions; fields:\n", len(auctions))
	for _, p := range paths {
		fmt.Printf("    %-28s %d\n", strings.TrimPrefix(p, "."), counts[p])
	}
	fmt.Printf("  time_left: %v\n", tl)

	r2, err := blizzard.OpenRaw(path)
	if err != nil {
		return err
	}
	defer r2.Close()
	n, err := listing.Parse(r2, kind, func(listing.Listing) error { return nil })
	if err == listing.ErrNoAuctions {
		fmt.Println("  parser: empty house (no auctions array)")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("  parser: ok, %d listings\n", n)
	_, _ = io.Copy(io.Discard, r2)
	return nil
}

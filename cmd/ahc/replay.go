package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/blizzard"
	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

// replay rebuilds a database from the raw bodies in raw/ (kept raw_retention), so changes to
// the tracker rules can be checked against real data. Houses and item metadata are copied
// from the live database; nothing in it is modified.
func replay(ctx context.Context, cfg config.Config, out string) error {
	if out == "" {
		return errors.New("usage: ahc replay -out new.db")
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s exists; pick a new path", out)
	}
	type rawFile struct {
		path  string
		house int64
		kind  listing.Kind
		at    int64
	}
	entries, err := os.ReadDir(cfg.RawDir())
	if err != nil {
		return err
	}
	var files []rawFile
	for _, e := range entries {
		// <house>-<kind>-<unix>.json.zst
		parts := strings.SplitN(strings.TrimSuffix(e.Name(), ".json.zst"), "-", 3)
		if len(parts) != 3 || !strings.HasSuffix(e.Name(), ".json.zst") {
			continue
		}
		h, err1 := strconv.ParseInt(parts[0], 10, 64)
		at, err2 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		files = append(files, rawFile{filepath.Join(cfg.RawDir(), e.Name()), h, listing.Kind(parts[1]), at})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at < files[j].at })
	if len(files) == 0 {
		return fmt.Errorf("no raw bodies in %s", cfg.RawDir())
	}

	st, err := store.Open(out)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.DB.ExecContext(ctx, `ATTACH DATABASE ? AS live`, "file:"+cfg.DBPath()+"?mode=ro"); err != nil {
		return err
	}
	for _, q := range []string{`INSERT INTO house SELECT * FROM live.house`, `INSERT INTO item SELECT * FROM live.item`} {
		if _, err := st.DB.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("copy from live db: %w", err)
		}
	}
	st.DB.ExecContext(ctx, `DETACH DATABASE live`)

	priceOnly := map[int64]bool{}
	for _, h := range cfg.Houses {
		priceOnly[h.ID] = h.PriceOnly
	}
	var first, last time.Time
	for _, f := range files {
		r, err := blizzard.OpenRaw(f.path)
		if err != nil {
			return err
		}
		h := sha256.New()
		cur, err := listing.ParseAll(io.TeeReader(r, h), f.kind)
		r.Close()
		if errors.Is(err, listing.ErrNoAuctions) {
			err = nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f.path, err)
		}
		sn := store.Snapshot{HouseID: f.house, Kind: f.kind, FetchedAt: f.at, SHA256: hex.EncodeToString(h.Sum(nil))}
		if dup, _ := st.HasSnapshotHash(ctx, f.house, f.kind, sn.SHA256); dup {
			continue
		}
		if first.IsZero() {
			first = time.Unix(f.at, 0)
		}
		last = time.Unix(f.at, 0)
		if priceOnly[f.house] {
			// Same as the live collector: no auction tracking for view-only houses.
			if _, err := st.ApplyPriceSnapshot(ctx, sn, cur); err != nil {
				return err
			}
			continue
		}
		res, err := st.ApplySnapshot(ctx, sn, cur)
		if err != nil {
			return err
		}
		fmt.Printf("%s %d %-9s %6d auctions, %6d new | units: sold %d, partial %d, unsold %d, cancelled %d, expired-or-sold %d\n",
			time.Unix(f.at, 0).In(rollup.Berlin).Format("15:04"), f.house, f.kind, len(cur), res.New,
			res.Events["sold"], res.Events["partial"], res.Events["unsold"], res.Events["cancelled"], res.Events["expired_or_sold"])
	}
	n, err := rollup.Range(ctx, st.DB, first, last, rollup.Options{AmbiguousWeight: cfg.AmbiguousWeight})
	if err != nil {
		return err
	}
	fmt.Printf("rolled up %d item-days into %s\n", n, out)
	return nil
}

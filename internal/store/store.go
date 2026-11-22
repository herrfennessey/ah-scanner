// Package store owns the SQLite database: schema, snapshot application and maintenance.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/aggregate"
	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/tracker"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the database in WAL mode so readers never block the collector.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(OFF)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; WAL lets other processes read concurrently
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if _, err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db}, nil
}

// OpenReadOnly opens an existing database for reading only, with up to conns connections, so
// several goroutines can query it at once (WAL lets them read beside the writer). No schema or
// migrations are applied.
func OpenReadOnly(path string, conns int) (*Store, error) {
	// Memory-mapped reads: pages come from the OS file cache with no read call each, shared by
	// every connection (each connection's own page cache is only 2 MB by default).
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)&_pragma=mmap_size(17179869184)&_pragma=cache_size(-65536)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(max(1, conns))
	db.SetMaxIdleConns(max(1, conns))
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) UpsertHouse(ctx context.Context, id int64, name, faction string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO house (id, name, faction) VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, faction=excluded.faction`, id, name, faction)
	return err
}

// HouseByFaction returns the stored house for a faction (lowest id if several).
func (s *Store) HouseByFaction(ctx context.Context, faction string) (int64, string, error) {
	var id int64
	var name string
	err := s.DB.QueryRowContext(ctx, `SELECT id, name FROM house WHERE faction=? ORDER BY id LIMIT 1`, faction).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("no %s house in the database yet (has the collector run?)", faction)
	}
	return id, name, err
}

type SnapshotInfo struct {
	ID           int64
	FetchedAt    int64
	LastModified int64
	SHA256       string
}

// LastSnapshot returns the newest applied snapshot for a house and kind, or ok=false.
func (s *Store) LastSnapshot(ctx context.Context, houseID int64, kind listing.Kind) (SnapshotInfo, bool, error) {
	var si SnapshotInfo
	err := s.DB.QueryRowContext(ctx, `SELECT id, fetched_at, COALESCE(last_modified,0), body_sha256 FROM snapshot
		WHERE house_id=? AND kind=? ORDER BY fetched_at DESC, id DESC LIMIT 1`, houseID, string(kind)).
		Scan(&si.ID, &si.FetchedAt, &si.LastModified, &si.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return si, false, nil
	}
	return si, err == nil, err
}

// HasSnapshotHash reports whether a body with this hash was already applied.
func (s *Store) HasSnapshotHash(ctx context.Context, houseID int64, kind listing.Kind, sha string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM snapshot WHERE house_id=? AND kind=? AND body_sha256=?`,
		houseID, string(kind), sha).Scan(&n)
	return n > 0, err
}

// Snapshot describes one fetched body about to be applied.
type Snapshot struct {
	HouseID      int64
	Kind         listing.Kind
	FetchedAt    int64
	LastModified int64
	SHA256       string
}

// DataTime is the time the data reflects: Last-Modified when the server sent one.
func (sn Snapshot) DataTime() int64 {
	if sn.LastModified > 0 {
		return sn.LastModified
	}
	return sn.FetchedAt
}

type ApplyResult struct {
	SnapshotID int64
	New, Seen  int
	Events     map[string]int64 // units by outcome
}

// ApplySnapshot records a snapshot, diffs it against open auctions and writes per-item stats,
// all in one transaction.
func (s *Store) ApplySnapshot(ctx context.Context, sn Snapshot, cur []listing.Listing) (ApplyResult, error) {
	res := ApplyResult{Events: map[string]int64{}}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	kind := string(sn.Kind)
	now := sn.DataTime()

	var prevTime int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(last_modified,0), fetched_at) FROM snapshot
		WHERE house_id=? AND kind=? ORDER BY fetched_at DESC, id DESC LIMIT 1`, sn.HouseID, kind).Scan(&prevTime)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}

	r, err := tx.ExecContext(ctx, `INSERT INTO snapshot (house_id, kind, fetched_at, last_modified, n_auctions, body_sha256)
		VALUES (?,?,?,?,?,?)`, sn.HouseID, kind, sn.FetchedAt, sn.LastModified, len(cur), sn.SHA256)
	if err != nil {
		return res, fmt.Errorf("insert snapshot: %w", err)
	}
	res.SnapshotID, _ = r.LastInsertId()

	open, err := loadOpen(ctx, tx, sn.HouseID, kind, prevTime)
	if err != nil {
		return res, err
	}
	diff := tracker.Diff(sn.Kind, open, cur, now)
	res.New, res.Seen = len(diff.New), len(diff.Seen)

	// The cheapest price of each item in this snapshot: a new listing at it is the front.
	cheapest := map[[2]any]int64{}
	for _, l := range cur {
		k := [2]any{l.ItemID, l.Variant}
		if m, ok := cheapest[k]; l.UnitPrice > 0 && (!ok || l.UnitPrice < m) {
			cheapest[k] = l.UnitPrice
		}
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO auction (house_id, kind, auction_id, item_id, variant, unit_price, buyout, bid,
		qty_first, qty_last, first_seen, last_seen, tl_first, tl_last, front) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(house_id, kind, auction_id) DO UPDATE SET closed_at=NULL, outcome=NULL,
		qty_last=excluded.qty_last, last_seen=excluded.last_seen, tl_last=excluded.tl_last`)
	if err != nil {
		return res, err
	}
	defer ins.Close()
	for _, l := range diff.New {
		if _, err := ins.ExecContext(ctx, sn.HouseID, kind, l.AuctionID, l.ItemID, l.Variant, l.UnitPrice, l.Buyout, l.Bid,
			l.Quantity, l.Quantity, now, now, l.TimeLeft, l.TimeLeft, l.UnitPrice > 0 && l.UnitPrice <= cheapest[[2]any{l.ItemID, l.Variant}]); err != nil {
			return res, fmt.Errorf("insert auction: %w", err)
		}
	}

	upd, err := tx.PrepareContext(ctx, `UPDATE auction SET qty_last=?, tl_last=?, last_seen=? WHERE house_id=? AND kind=? AND auction_id=?`)
	if err != nil {
		return res, err
	}
	defer upd.Close()
	for _, l := range diff.Seen {
		o := open[l.AuctionID]
		if o.Qty == l.Quantity && o.TimeLeft == l.TimeLeft {
			continue // last_seen is implied by the snapshot; only write real changes
		}
		if _, err := upd.ExecContext(ctx, l.Quantity, l.TimeLeft, now, sn.HouseID, kind, l.AuctionID); err != nil {
			return res, err
		}
	}

	closeStmt, err := tx.PrepareContext(ctx, `UPDATE auction SET closed_at=?, outcome=?, last_seen=? WHERE house_id=? AND kind=? AND auction_id=?`)
	if err != nil {
		return res, err
	}
	defer closeStmt.Close()
	for id, outcome := range diff.Closed {
		if _, err := closeStmt.ExecContext(ctx, now, outcome, prevTime, sn.HouseID, kind, id); err != nil {
			return res, err
		}
	}

	ev, err := tx.PrepareContext(ctx, `INSERT INTO sale_event (house_id, kind, auction_id, item_id, variant, ts, outcome, units, unit_price)
		VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return res, err
	}
	defer ev.Close()
	for _, e := range diff.Events {
		res.Events[e.Outcome] += e.Units
		if e.Outcome != tracker.Partial {
			continue // closures live on the auction row
		}
		if _, err := ev.ExecContext(ctx, sn.HouseID, kind, e.AuctionID, e.ItemID, e.Variant, now, e.Outcome, e.Units, e.UnitPrice); err != nil {
			return res, err
		}
	}

	if err := writeItemStats(ctx, tx, res.SnapshotID, cur); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// ApplyPriceSnapshot records only a snapshot's per-item prices and supply, without tracking
// individual auctions. Used for houses we can't trade on (Alliance from a Horde character) but
// whose prices drive demand on the shared Blackwater house.
func (s *Store) ApplyPriceSnapshot(ctx context.Context, sn Snapshot, cur []listing.Listing) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `INSERT INTO snapshot (house_id, kind, fetched_at, last_modified, n_auctions, body_sha256)
		VALUES (?,?,?,?,?,?)`, sn.HouseID, string(sn.Kind), sn.FetchedAt, sn.LastModified, len(cur), sn.SHA256)
	if err != nil {
		return 0, fmt.Errorf("insert snapshot: %w", err)
	}
	id, _ := r.LastInsertId()
	if err := writeItemStats(ctx, tx, id, cur); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func writeItemStats(ctx context.Context, tx *sql.Tx, snapshotID int64, cur []listing.Listing) error {
	b := aggregate.NewBuilder()
	for _, l := range cur {
		b.Add(l)
	}
	st, err := tx.PrepareContext(ctx, `INSERT INTO item_snapshot (snapshot_id, item_id, variant, listed_qty, n_auctions, min_unit, p20_unit, median_unit)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, a := range b.Stats() {
		if _, err := st.ExecContext(ctx, snapshotID, a.ItemID, a.Variant, a.ListedQty, a.NAuctions,
			nullIfZero(a.MinUnit), nullIfZero(a.P20Unit), nullIfZero(a.MedianUnit)); err != nil {
			return err
		}
	}
	return nil
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func loadOpen(ctx context.Context, tx *sql.Tx, houseID int64, kind string, prevTime int64) (map[int64]tracker.Open, error) {
	rows, err := tx.QueryContext(ctx, `SELECT auction_id, item_id, variant, COALESCE(unit_price,0), qty_last, tl_last FROM auction
		WHERE house_id=? AND kind=? AND closed_at IS NULL`, houseID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]tracker.Open{}
	for rows.Next() {
		var o tracker.Open
		if err := rows.Scan(&o.AuctionID, &o.ItemID, &o.Variant, &o.UnitPrice, &o.Qty, &o.TimeLeft); err != nil {
			return nil, err
		}
		// Every open auction was present in the previous snapshot by construction.
		o.LastSeen = prevTime
		m[o.AuctionID] = o
	}
	return m, rows.Err()
}

// ItemsNeedingNames returns item IDs seen on the AH that lack a name, icon or tooltip and were not looked up recently.
func (s *Store) ItemsNeedingNames(ctx context.Context, limit int, retryAfter time.Duration) ([]int64, error) {
	cutoff := time.Now().Add(-retryAfter).Unix()
	// Items in tracked auctions, plus anything in the latest snapshot of any house (price-only
	// houses have no auction rows).
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT a.item_id FROM (
			SELECT item_id FROM auction WHERE closed_at IS NULL
			UNION SELECT item_id FROM item_snapshot WHERE snapshot_id IN (SELECT MAX(id) FROM snapshot GROUP BY house_id, kind)
		) a LEFT JOIN item i ON i.id = a.item_id
		WHERE (i.id IS NULL OR (i.name IS NOT NULL AND i.tooltip IS NULL)
			OR ((i.name IS NULL OR i.icon IS NULL OR i.looked_up_at IS NULL) AND COALESCE(i.looked_up_at,0) < ?))
		ORDER BY i.id IS NULL DESC, i.name IS NULL DESC, i.icon IS NULL DESC LIMIT ?`, cutoff, limit) // new items before the tooltip backfill
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type Item struct {
	ID        int64
	Name      string
	Quality   int
	Class     string
	Subclass  string
	SellPrice int64 // vendor sell price, copper; basis for the deposit estimate
	ItemLevel int
	ReqLevel  int
	Icon      string // icon URL from the media endpoint
	// Tooltip is the raw preview_item object from the item API: every tooltip line
	// (stats, binding, "Use:" text, recipe, sell price). Nil keeps what is stored.
	Tooltip json.RawMessage
}

// SetItem stores a resolved item. An empty name records a failed lookup.
func (s *Store) SetItem(ctx context.Context, it Item) error {
	var name any
	if it.Name != "" {
		name = it.Name
	}
	var quality any
	if it.Name != "" {
		quality = it.Quality
	}
	var tooltip any
	if len(it.Tooltip) > 0 {
		tooltip = string(it.Tooltip)
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO item (id, name, quality, class, subclass, sell_price, item_level, required_level, icon, tooltip, looked_up_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=COALESCE(excluded.name, item.name), quality=COALESCE(excluded.quality, item.quality),
		class=COALESCE(excluded.class, item.class), subclass=COALESCE(excluded.subclass, item.subclass),
		sell_price=COALESCE(excluded.sell_price, item.sell_price), item_level=COALESCE(excluded.item_level, item.item_level),
		required_level=COALESCE(excluded.required_level, item.required_level), icon=COALESCE(excluded.icon, item.icon),
		tooltip=COALESCE(excluded.tooltip, item.tooltip), looked_up_at=excluded.looked_up_at`,
		it.ID, name, quality, nullStr(it.Class), nullStr(it.Subclass), nullIfZero(it.SellPrice), nullIfZero(int64(it.ItemLevel)),
		nullIfZero(int64(it.ReqLevel)), nullStr(it.Icon), tooltip, time.Now().Unix())
	return err
}

// HaveIcons returns the IDs of items whose icon is already stored, so a tooltip backfill can
// skip the media request.
func (s *Store) HaveIcons(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM item WHERE icon IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		m[id] = true
	}
	return m, rows.Err()
}

// MissingTooltips returns those of ids with no stored tooltip (including unknown items), in order.
func (s *Store) MissingTooltips(ctx context.Context, ids []int64) ([]int64, error) {
	var out []int64
	for _, id := range ids {
		var has bool
		err := s.DB.QueryRowContext(ctx, `SELECT tooltip IS NOT NULL FROM item WHERE id=?`, id).Scan(&has)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if !has {
			out = append(out, id)
		}
	}
	return out, nil
}

// ItemTooltip returns an item's stored preview_item JSON, or nil if there is none.
func (s *Store) ItemTooltip(ctx context.Context, id int64) (json.RawMessage, error) {
	var t sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT tooltip FROM item WHERE id=?`, id).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !t.Valid {
		return nil, nil
	}
	return json.RawMessage(t.String), nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Retention describes how long detail is kept. Zero values disable that step.
type Retention struct {
	AuctionDays  int // closed auctions and partial-sale events
	FullDays     int // item_snapshot at full resolution
	ThinHours    int // then one snapshot per this many hours per house/kind
	SnapshotDays int // item_snapshot and snapshot rows dropped after this
}

// Retain deletes detail past its retention. item_daily is never touched.
func (s *Store) Retain(ctx context.Context, now time.Time, r Retention) (map[string]int64, error) {
	day := int64(86400)
	out := map[string]int64{}
	exec := func(name, q string, args ...any) error {
		res, err := s.DB.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		out[name], _ = res.RowsAffected()
		return nil
	}
	n := now.Unix()
	if r.AuctionDays > 0 {
		cut := n - int64(r.AuctionDays)*day
		if err := exec("auction", `DELETE FROM auction WHERE closed_at IS NOT NULL AND closed_at < ?`, cut); err != nil {
			return out, err
		}
		if err := exec("sale_event", `DELETE FROM sale_event WHERE ts < ?`, cut); err != nil {
			return out, err
		}
	}
	if r.SnapshotDays > 0 {
		cut := n - int64(r.SnapshotDays)*day
		if err := exec("item_snapshot_expired", `DELETE FROM item_snapshot WHERE snapshot_id IN (SELECT id FROM snapshot WHERE fetched_at < ?)`, cut); err != nil {
			return out, err
		}
		if err := exec("snapshot", `DELETE FROM snapshot WHERE fetched_at < ?`, cut); err != nil {
			return out, err
		}
	}
	if r.FullDays > 0 && r.ThinHours > 0 {
		cut := n - int64(r.FullDays)*day
		bucket := int64(r.ThinHours) * 3600
		if err := exec("item_snapshot_thinned", `DELETE FROM item_snapshot WHERE snapshot_id IN (
			SELECT id FROM snapshot WHERE fetched_at < ? AND id NOT IN (
				SELECT MIN(id) FROM snapshot WHERE fetched_at < ? GROUP BY house_id, kind, fetched_at / ?))`, cut, cut, bucket); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Backup writes a consistent copy with VACUUM INTO, compresses it with zstd and keeps the
// newest `keep` backups. Restore with: zstd -d ahc-YYYYMMDD.db.zst -o ahc.db
func (s *Store) Backup(ctx context.Context, dir string, day time.Time, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "ahc-backup.tmp.db")
	_ = os.Remove(tmp) // VACUUM INTO refuses to overwrite
	defer os.Remove(tmp)
	if _, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ahc-"+day.Format("20060102")+".db.zst")
	if err := compressFile(tmp, path); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return path, err
	}
	var backups []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ahc-") && (strings.HasSuffix(e.Name(), ".db.zst") || strings.HasSuffix(e.Name(), ".db")) && e.Name() != filepath.Base(tmp) {
			backups = append(backups, e.Name())
		}
	}
	sort.Strings(backups)
	for len(backups) > keep {
		_ = os.Remove(filepath.Join(dir, backups[0]))
		backups = backups[1:]
	}
	return path, nil
}

func compressFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst + ".part")
	if err != nil {
		return err
	}
	zw, err := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		out.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(dst+".part", dst)
}

// EffectiveInterval is the average spacing between distinct snapshots over the window, or 0 if fewer than two.
func (s *Store) EffectiveInterval(ctx context.Context, houseID int64, kind listing.Kind, window time.Duration) (time.Duration, error) {
	since := time.Now().Add(-window).Unix()
	var n int
	var minT, maxT sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT count(*), MIN(fetched_at), MAX(fetched_at) FROM snapshot WHERE house_id=? AND kind=? AND fetched_at >= ?`,
		houseID, string(kind), since).Scan(&n, &minT, &maxT)
	if err != nil || n < 2 {
		return 0, err
	}
	return time.Duration(maxT.Int64-minT.Int64) * time.Second / time.Duration(n-1), nil
}

// OpenCount is the number of open auctions for a house and kind.
func (s *Store) OpenCount(ctx context.Context, houseID int64, kind listing.Kind) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM auction WHERE house_id=? AND kind=? AND closed_at IS NULL`, houseID, string(kind)).Scan(&n)
	return n, err
}

// PublishTimes returns the Last-Modified times of snapshots since `since`, per "house/kind",
// oldest first. Used to learn when each house publishes.
func (s *Store) PublishTimes(ctx context.Context, since int64) (map[string][]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT house_id, kind, last_modified FROM snapshot
		WHERE last_modified > 0 AND fetched_at >= ? ORDER BY fetched_at`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var h, lm int64
		var k string
		if err := rows.Scan(&h, &k, &lm); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%d/%s", h, k)
		out[key] = append(out[key], lm)
	}
	return out, rows.Err()
}

// BackfillFront sets auction.front for auctions first seen since the given time that predate
// it, from the cheapest price of their item in the snapshot that first listed them. One house
// and kind at a time, so the collector's writes wait seconds, not minutes.
func (s *Store) BackfillFront(ctx context.Context, since int64) (int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT house_id, kind FROM snapshot`)
	if err != nil {
		return 0, err
	}
	var hk [][2]any
	for rows.Next() {
		var h int64
		var k string
		if err := rows.Scan(&h, &k); err != nil {
			rows.Close()
			return 0, err
		}
		hk = append(hk, [2]any{h, k})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, x := range hk {
		res, err := s.DB.ExecContext(ctx, `UPDATE auction SET front = COALESCE(unit_price > 0 AND unit_price <= (
				SELECT i.min_unit FROM snapshot sn JOIN item_snapshot i ON i.snapshot_id=sn.id
				WHERE sn.house_id=auction.house_id AND sn.kind=auction.kind AND sn.fetched_at>=auction.first_seen AND sn.fetched_at<auction.first_seen+3600
					AND COALESCE(NULLIF(sn.last_modified,0), sn.fetched_at)=auction.first_seen
					AND i.item_id=auction.item_id AND i.variant=auction.variant LIMIT 1), 0)
			WHERE house_id=? AND kind=? AND front IS NULL AND first_seen>=?`, x[0], x[1], since)
		if err != nil {
			return n, err
		}
		c, _ := res.RowsAffected()
		n += c
	}
	return n, nil
}

// ReclassifyGearSales applies the tracker's rules 4c and 4d to gear recorded as sold since the
// given time, before the rules existed: a copy that vanished while a cheaper one, listed before
// it, was still there afterwards (4c), or as an identical copy at the same price appeared (4d),
// becomes cancelled. Returns how many changed; the daily rows of those days need rolling up again.
func (s *Store) ReclassifyGearSales(ctx context.Context, since int64) (int64, error) {
	houses, err := s.DB.QueryContext(ctx, `SELECT DISTINCT house_id FROM auction WHERE kind='gear'`)
	if err != nil {
		return 0, err
	}
	var hs []int64
	for houses.Next() {
		var h int64
		if err := houses.Scan(&h); err != nil {
			houses.Close()
			return 0, err
		}
		hs = append(hs, h)
	}
	houses.Close()
	if err := houses.Err(); err != nil {
		return 0, err
	}
	type auc struct {
		id, price, qty, first, last, closed int64 // closed 0: still open
		sold                                bool
	}
	var n int64
	for _, h := range hs {
		// Everything that could have been up next to a sale since then: listings last longer
		// than 2 days in no game version.
		rows, err := s.DB.QueryContext(ctx, `SELECT auction_id, item_id, variant, unit_price, COALESCE(qty_first, 1), first_seen, last_seen, COALESCE(closed_at, 0), outcome='sold'
			FROM auction WHERE house_id=? AND kind='gear' AND unit_price>0 AND (closed_at IS NULL OR closed_at>=?)`, h, since-2*86400)
		if err != nil {
			return n, err
		}
		groups := map[[2]any][]auc{}
		for rows.Next() {
			var a auc
			var item int64
			var variant string
			var sold sql.NullBool
			if err := rows.Scan(&a.id, &item, &variant, &a.price, &a.qty, &a.first, &a.last, &a.closed, &sold); err != nil {
				rows.Close()
				return n, err
			}
			a.sold = sold.Bool
			k := [2]any{item, variant}
			groups[k] = append(groups[k], a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return n, err
		}
		var change []int64
		for _, g := range groups {
			sort.Slice(g, func(i, j int) bool { return g[i].price < g[j].price })
			for i, a := range g {
				if !a.sold || a.closed < since {
					continue
				}
				cancelled := false
				for _, b := range g[:i] {
					if b.price >= a.price {
						break
					}
					if b.first <= a.last && (b.closed == 0 || b.closed > a.closed) {
						cancelled = true // 4c
						break
					}
				}
				// 4d: reposted at the same price in the scan it vanished. g is sorted by price, so
				// start at the first copy at a's price.
				for j := sort.Search(len(g), func(j int) bool { return g[j].price >= a.price }); !cancelled && j < len(g) && g[j].price == a.price; j++ {
					b := g[j]
					cancelled = b.id != a.id && b.qty == a.qty && b.first == a.closed
				}
				if cancelled {
					change = append(change, a.id)
				}
			}
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return n, err
		}
		upd, err := tx.PrepareContext(ctx, `UPDATE auction SET outcome='cancelled' WHERE house_id=? AND kind='gear' AND auction_id=?`)
		if err != nil {
			tx.Rollback()
			return n, err
		}
		for _, id := range change {
			if _, err := upd.ExecContext(ctx, h, id); err != nil {
				upd.Close()
				tx.Rollback()
				return n, err
			}
		}
		upd.Close()
		if err := tx.Commit(); err != nil {
			return n, err
		}
		n += int64(len(change))
	}
	return n, nil
}

// migrate adds columns introduced after a database was first created. CREATE TABLE IF NOT
// EXISTS leaves existing tables alone, so new columns are added here.
func migrate(db *sql.DB) ([]string, error) {
	var added []string
	add := []struct{ table, column, decl string }{
		{"item_daily", "sales", "INT DEFAULT 0"},
		{"item_daily", "largest_sale", "INT DEFAULT 0"},
		{"item", "tooltip", "TEXT"},
		{"auction", "front", "INT"},
	}
	for _, a := range add {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, a.table, a.column).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", a.table, a.column, a.decl)); err != nil {
				return nil, err
			}
			added = append(added, a.table+"."+a.column)
		}
	}
	// Indexes on added columns: here, since schema.sql runs before the columns exist.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS auction_front ON auction (house_id, first_seen) WHERE front=1`); err != nil {
		return nil, err
	}
	return added, nil
}

// SaveFlipCache stores the precomputed flip inputs (opaque bytes to the store).
func (s *Store) SaveFlipCache(ctx context.Context, computedAt int64, data []byte) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO flip_cache (id, computed_at, data) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET computed_at=excluded.computed_at, data=excluded.data`, computedAt, data)
	return err
}

// FlipCacheTime returns when the flip inputs were last computed (0 if never).
func (s *Store) FlipCacheTime(ctx context.Context) (int64, error) {
	var t int64
	err := s.DB.QueryRowContext(ctx, `SELECT computed_at FROM flip_cache WHERE id=1`).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return t, err
}

// LoadFlipCache returns the stored flip inputs, or nil if none.
func (s *Store) LoadFlipCache(ctx context.Context) (int64, []byte, error) {
	var t int64
	var b []byte
	err := s.DB.QueryRowContext(ctx, `SELECT computed_at, data FROM flip_cache WHERE id=1`).Scan(&t, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil
	}
	return t, b, err
}

// FlipHistorySource says where a flip_history row came from.
const (
	FlipHistoryLive    = "live"    // computed by the collector after a scan
	FlipHistoryRebuilt = "rebuilt" // reconstructed by a backtest for a past planning time
)

// FlipHistoryRow is one stored set of flip inputs (Data is compressed and opaque to the store).
type FlipHistoryRow struct {
	At     int64
	Source string
	Data   []byte
}

// SaveFlipHistory stores flip inputs computed at the given time, replacing any row at that time.
func (s *Store) SaveFlipHistory(ctx context.Context, at int64, source, version string, data []byte) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO flip_history (computed_at, source, version, data) VALUES (?,?,?,?)
		ON CONFLICT(computed_at) DO UPDATE SET source=excluded.source, version=excluded.version, data=excluded.data`, at, source, version, data)
	return err
}

// FlipHistory returns the stored flip inputs computed in [from, to], oldest first.
func (s *Store) FlipHistory(ctx context.Context, from, to int64) ([]FlipHistoryRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT computed_at, source, data FROM flip_history WHERE computed_at>=? AND computed_at<=? ORDER BY computed_at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlipHistoryRow
	for rows.Next() {
		var r FlipHistoryRow
		if err := rows.Scan(&r.At, &r.Source, &r.Data); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneFlipHistory deletes stored flip inputs computed before the given time.
func (s *Store) PruneFlipHistory(ctx context.Context, before int64) (int64, error) {
	r, err := s.DB.ExecContext(ctx, `DELETE FROM flip_history WHERE computed_at<?`, before)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// Meta returns a stored marker value, or "" if unset.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta stores a marker value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// SetWritRep records the reputation a writ's quest pays.
func (s *Store) SetWritRep(ctx context.Context, itemID, questID int64, rep int) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO writ_quest (item_id, quest_id, rep, fetched_at) VALUES (?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET quest_id=excluded.quest_id, rep=excluded.rep, fetched_at=excluded.fetched_at`,
		itemID, questID, rep, time.Now().Unix())
	return err
}

// WritReps returns writ item id → quest reputation for every writ fetched so far.
func (s *Store) WritReps(ctx context.Context) (map[int64]int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT item_id, rep FROM writ_quest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]int{}
	for rows.Next() {
		var id int64
		var rep int
		if err := rows.Scan(&id, &rep); err != nil {
			return nil, err
		}
		m[id] = rep
	}
	return m, rows.Err()
}

// SaveSupplyCache stores the precomputed Supply page (opaque bytes to the store).
func (s *Store) SaveSupplyCache(ctx context.Context, computedAt int64, data []byte) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO supply_cache (id, computed_at, data) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET computed_at=excluded.computed_at, data=excluded.data`, computedAt, data)
	return err
}

// LoadSupplyCache returns the stored Supply page, or nil if none.
func (s *Store) LoadSupplyCache(ctx context.Context) (int64, []byte, error) {
	var t int64
	var b []byte
	err := s.DB.QueryRowContext(ctx, `SELECT computed_at, data FROM supply_cache WHERE id=1`).Scan(&t, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil
	}
	return t, b, err
}

// LastSupplyPoint returns the time of a house's newest Supply chart point (0 if none).
func (s *Store) LastSupplyPoint(ctx context.Context, houseID int64) (int64, error) {
	var t int64
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts),0) FROM supply_history WHERE house_id=?`, houseID).Scan(&t)
	return t, err
}

// ReplaceSupplyPoints deletes a house's chart points after since and stores points (ts → JSON)
// in one transaction.
func (s *Store) ReplaceSupplyPoints(ctx context.Context, houseID, since int64, points map[int64][]byte) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM supply_history WHERE house_id=? AND ts > ?`, houseID, since); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO supply_history (house_id, ts, data) VALUES (?,?,?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for t, b := range points {
		if _, err := ins.ExecContext(ctx, houseID, t, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SupplyPoints returns a house's chart points since since, oldest first, as raw JSON.
func (s *Store) SupplyPoints(ctx context.Context, houseID, since int64) ([]json.RawMessage, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT data FROM supply_history WHERE house_id=? AND ts >= ? ORDER BY ts`, houseID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(d))
	}
	return out, rows.Err()
}

// Package web serves the browser UI and its JSON API from the same binary.
package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"github.com/herrfennessey/ah-scanner/internal/suffix"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

//go:embed static
var static embed.FS

type Server struct {
	Cfg   config.Config
	Store *store.Store
	Log   *slog.Logger

	mu        sync.Mutex
	cache     map[int64]cachedList
	flipCache *report.FlipInputs
}

type cachedList struct {
	stamp string
	rows  []ItemRow
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/houses", s.houses)
	mux.HandleFunc("GET /api/items", s.items)
	mux.HandleFunc("GET /api/item/{id}", s.item)
	mux.HandleFunc("GET /api/item/{id}/history", s.history)
	mux.HandleFunc("GET /api/item/{id}/sell", s.sell)
	mux.HandleFunc("GET /api/item/{id}/book", s.book)
	mux.HandleFunc("GET /api/item/{id}/tooltip", s.tooltip)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/flips", s.apiFlips)
	mux.HandleFunc("GET /api/flips/version", s.apiFlipsVersion)
	mux.HandleFunc("GET /api/supply", s.apiSupply)
	mcpH := s.mcpHandler()
	for _, m := range []string{"POST", "GET", "DELETE"} {
		mux.Handle(m+" /mcp", mcpH) // explicit methods: "/mcp" alone conflicts with "GET /"
	}
	sub, _ := fs.Sub(static, "static")
	files := http.FileServerFS(sub)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	return mux
}

// ListenAndServe runs until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	s.Log.Info("web UI listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

type House struct {
	ID       int64   `json:"id"`
	Faction  string  `json:"faction"`
	Name     string  `json:"name"`
	Listings int64   `json:"listings"`
	Items    int64   `json:"items"`
	LastScan int64   `json:"last_scan"`
	Cut      float64 `json:"cut"`
	ViewOnly bool    `json:"view_only"` // not tradeable: shown for reference (Alliance from a Horde character)
	Tracked  bool    `json:"tracked"`   // individual auctions tracked, so sales are known
}

func (s *Server) houseList(ctx context.Context) ([]House, error) {
	var out []House
	for _, h := range s.Cfg.Houses {
		id := h.ID
		var name string
		if id == 0 {
			var err error
			id, name, err = s.Store.HouseByFaction(ctx, h.Faction)
			if err != nil {
				continue
			}
		} else {
			s.Store.DB.QueryRowContext(ctx, `SELECT name FROM house WHERE id=?`, id).Scan(&name)
		}
		hh := House{ID: id, Faction: h.Faction, Name: name, Cut: h.AHCut(), ViewOnly: !h.Tradeable(), Tracked: !h.PriceOnly}
		s.Store.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(n_auctions),0), COALESCE(MAX(fetched_at),0) FROM snapshot
			WHERE id IN (SELECT MAX(id) FROM snapshot WHERE house_id=? GROUP BY kind)`, id).Scan(&hh.Listings, &hh.LastScan)
		s.Store.DB.QueryRowContext(ctx, `SELECT count(DISTINCT item_id) FROM item_snapshot
			WHERE snapshot_id IN (SELECT MAX(id) FROM snapshot WHERE house_id=? GROUP BY kind)`, id).Scan(&hh.Items)
		out = append(out, hh)
	}
	return out, nil
}

func (s *Server) houses(w http.ResponseWriter, r *http.Request) {
	hs, err := s.houseList(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, hs)
}

// houseParam resolves ?house= (id or faction), defaulting to the first configured house.
func (s *Server) houseParam(r *http.Request) (House, error) {
	return s.houseByName(r.Context(), r.URL.Query().Get("house"))
}

func (s *Server) houseByName(ctx context.Context, q string) (House, error) {
	hs, err := s.houseList(ctx)
	if err != nil {
		return House{}, err
	}
	if len(hs) == 0 {
		return House{}, errors.New("no house data yet: the collector has not completed a cycle")
	}
	q = strings.ToLower(strings.TrimSpace(q))
	for _, h := range hs {
		if (q == "" && !h.ViewOnly) || q == h.Faction || q == strconv.FormatInt(h.ID, 10) {
			return h, nil
		}
	}
	return House{}, errors.New("unknown house " + strconv.Quote(q))
}

// ItemRow is one line of the browse table.
type ItemRow struct {
	ID         int64    `json:"id"`
	Variant    string   `json:"variant,omitempty"`
	Suffix     string   `json:"suffix,omitempty"` // "of the Eagle", already part of Name
	Name       string   `json:"name"`
	Quality    int      `json:"quality"`
	ItemLevel  int      `json:"ilvl"`
	ReqLevel   int      `json:"req_level"`
	Class      string   `json:"class"`
	Subclass   string   `json:"subclass"`
	Icon       string   `json:"icon"`
	Min        int64    `json:"min"`
	Median     int64    `json:"median"`
	Fair       int64    `json:"fair"`
	Available  int64    `json:"available"`
	Auctions   int64    `json:"auctions"`
	SoldEst    float64  `json:"sold_est"`
	SoldLo     float64  `json:"sold_lo"`
	SoldHi     float64  `json:"sold_hi"`
	SellThru   float64  `json:"sell_through"`
	Change24   *float64 `json:"change_24h"`
	Volatile   bool     `json:"volatile"`
	MarketGold int64    `json:"market_gold"`
	// Hint: sell into a stretched price, chase a falling one, hold a steady one, or stop
	// reposting a slow one. Nil when nothing stands out or there's no history yet.
	Hint *report.Hint `json:"hint,omitempty"`
}

type itemMeta struct {
	name, class, subclass, icon string
	quality, ilvl, req          int
}

func loadMeta(ctx context.Context, db *sql.DB) (map[int64]itemMeta, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(name,''), COALESCE(class,''), COALESCE(subclass,''), COALESCE(icon,''),
		COALESCE(quality,1), COALESCE(item_level,0), COALESCE(required_level,0) FROM item`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]itemMeta{}
	for rows.Next() {
		var id int64
		var x itemMeta
		if err := rows.Scan(&id, &x.name, &x.class, &x.subclass, &x.icon, &x.quality, &x.ilvl, &x.req); err != nil {
			return nil, err
		}
		m[id] = x
	}
	return m, rows.Err()
}

type snapStat struct{ min, p20, median, listed, n int64 }

// statsAt returns per-item stats from the newest snapshot of each kind at or before `at` (0 = latest).
func statsAt(ctx context.Context, db *sql.DB, houseID, at int64) (map[report.Key]snapStat, error) {
	q := `SELECT i.item_id, i.variant, COALESCE(i.min_unit,0), COALESCE(i.p20_unit,0), COALESCE(i.median_unit,0), i.listed_qty, i.n_auctions
		FROM item_snapshot i WHERE i.snapshot_id IN (SELECT MAX(id) FROM snapshot WHERE house_id=? GROUP BY kind)`
	args := []any{houseID}
	if at > 0 {
		q = strings.Replace(q, "WHERE house_id=? GROUP BY kind", "WHERE house_id=? AND fetched_at<=? GROUP BY kind", 1)
		args = append(args, at)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[report.Key]snapStat{}
	for rows.Next() {
		var k report.Key
		var s snapStat
		if err := rows.Scan(&k.Item, &k.Variant, &s.min, &s.p20, &s.median, &s.listed, &s.n); err != nil {
			return nil, err
		}
		m[k] = s
	}
	return m, rows.Err()
}

func (s *Server) buildRows(ctx context.Context, houseID int64) ([]ItemRow, error) {
	var stamp string
	s.Store.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) || '/' || (SELECT count(*) FROM item WHERE name IS NOT NULL) || '/' ||
		(SELECT COALESCE(MAX(day),'') FROM item_daily WHERE house_id=?) FROM snapshot WHERE house_id=?`, houseID, houseID).Scan(&stamp)
	s.mu.Lock()
	if c, ok := s.cache[houseID]; ok && c.stamp == stamp {
		s.mu.Unlock()
		return c.rows, nil
	}
	s.mu.Unlock()

	now := time.Now()
	cur, err := statsAt(ctx, s.Store.DB, houseID, 0)
	if err != nil {
		return nil, err
	}
	prev, err := statsAt(ctx, s.Store.DB, houseID, now.Add(-24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	recent, err := statsAt(ctx, s.Store.DB, houseID, now.Add(-3*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	// Resolved houses, so a house configured by faction alone (id 0) still finds its odds.
	var house House
	hs, err := s.houseList(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if h.ID == houseID {
			house = h
			break
		}
	}
	daily, _, err := report.LoadDaily(ctx, s.Store.DB, houseID, 7, now)
	if err != nil {
		return nil, err
	}
	meta, err := loadMeta(ctx, s.Store.DB)
	if err != nil {
		return nil, err
	}
	rows := make([]ItemRow, 0, len(cur))
	for k, c := range cur {
		m, ok := meta[k.Item]
		if !ok {
			m.quality = 1
		}
		row := ItemRow{ID: k.Item, Variant: k.Variant, Name: m.name, Quality: m.quality, ItemLevel: m.ilvl, ReqLevel: m.req,
			Class: m.class, Subclass: m.subclass, Icon: m.icon, Min: c.min, Median: c.median, Available: c.listed, Auctions: c.n, SellThru: -1}
		if row.Name == "" {
			row.Name = "Item #" + strconv.FormatInt(k.Item, 10)
		}
		row.Suffix = suffix.Name(k.Variant)
		row.Name = suffix.Label(row.Name, k.Variant)
		row.Fair = c.p20
		if d := daily[k]; d != nil {
			if d.Fair > 0 {
				row.Fair = d.Fair
			}
			row.SoldEst, row.SoldLo, row.SoldHi = d.SoldEst, d.SoldLo, d.SoldHi
			row.SellThru = d.SellThrough()
			row.Volatile = d.Volatile()
			// Judged against the item's own history only (d.Fair, not the p20 fallback above).
			row.Hint = report.MarketHint(house.Faction, report.HintInput{P20: c.p20, PrevP20: recent[k].p20, Listed: c.listed,
				Fair: d.Fair, UsualListed: d.Listed, SoldPerDay: d.SoldLo, Untracked: !house.Tracked})
		}
		if p, ok := prev[k]; ok && p.p20 > 0 && c.p20 > 0 {
			ch := float64(c.p20)/float64(p.p20) - 1
			row.Change24 = &ch
		}
		row.MarketGold = row.Fair * row.Available
		rows = append(rows, row)
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[int64]cachedList{}
	}
	s.cache[houseID] = cachedList{stamp: stamp, rows: rows}
	s.mu.Unlock()
	return rows, nil
}

type classCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func (s *Server) items(w http.ResponseWriter, r *http.Request) {
	h, err := s.houseParam(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	all, err := s.buildRows(r.Context(), h.ID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	q := r.URL.Query()
	text := strings.ToLower(strings.TrimSpace(q.Get("q")))
	class := q.Get("class")
	qualities := map[int]bool{}
	for _, v := range strings.Split(q.Get("quality"), ",") {
		if n, err := strconv.Atoi(v); err == nil {
			qualities[n] = true
		}
	}
	ilMin, _ := strconv.Atoi(q.Get("ilvl_min"))
	ilMax, _ := strconv.Atoi(q.Get("ilvl_max"))
	reqMin, _ := strconv.Atoi(q.Get("req_min"))
	reqMax, _ := strconv.Atoi(q.Get("req_max"))
	minSold, _ := strconv.ParseFloat(q.Get("min_sold"), 64)

	classes := map[string]int{}
	var rows []ItemRow
	for _, row := range all {
		if text != "" && !strings.Contains(strings.ToLower(row.Name), text) && strconv.FormatInt(row.ID, 10) != text {
			continue
		}
		if len(qualities) > 0 && !qualities[row.Quality] {
			continue
		}
		if (ilMin > 0 && row.ItemLevel < ilMin) || (ilMax > 0 && row.ItemLevel > ilMax) ||
			(reqMin > 0 && row.ReqLevel < reqMin) || (reqMax > 0 && row.ReqLevel > reqMax) || row.SoldLo < minSold {
			continue
		}
		cn := row.Class
		if cn == "" {
			cn = "Unknown"
		}
		classes[cn]++
		if class != "" && cn != class {
			continue
		}
		rows = append(rows, row)
	}
	sortKey, desc := q.Get("sort"), q.Get("dir") != "asc"
	less := map[string]func(a, b ItemRow) bool{
		"name":      func(a, b ItemRow) bool { return a.Name < b.Name },
		"min":       func(a, b ItemRow) bool { return a.Min < b.Min },
		"fair":      func(a, b ItemRow) bool { return a.Fair < b.Fair },
		"available": func(a, b ItemRow) bool { return a.Available < b.Available },
		"sold":      func(a, b ItemRow) bool { return a.SoldEst < b.SoldEst },
		"turnover":  func(a, b ItemRow) bool { return a.SoldEst*float64(a.Fair) < b.SoldEst*float64(b.Fair) },
		"change": func(a, b ItemRow) bool {
			return ch(a) < ch(b)
		},
		"value": func(a, b ItemRow) bool { return a.MarketGold < b.MarketGold },
	}[sortKey]
	if less == nil {
		less = func(a, b ItemRow) bool { return a.MarketGold < b.MarketGold }
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
	total := len(rows)
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	cl := make([]classCount, 0, len(classes))
	for n, c := range classes {
		cl = append(cl, classCount{n, c})
	}
	sort.Slice(cl, func(i, j int) bool { return cl[i].Count > cl[j].Count })
	unnamed := 0
	for _, r := range all {
		if r.Class == "" && strings.HasPrefix(r.Name, "Item #") {
			unnamed++
		}
	}
	writeJSON(w, map[string]any{"house": h, "total": total, "rows": rows[offset:end], "classes": cl, "all_count": len(all), "unnamed": unnamed})
}

func ch(r ItemRow) float64 {
	if r.Change24 == nil {
		return 0
	}
	return *r.Change24
}

func (s *Server) item(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, err)
		return
	}
	h, err := s.houseParam(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	variant := r.URL.Query().Get("variant")
	hs, _ := s.houseList(r.Context())
	type perHouse struct {
		House House    `json:"house"`
		Row   *ItemRow `json:"row"`
	}
	var houses []perHouse
	var self *ItemRow
	for _, hh := range hs {
		rows, err := s.buildRows(r.Context(), hh.ID)
		if err != nil {
			fail(w, 500, err)
			return
		}
		var found *ItemRow
		for i := range rows {
			if rows[i].ID == id && rows[i].Variant == variant {
				found = &rows[i]
				break
			}
		}
		houses = append(houses, perHouse{hh, found})
		if hh.ID == h.ID {
			self = found
		}
	}
	if self == nil {
		meta, _ := loadMeta(r.Context(), s.Store.DB)
		m, ok := meta[id]
		if !ok {
			fail(w, 404, errors.New("this item has not been seen on the auction house"))
			return
		}
		self = &ItemRow{ID: id, Name: m.name, Quality: m.quality, ItemLevel: m.ilvl, ReqLevel: m.req, Class: m.class, Subclass: m.subclass, Icon: m.icon, SellThru: -1}
	}
	var variants []string
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT DISTINCT variant FROM auction WHERE house_id=? AND item_id=? AND variant != '' LIMIT 50`, h.ID, id)
	if err == nil {
		for rows.Next() {
			var v string
			rows.Scan(&v)
			variants = append(variants, v)
		}
		rows.Close()
	}
	writeJSON(w, map[string]any{"house": h, "item": self, "houses": houses, "variants": variants})
}

type histPoint struct {
	T      int64 `json:"t"`
	Min    int64 `json:"min"`
	P20    int64 `json:"p20"`
	Median int64 `json:"median"`
	Listed int64 `json:"listed"`
}

type dayPoint struct {
	Day  string `json:"day"`
	Est  int64  `json:"est"`
	Lo   int64  `json:"lo"`
	Hi   int64  `json:"hi"`
	Fair int64  `json:"fair"`
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, err)
		return
	}
	h, err := s.houseParam(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	variant := r.URL.Query().Get("variant")
	spans := map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "all": 20 * 365 * 24 * time.Hour}
	span, ok := spans[r.URL.Query().Get("range")]
	if !ok {
		span = spans["7d"]
	}
	since := time.Now().Add(-span).Unix()
	// Price-only houses have no auction rows, and closed auctions are pruned, so fall back to
	// the snapshots the item appeared in.
	var kind string
	s.Store.DB.QueryRowContext(r.Context(), `SELECT kind FROM auction WHERE house_id=? AND item_id=? LIMIT 1`, h.ID, id).Scan(&kind)
	if kind == "" {
		s.Store.DB.QueryRowContext(r.Context(), `SELECT s.kind FROM item_snapshot i JOIN snapshot s ON s.id=i.snapshot_id
			WHERE i.item_id=? AND s.house_id=? LIMIT 1`, id, h.ID).Scan(&kind)
	}
	if kind == "" {
		kind = string(listing.Commodity)
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT s.fetched_at, COALESCE(i.min_unit,0), COALESCE(i.p20_unit,0), COALESCE(i.median_unit,0), COALESCE(i.listed_qty,0)
		FROM snapshot s LEFT JOIN item_snapshot i ON i.snapshot_id=s.id AND i.item_id=? AND i.variant=?
		WHERE s.house_id=? AND s.kind=? AND s.fetched_at>=?
		AND EXISTS (SELECT 1 FROM item_snapshot x WHERE x.snapshot_id=s.id) -- skip snapshots thinned by retention
		ORDER BY s.fetched_at`, id, variant, h.ID, kind, since)
	if err != nil {
		fail(w, 500, err)
		return
	}
	pts := []histPoint{}
	for rows.Next() {
		var p histPoint
		rows.Scan(&p.T, &p.Min, &p.P20, &p.Median, &p.Listed)
		pts = append(pts, p)
	}
	rows.Close()
	days := []dayPoint{}
	drows, err := s.Store.DB.QueryContext(r.Context(), `SELECT day, sold_est, sold_lo, sold_hi, COALESCE(fair_unit,0) FROM item_daily
		WHERE house_id=? AND item_id=? AND variant=? AND day>=? ORDER BY day`, h.ID, id, variant, time.Unix(since, 0).Format("2006-01-02"))
	if err == nil {
		for drows.Next() {
			var d dayPoint
			drows.Scan(&d.Day, &d.Est, &d.Lo, &d.Hi, &d.Fair)
			days = append(days, d)
		}
		drows.Close()
	}
	writeJSON(w, map[string]any{"points": pts, "days": days})
}

func (s *Server) sell(w http.ResponseWriter, r *http.Request) {
	h, err := s.houseParam(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	a, err := report.AnalyzeSell(r.Context(), s.Store, report.SellOptions{
		Options: report.Options{HouseID: h.ID, HouseName: h.Name, Cut: h.Cut, DepositRate: s.Cfg.DepositRate, Days: days, Now: time.Now()},
		Item:    r.PathValue("id"), Variant: r.URL.Query().Get("variant"), BlockHours: 3, AmbiguousWeight: s.Cfg.AmbiguousWeight,
	})
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, a)
}

// book is the item's price distribution: open listings by price and recent purchases.
func (s *Server) book(w http.ResponseWriter, r *http.Request) {
	h, err := s.houseParam(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, err)
		return
	}
	b, err := report.LoadItemBook(r.Context(), s.Store.DB, h.ID, id, r.URL.Query().Get("variant"), h.Tracked, time.Now().Unix())
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, b)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(s.Cfg.StatusPath())
	if err != nil {
		writeJSON(w, map[string]any{"error": "no status yet"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

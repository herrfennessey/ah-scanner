package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/herrfennessey/ah-scanner/internal/config"
	"github.com/herrfennessey/ah-scanner/internal/listing"
	"github.com/herrfennessey/ah-scanner/internal/report"
	"github.com/herrfennessey/ah-scanner/internal/rollup"
	"github.com/herrfennessey/ah-scanner/internal/store"
)

// fixture builds a small database: Elemental Air on Horde with one partial sale.
func fixture(t *testing.T, token string) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ahc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	st.UpsertHouse(ctx, 113, "Horde Auction House", "horde")
	st.UpsertHouse(ctx, 116, "Blackwater Auction House", "blackwater")
	st.SetItem(ctx, store.Item{ID: 7069, Name: "Elemental Air", Quality: 1, Class: "Trade Goods", ItemLevel: 25})
	now := time.Now().Add(-time.Hour).Unix()
	l := []listing.Listing{{AuctionID: 1, ItemID: 7069, Quantity: 10, UnitPrice: 2289, TimeLeft: listing.Long}}
	st.ApplySnapshot(ctx, store.Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: now, SHA256: "a"}, l)
	l[0].Quantity = 4
	st.ApplySnapshot(ctx, store.Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: now + 1800, SHA256: "b"}, l)
	rollup.Range(ctx, st.DB, time.Now(), time.Now(), rollup.Options{AmbiguousWeight: 0.25})
	// As the collector does after each publish: precompute the flip inputs.
	in, err := report.BuildFlipInputs(ctx, st, []report.FlipHouse{
		{ID: 113, Faction: "horde", Cut: 0.05, Tracked: true}, {ID: 116, Faction: "blackwater", Cut: 0.15, Tracked: true},
	}, 0.2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(in)
	st.SaveFlipCache(ctx, in.ComputedAt, b)

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.MCPToken = token
	srv := httptest.NewServer((&Server{Cfg: cfg, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func TestItemsAPI(t *testing.T) {
	srv, _ := fixture(t, "")
	resp, err := http.Get(srv.URL + "/api/items?house=horde&q=air")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Total int       `json:"total"`
		Rows  []ItemRow `json:"rows"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Total != 1 || out.Rows[0].Name != "Elemental Air" || out.Rows[0].Min != 2289 || out.Rows[0].Available != 4 {
		t.Fatalf("%+v", out)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPTools(t *testing.T) {
	srv, _ := fixture(t, "s3cret")
	ctx := context.Background()

	// Without the token the endpoint refuses.
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: HTTP %d", resp.StatusCode)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{"s3cret"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "flips") || len(names) != 6 {
		t.Fatalf("tools: %s", got)
	}

	call := func(name string, args any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("%s: %v", name, res.Content)
		}
		b, _ := json.Marshal(res.StructuredContent)
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	s := call("search_items", map[string]any{"query": "elemental air"})
	items := s["items"].([]any)
	first := items[0].(map[string]any)
	if first["name"] != "Elemental Air" || first["min_buyout_text"] != "22s89c" {
		t.Fatalf("search: %v", first)
	}
	if sold := first["sold_per_day"].(map[string]any); sold["lo"].(float64) != 6 {
		t.Fatalf("partial sale not counted: %v", sold)
	}
	sell := call("sell_advice", map[string]any{"item": "7069"})
	if a := sell["analysis"].(map[string]any); a["name"] != "Elemental Air" || len(a["verdict"].([]any)) == 0 {
		t.Fatalf("sell: %v", a)
	}
	item := call("item_summary", map[string]any{"item": "Elemental Air"})
	if len(item["houses"].([]any)) != 2 {
		t.Fatalf("item: %v", item)
	}
	call("movers", map[string]any{"min_sold_lo": 1})
	if fl := call("flips", map[string]any{"gold": 40}); fl["gold"].(float64) != 400000 || fl["funnel"] == nil {
		t.Fatalf("flips: %v", fl)
	}
	if st := call("collector_status", map[string]any{}); st["houses"] == nil {
		t.Fatalf("status: %v", st)
	}
}

func TestTooltipEndpoint(t *testing.T) {
	srv, st := fixture(t, "")
	st.SetItem(context.Background(), store.Item{ID: 7069, Name: "Elemental Air", Tooltip: json.RawMessage(`{"name":"Elemental Air"}`)})
	resp, err := http.Get(srv.URL + "/api/item/7069/tooltip")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Name      string          `json:"name"`
		Preview   json.RawMessage `json:"preview"`
		House     string          `json:"house"`
		Min       int64           `json:"min"`
		Available int64           `json:"available"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out.Name != "Elemental Air" || string(out.Preview) != `{"name":"Elemental Air"}` || out.Min != 2289 || out.Available != 4 || out.House != "horde" {
		t.Fatalf("%d %+v", resp.StatusCode, out)
	}
	resp2, _ := http.Get(srv.URL + "/api/item/999/tooltip")
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("unknown item: %d", resp2.StatusCode)
	}
}

func TestSupplyEndpoint(t *testing.T) {
	srv, st := fixture(t, "")
	ctx := context.Background()
	fill := `{"spells":[{"description":"Use: Fill the crate with any bundle from the following list:\n- 20 Peacebloom\n- 10 Earthroot"}]}`
	for _, it := range []store.Item{
		{ID: 248682, Name: "Waylaid Crate: Apprentice Herbs", Tooltip: json.RawMessage(fill)},
		{ID: 2449, Name: "Earthroot"},
		{ID: 264021, Name: "Craftsman's Writ: Golden Rod"},
		{ID: 11128, Name: "Golden Rod"},
	} {
		st.SetItem(ctx, it)
	}
	st.SetWritRep(ctx, 264021, 94221, 75)
	now := time.Now().Unix()
	st.ApplySnapshot(ctx, store.Snapshot{HouseID: 113, Kind: listing.Commodity, FetchedAt: now, SHA256: "s"}, []listing.Listing{
		{AuctionID: 1, ItemID: 7069, Quantity: 4, UnitPrice: 2289, TimeLeft: listing.Long},
		{AuctionID: 11, ItemID: 248682, Quantity: 3, UnitPrice: 10, TimeLeft: listing.Long},
		{AuctionID: 12, ItemID: 2449, Quantity: 100, UnitPrice: 60, TimeLeft: listing.Long},
		{AuctionID: 13, ItemID: 264021, Quantity: 1, UnitPrice: 500, TimeLeft: listing.Long},
		{AuctionID: 14, ItemID: 11128, Quantity: 4, UnitPrice: 2000, TimeLeft: listing.Long},
	})
	resp, err := http.Get(srv.URL + "/api/supply")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Crates []struct {
			ID       int64   `json:"id"`
			PerFavor float64 `json:"per_favor"`
			Icon     string  `json:"icon"`
		} `json:"crates"`
		Writs []struct {
			ID       int64 `json:"id"`
			Net      int64 `json:"net"`
			Customer struct {
				Zone string `json:"zone"`
			} `json:"customer"`
		} `json:"writs"`
		ComputedAt int64 `json:"computed_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, err)
	}
	// (10 crate + 10×60 Earthroot) / 5 Favor; 3300 pouch − 500 writ − 2000 Golden Rod.
	if len(out.Crates) != 1 || out.Crates[0].PerFavor != 122 || len(out.Writs) != 1 || out.Writs[0].Net != 800 ||
		out.Writs[0].Customer.Zone != "Arathi Highlands" || out.ComputedAt == 0 {
		t.Fatalf("%+v", out)
	}
}

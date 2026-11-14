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
	if got := strings.Join(names, ","); !strings.Contains(got, "cancel_or_ride") || len(names) != 9 {
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
	if pc := call("price_check", map[string]any{"item": "Elemental Air", "price": "20s"}); pc["verdict"] == nil {
		t.Fatalf("price_check: %v", pc)
	}
	if so := call("sell_or_hold", map[string]any{"item": "7069", "qty": 12}); so["verdict"] == nil {
		t.Fatalf("sell_or_hold: %v", so)
	}
	if cr := call("cancel_or_ride", map[string]any{"item": "7069", "my_price": "22s89c", "qty": 4}); cr["verdict"] != "ALREADY CHEAPEST" {
		t.Fatalf("cancel_or_ride: %v", cr)
	}
	call("flips", map[string]any{})
	if st := call("collector_status", map[string]any{}); st["houses"] == nil {
		t.Fatalf("status: %v", st)
	}
}

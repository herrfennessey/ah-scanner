package blizzard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	srv       *httptest.Server
	tokens    atomic.Int32
	hits      atomic.Int32
	handler   func(w http.ResponseWriter, r *http.Request, hit int32)
	expiresIn int
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, hit int32)) (*fake, *Client, *[]time.Duration) {
	t.Helper()
	f := &fake{handler: h, expiresIn: 86400}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "id" || secret != "secret" || r.FormValue("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := f.tokens.Add(1)
		fmt.Fprintf(w, `{"access_token":"tok%d","token_type":"bearer","expires_in":%d}`, n, f.expiresIn)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.handler(w, r, f.hits.Add(1))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	c := New(f.srv.URL, f.srv.URL+"/token", "id", "secret", "en_GB", nil)
	var sleeps []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	return f, c, &sleeps
}

const body = `{"auctions":[{"id":1,"item":{"id":7069},"quantity":2,"unit_price":5,"time_left":"LONG"}]}`

func fetch(t *testing.T, c *Client, ims string) (FetchResult, error) {
	t.Helper()
	return c.FetchToFile(context.Background(), "/data/wow/ruleset/1/auctions/2/commodities", nil, ims, filepath.Join(t.TempDir(), "x.json.zst"))
}

func readRaw(t *testing.T, path string) string {
	t.Helper()
	r, err := OpenRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFetch200(t *testing.T) {
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		if r.Header.Get("Authorization") != "Bearer tok1" || r.URL.Query().Get("locale") != "en_GB" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Last-Modified", "Sat, 14 Nov 2026 08:00:00 GMT")
		io.WriteString(w, body)
	})
	fr, err := fetch(t, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if fr.NotModified || fr.SHA256 == "" || fr.LastModified == "" || readRaw(t, fr.Path) != body {
		t.Fatalf("%+v", fr)
	}
	if ParseHTTPTime(fr.LastModified) != time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC).Unix() {
		t.Fatal("last-modified parse")
	}
}

func TestFetch304(t *testing.T) {
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		if r.Header.Get("If-Modified-Since") == "Sat, 14 Nov 2026 08:00:00 GMT" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		io.WriteString(w, body)
	})
	fr, err := fetch(t, c, "Sat, 14 Nov 2026 08:00:00 GMT")
	if err != nil || !fr.NotModified || fr.Path != "" {
		t.Fatalf("%+v %v", fr, err)
	}
}

func TestFetch401RefreshesToken(t *testing.T) {
	f, c, sleeps := newFake(t, func(w http.ResponseWriter, r *http.Request, hit int32) {
		if r.Header.Get("Authorization") == "Bearer tok1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, body)
	})
	fr, err := fetch(t, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if f.tokens.Load() != 2 || readRaw(t, fr.Path) != body || len(*sleeps) != 0 {
		t.Fatalf("tokens=%d sleeps=%v", f.tokens.Load(), *sleeps)
	}
}

func TestTokenRefreshedWhenUnderOneHour(t *testing.T) {
	f, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ int32) { io.WriteString(w, body) })
	now := time.Now()
	c.Now = func() time.Time { return now }
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(22 * time.Hour)
	c.Token(context.Background())
	if f.tokens.Load() != 1 {
		t.Fatal("refreshed too early")
	}
	now = now.Add(90 * time.Minute) // 30 min left
	c.Token(context.Background())
	if f.tokens.Load() != 2 {
		t.Fatal("did not refresh with < 1h left")
	}
}

func TestFetch429Backoff(t *testing.T) {
	f, c, sleeps := newFake(t, func(w http.ResponseWriter, r *http.Request, hit int32) {
		if hit <= 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, body)
	})
	if _, err := fetch(t, c, ""); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if fmt.Sprint(*sleeps) != fmt.Sprint(want) || f.hits.Load() != 4 {
		t.Fatalf("sleeps %v hits %d", *sleeps, f.hits.Load())
	}
}

func TestFetch5xxGivesUpAfterFiveTries(t *testing.T) {
	f, c, sleeps := newFake(t, func(w http.ResponseWriter, r *http.Request, hit int32) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := fetch(t, c, "")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err %v", err)
	}
	if f.hits.Load() != 5 || len(*sleeps) != 4 || (*sleeps)[3] != 8*time.Second {
		t.Fatalf("hits %d sleeps %v", f.hits.Load(), *sleeps)
	}
}

func TestFetch404NotRetried(t *testing.T) {
	f, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, hit int32) { w.WriteHeader(http.StatusNotFound) })
	_, err := fetch(t, c, "")
	if !IsNotFound(err) || f.hits.Load() != 1 {
		t.Fatalf("err %v hits %d", err, f.hits.Load())
	}
}

func TestFetchTruncatedBodyRetried(t *testing.T) {
	f, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, hit int32) {
		if hit == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)+100))
			io.WriteString(w, body[:20])
			return // connection closes short of Content-Length
		}
		io.WriteString(w, body)
	})
	fr, err := fetch(t, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 || readRaw(t, fr.Path) != body {
		t.Fatalf("hits %d", f.hits.Load())
	}
}

func TestParseIndex(t *testing.T) {
	raw := `{"_links":{},"auctions":[
		{"key":{"href":"x"},"name":"Alliance Auction House","id":2},
		{"key":{"href":"x"},"name":{"en_GB":"Horde Auction House"},"id":6},
		{"key":{"href":"x"},"name":"Blackwater Auction House","id":7},
		{"name":"Something","id":9,"faction":{"type":"HORDE"}}]}`
	hs, err := ParseIndex([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(hs)
	want := "[{2 Alliance Auction House alliance} {6 Horde Auction House horde} {7 Blackwater Auction House blackwater} {9 Something horde}]"
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestBadCredentialsFailFast(t *testing.T) {
	f, c, sleeps := newFake(t, func(w http.ResponseWriter, r *http.Request, _ int32) { io.WriteString(w, body) })
	c.ClientSecret = "wrong"
	_, err := fetch(t, c, "")
	var ce *CredentialsError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "BLIZZARD_CLIENT_SECRET") {
		t.Fatalf("want credentials error, got %v", err)
	}
	if f.hits.Load() != 0 || len(*sleeps) != 0 {
		t.Fatalf("should not retry: hits=%d sleeps=%v", f.hits.Load(), *sleeps)
	}
}

func TestItemKeepsPreviewAndSkipsKnownIcon(t *testing.T) {
	var media atomic.Int32
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		if strings.Contains(r.URL.Path, "/media/") {
			media.Add(1)
			io.WriteString(w, `{"assets":[{"key":"icon","value":"i.jpg"}]}`)
			return
		}
		io.WriteString(w, `{"name":"Golden Rod","quality":{"type":"COMMON"},"preview_item":{"name":"Golden Rod","sell_price":{"value":250}}}`)
	})
	it, err := c.Item(context.Background(), 11128, []string{"static-forever-eu"}, true)
	if err != nil || it.Name != "Golden Rod" || string(it.Tooltip) != `{"name":"Golden Rod","sell_price":{"value":250}}` || media.Load() != 0 {
		t.Fatalf("%+v %s err %v media %d", it, it.Tooltip, err, media.Load())
	}
	it, _ = c.Item(context.Background(), 11128, []string{"static-forever-eu"}, false)
	if it.Icon != "i.jpg" || media.Load() != 1 {
		t.Fatalf("icon %q media %d", it.Icon, media.Load())
	}
}

func TestQuestRep(t *testing.T) {
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		if !strings.HasSuffix(r.URL.Path, "/data/wow/quest/94221") {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"id":94221,"rewards":{"reputations":[{"reward":{"name":"Azeroth Commerce Authority","id":2586},"value":75},{"reward":{"name":"Durotar Supply and Logistics","id":2587},"value":75}]},"is_daily":true}`)
	})
	rep, err := c.Quest(context.Background(), 94221, []string{"static-forever-eu"}, 2587)
	if err != nil || rep != 75 {
		t.Fatalf("rep %d err %v", rep, err)
	}
}

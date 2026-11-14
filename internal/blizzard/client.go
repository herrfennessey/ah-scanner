// Package blizzard talks to the Battle.net OAuth endpoint and the (community-discovered)
// WoW Forever auction endpoints.
package blizzard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

type Client struct {
	HTTP         *http.Client
	APIBase      string
	TokenURL     string
	ClientID     string
	ClientSecret string
	Locale       string
	Log          *slog.Logger

	// Sleep and Now are swappable for tests.
	Sleep func(context.Context, time.Duration) error
	Now   func() time.Time
	// Backoff is the wait before retry n (0-based); defaults to 1s, 2s, 4s, 8s.
	Backoff  func(n int) time.Duration
	MaxTries int

	// MinGap spaces requests out (Blizzard allows 100/s per client); 0 = no limit.
	MinGap   time.Duration
	gapMu    sync.Mutex
	nextSlot time.Time

	mu      sync.Mutex
	token   string
	expires time.Time

	// Requests counts HTTP requests made (for the budget log line).
	Requests atomic.Int64
}

func New(apiBase, tokenURL, id, secret, locale string, log *slog.Logger) *Client {
	return &Client{
		HTTP:         &http.Client{Timeout: 5 * time.Minute},
		APIBase:      strings.TrimRight(apiBase, "/"),
		TokenURL:     tokenURL,
		ClientID:     id,
		ClientSecret: secret,
		Locale:       locale,
		Log:          log,
		MinGap:       25 * time.Millisecond, // 40 requests/s
	}
}

// pace blocks until this request's slot under MinGap.
func (c *Client) pace(ctx context.Context) error {
	if c.MinGap <= 0 {
		return nil
	}
	c.gapMu.Lock()
	now := time.Now()
	slot := c.nextSlot
	if slot.Before(now) {
		slot = now
	}
	c.nextSlot = slot.Add(c.MinGap)
	c.gapMu.Unlock()
	if wait := time.Until(slot); wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) backoff(n int) time.Duration {
	if c.Backoff != nil {
		return c.Backoff(n)
	}
	return time.Second << n
}

func (c *Client) maxTries() int {
	if c.MaxTries > 0 {
		return c.MaxTries
	}
	return 5
}

// Token returns a cached access token, refreshing it when under 1 hour of validity remains.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Add(time.Hour).Before(c.expires) {
		return c.token, nil
	}
	return c.refreshLocked(ctx)
}

func (c *Client) invalidate() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func (c *Client) refreshLocked(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.ClientID, c.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.Requests.Add(1)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("token: decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("token: empty access_token")
	}
	c.token = tr.AccessToken
	c.expires = c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, nil
}

// HTTPError is a non-retryable (or retries-exhausted) HTTP status.
type HTTPError struct {
	Status int
	URL    string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d %s", e.URL, e.Status, e.Body)
}

// IsNotFound reports whether err is an HTTP 404.
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

type Response struct {
	NotModified  bool
	LastModified string
	Status       int
}

// Get performs an authenticated GET with retries. consume is called with the body of a 200
// response and may be called again on a retry, so it must reset its output each time.
func (c *Client) Get(ctx context.Context, path string, query url.Values, ifModifiedSince string, consume func(io.Reader) error) (Response, error) {
	if query == nil {
		query = url.Values{}
	}
	if query.Get("locale") == "" && c.Locale != "" {
		query.Set("locale", c.Locale)
	}
	u := c.APIBase + path + "?" + query.Encode()
	refreshed, skipSleep := false, false
	var lastErr error
	for try := 0; try < c.maxTries(); try++ {
		if try > 0 && !skipSleep {
			if err := c.sleep(ctx, c.backoff(try-1)); err != nil {
				return Response{}, err
			}
		}
		tok, err := c.Token(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		skipSleep = false
		resp, retry, err := c.do(ctx, u, tok, ifModifiedSince, consume)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if resp.Status == http.StatusUnauthorized && !refreshed {
			refreshed = true
			c.invalidate()
			try-- // a token refresh does not use up a retry
			skipSleep = true
			continue
		}
		if !retry {
			return resp, err
		}
		if c.Log != nil {
			c.Log.Warn("request failed, will retry", "url", path, "try", try+1, "err", err)
		}
	}
	return Response{}, fmt.Errorf("giving up after %d tries: %w", c.maxTries(), lastErr)
}

func (c *Client) do(ctx context.Context, u, tok, ims string, consume func(io.Reader) error) (Response, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Response{}, false, err
	}
	if err := c.pace(ctx); err != nil {
		return Response{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if ims != "" {
		req.Header.Set("If-Modified-Since", ims)
	}
	c.Requests.Add(1)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, ctx.Err() == nil, err
	}
	defer resp.Body.Close()
	r := Response{Status: resp.StatusCode, LastModified: resp.Header.Get("Last-Modified")}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		r.NotModified = true
		return r, false, nil
	case resp.StatusCode == http.StatusOK:
		if err := consume(resp.Body); err != nil {
			return r, true, fmt.Errorf("read body: %w", err) // truncated / reset: retry
		}
		return r, false, nil
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		redacted := strings.SplitN(u, "?", 2)[0]
		return r, retry, &HTTPError{Status: resp.StatusCode, URL: redacted, Body: strings.TrimSpace(string(b))}
	}
}

// GetJSON fetches a small JSON document into v.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, v any) error {
	_, err := c.Get(ctx, path, query, "", func(r io.Reader) error {
		return json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(v)
	})
	return err
}

// FetchResult describes a downloaded auction body.
type FetchResult struct {
	NotModified  bool
	LastModified string
	Path         string // zstd-compressed raw body
	SHA256       string
	Bytes        int64
}

// FetchToFile streams a body into a zstd file at dst (atomically), hashing it on the way.
// It never holds the body in memory.
func (c *Client) FetchToFile(ctx context.Context, path string, query url.Values, ims, dst string) (FetchResult, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return FetchResult{}, err
	}
	tmp := dst + ".part"
	var fr FetchResult
	resp, err := c.Get(ctx, path, query, ims, func(r io.Reader) error {
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		defer f.Close()
		zw, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(zw, h), r)
		if err != nil {
			zw.Close()
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		fr.SHA256 = hex.EncodeToString(h.Sum(nil))
		fr.Bytes = n
		return f.Sync()
	})
	if err != nil {
		os.Remove(tmp)
		return fr, err
	}
	fr.LastModified = resp.LastModified
	if resp.NotModified {
		fr.NotModified = true
		return fr, nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fr, err
	}
	fr.Path = dst
	return fr, nil
}

// OpenRaw opens a zstd raw body for streaming.
func OpenRaw(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	zr, err := zstd.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &rawReader{zr: zr, f: f}, nil
}

type rawReader struct {
	zr *zstd.Decoder
	f  *os.File
}

func (r *rawReader) Read(p []byte) (int, error) { return r.zr.Read(p) }
func (r *rawReader) Close() error {
	r.zr.Close()
	return r.f.Close()
}

// ParseHTTPTime parses a Last-Modified header to Unix seconds (0 if absent or invalid).
func ParseHTTPTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := http.ParseTime(s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// RulesetPath builds /data/wow/ruleset/{id}/auctions/... paths.
func RulesetPath(rulesetID int, rest string) string {
	return "/data/wow/ruleset/" + strconv.Itoa(rulesetID) + "/auctions/" + rest
}

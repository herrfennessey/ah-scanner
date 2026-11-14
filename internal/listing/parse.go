// Package listing stream-decodes auction house JSON bodies into compact listing records.
//
// The Forever endpoints are undocumented, so the parser ignores unknown fields but fails
// the whole snapshot loudly when a field it relies on is missing.
package listing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type Kind string

const (
	Gear      Kind = "gear"
	Commodity Kind = "commodity"
)

// TimeLeft buckets as returned by the API.
const (
	Short    = "SHORT"
	Medium   = "MEDIUM"
	Long     = "LONG"
	VeryLong = "VERY_LONG"
)

// Listing is one auction in one snapshot. Prices are copper.
type Listing struct {
	AuctionID int64
	ItemID    int64
	Variant   string // random suffix / bonus key, "" when none
	Quantity  int64
	UnitPrice int64 // commodities: unit_price; gear: buyout/quantity (0 if bid-only)
	Buyout    int64
	Bid       int64
	TimeLeft  string
}

type rawItem struct {
	ID         *int64  `json:"id"`
	Rand       *int64  `json:"rand"`
	Suffix     *int64  `json:"suffix"`
	BonusLists []int64 `json:"bonus_lists"`
}

type rawAuction struct {
	ID        *int64   `json:"id"`
	Item      *rawItem `json:"item"`
	Bid       *int64   `json:"bid"`
	Buyout    *int64   `json:"buyout"`
	Quantity  *int64   `json:"quantity"`
	UnitPrice *int64   `json:"unit_price"`
	TimeLeft  *string  `json:"time_left"`
}

// ErrNoAuctions means the body had no "auctions" array at the top level.
var ErrNoAuctions = errors.New(`body has no top-level "auctions" array`)

// Parse streams r and calls fn for every auction. It never holds the whole body in memory.
func Parse(r io.Reader, kind Kind, fn func(Listing) error) (int, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := expectDelim(dec, '{'); err != nil {
		return 0, err
	}
	n := 0
	found := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return n, fmt.Errorf("read key: %w", err)
		}
		key, _ := tok.(string)
		if key != "auctions" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return n, fmt.Errorf("skip %q: %w", key, err)
			}
			continue
		}
		found = true
		if err := expectDelim(dec, '['); err != nil {
			return n, fmt.Errorf("auctions: %w", err)
		}
		for dec.More() {
			var ra rawAuction
			if err := dec.Decode(&ra); err != nil {
				return n, fmt.Errorf("auction #%d: %w", n, err)
			}
			l, err := convert(ra, kind)
			if err != nil {
				return n, fmt.Errorf("auction #%d: %w", n, err)
			}
			if err := fn(l); err != nil {
				return n, err
			}
			n++
		}
		if err := expectDelim(dec, ']'); err != nil {
			return n, fmt.Errorf("auctions end: %w", err)
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return n, err
	}
	if !found {
		return 0, ErrNoAuctions
	}
	return n, nil
}

// ParseAll collects every listing; convenient for tests and probe.
func ParseAll(r io.Reader, kind Kind) ([]Listing, error) {
	var out []Listing
	_, err := Parse(r, kind, func(l Listing) error { out = append(out, l); return nil })
	return out, err
}

func expectDelim(dec *json.Decoder, d json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("expected %q: %w", d, err)
	}
	if got, ok := tok.(json.Delim); !ok || got != d {
		return fmt.Errorf("expected %q, got %v", d, tok)
	}
	return nil
}

func convert(ra rawAuction, kind Kind) (Listing, error) {
	var missing []string
	if ra.ID == nil {
		missing = append(missing, "id")
	}
	if ra.Item == nil || ra.Item.ID == nil {
		missing = append(missing, "item.id")
	}
	if ra.Quantity == nil {
		missing = append(missing, "quantity")
	}
	if ra.TimeLeft == nil {
		missing = append(missing, "time_left")
	}
	if kind == Commodity && ra.UnitPrice == nil {
		missing = append(missing, "unit_price")
	}
	if kind == Gear && ra.Buyout == nil && ra.Bid == nil && ra.UnitPrice == nil {
		missing = append(missing, "buyout|bid")
	}
	if len(missing) > 0 {
		return Listing{}, fmt.Errorf("missing required field(s) %s", strings.Join(missing, ", "))
	}
	l := Listing{
		AuctionID: *ra.ID,
		ItemID:    *ra.Item.ID,
		Quantity:  *ra.Quantity,
		TimeLeft:  *ra.TimeLeft,
		Variant:   variantKey(ra.Item),
	}
	if l.Quantity <= 0 {
		return Listing{}, fmt.Errorf("auction %d: quantity %d", l.AuctionID, l.Quantity)
	}
	if ra.Bid != nil {
		l.Bid = *ra.Bid
	}
	if ra.Buyout != nil {
		l.Buyout = *ra.Buyout
	}
	switch {
	case ra.UnitPrice != nil:
		l.UnitPrice = *ra.UnitPrice
		if l.Buyout == 0 {
			l.Buyout = l.UnitPrice * l.Quantity
		}
	case l.Buyout > 0:
		l.UnitPrice = l.Buyout / l.Quantity
	}
	return l, nil
}

// variantKey distinguishes random-suffix / bonus variants of the same item ID.
func variantKey(it *rawItem) string {
	var parts []string
	if it.Rand != nil && *it.Rand != 0 {
		parts = append(parts, "r"+strconv.FormatInt(*it.Rand, 10))
	}
	if it.Suffix != nil && *it.Suffix != 0 {
		parts = append(parts, "s"+strconv.FormatInt(*it.Suffix, 10))
	}
	if len(it.BonusLists) > 0 {
		b := append([]int64(nil), it.BonusLists...)
		sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
		s := make([]string, len(b))
		for i, v := range b {
			s[i] = strconv.FormatInt(v, 10)
		}
		parts = append(parts, "b"+strings.Join(s, ":"))
	}
	return strings.Join(parts, ",")
}

// MinRemaining is the minimum time an auction still had when it last showed this bucket.
func MinRemaining(tl string) int64 {
	switch tl {
	case Medium:
		return 30 * 60
	case Long:
		return 2 * 3600
	case VeryLong:
		return 12 * 3600
	default:
		return 0
	}
}

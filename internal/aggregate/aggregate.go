// Package aggregate computes per-item price statistics for one snapshot.
package aggregate

import (
	"sort"

	"github.com/herrfennessey/ah-scanner/internal/listing"
)

type Key struct {
	ItemID  int64
	Variant string
}

// Stat is one item's listed quantity and unit-weighted price points in a snapshot.
// Prices are 0 when no listing of the item has a buyout (bid-only gear).
type Stat struct {
	Key
	ListedQty  int64
	NAuctions  int64
	MinUnit    int64
	P20Unit    int64 // price at which the cheapest 20% of listed units clear
	MedianUnit int64 // unit-weighted median
}

type priced struct{ price, qty int64 }

// Builder accumulates listings one at a time so the caller can stream.
type Builder struct {
	m map[Key]*acc
}

type acc struct {
	qty, n int64
	prices []priced
}

func NewBuilder() *Builder { return &Builder{m: map[Key]*acc{}} }

func (b *Builder) Add(l listing.Listing) {
	k := Key{l.ItemID, l.Variant}
	a := b.m[k]
	if a == nil {
		a = &acc{}
		b.m[k] = a
	}
	a.qty += l.Quantity
	a.n++
	if l.UnitPrice > 0 {
		a.prices = append(a.prices, priced{l.UnitPrice, l.Quantity})
	}
}

func (b *Builder) Stats() []Stat {
	out := make([]Stat, 0, len(b.m))
	for k, a := range b.m {
		s := Stat{Key: k, ListedQty: a.qty, NAuctions: a.n}
		if len(a.prices) > 0 {
			sort.Slice(a.prices, func(i, j int) bool { return a.prices[i].price < a.prices[j].price })
			var total int64
			for _, p := range a.prices {
				total += p.qty
			}
			s.MinUnit = a.prices[0].price
			s.P20Unit = quantile(a.prices, total, 0.2)
			s.MedianUnit = quantile(a.prices, total, 0.5)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ItemID != out[j].ItemID {
			return out[i].ItemID < out[j].ItemID
		}
		return out[i].Variant < out[j].Variant
	})
	return out
}

// quantile returns the price of the unit at fraction q of the cumulative quantity (sorted asc).
func quantile(ps []priced, total int64, q float64) int64 {
	target := int64(float64(total)*q + 0.999999) // ceil, at least the first unit
	if target < 1 {
		target = 1
	}
	var cum int64
	for _, p := range ps {
		cum += p.qty
		if cum >= target {
			return p.price
		}
	}
	return ps[len(ps)-1].price
}

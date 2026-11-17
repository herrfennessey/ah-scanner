package report

import (
	"fmt"
	"math/rand"
	"testing"
)

// randomItems makes markets of every shape: thin and busy, steady and sliding, volatile, with
// and without evidence, cheap listings on either faction house.
func randomItems(n int, seed int64) []FlipItem {
	r := rand.New(rand.NewSource(seed))
	var items []FlipItem
	for i := 0; i < n; i++ {
		fair := int64(200 + r.Intn(50000))
		stat := func() *FlipStat {
			per := float64(1 + r.Intn(400))
			s := &FlipStat{Fair: fair, Sales: int64(5 + r.Intn(300)), SalesPerDay: per / float64(1+r.Intn(5)), RobustPerDay: per,
				Volatile: r.Intn(6) == 0}
			p := int64(float64(fair) * (0.4 + r.Float64()))
			for l := 0; l < 1+r.Intn(4); l++ {
				s.Book = append(s.Book, FlipLevel{P: p, Q: int64(1 + r.Intn(60))})
				p = int64(float64(p) * (1.02 + r.Float64()*0.3))
			}
			s.Min = s.Book[0].P
			for _, l := range s.Book {
				s.Listed += l.Q
			}
			if r.Intn(2) == 0 {
				s.Evidence = true
				s.Trend = (r.Float64() - 0.6) / 2
				for k := 0; k < r.Intn(6); k++ {
					s.Sold12 = addLevel(s.Sold12, int64(float64(fair)*(0.7+r.Float64()*0.5)), int64(1+r.Intn(10)), 1)
				}
				s.Sold48 = append(s.Sold48, s.Sold12...)
				for k := 0; k < r.Intn(10); k++ {
					s.Sold48 = addLevel(s.Sold48, int64(float64(fair)*(0.7+r.Float64()*0.5)), int64(1+r.Intn(20)), 1)
				}
				if r.Intn(2) == 0 {
					s.Front = &FrontStat{N: float64(r.Intn(30))}
					for h := range s.Front.Sold {
						s.Front.Sold[h] = s.Front.N * r.Float64()
						s.Front.Under[h] = s.Front.N * r.Float64()
					}
				}
			}
			return s
		}
		items = append(items, FlipItem{Item: int64(1000 + i), Name: fmt.Sprint("item ", i), Deposit: int64(r.Intn(300)),
			Houses: map[string]*FlipStat{"horde": stat(), "blackwater": stat(), "alliance": stat()}})
	}
	return items
}

func TestStylesNest(t *testing.T) {
	// Whatever a less attentive player is offered, a more attentive one is offered too: they can
	// always play it the same way.
	key := func(f Flip) string { return fmt.Sprint(f.ItemID, f.BuyHouse, f.SellHouse) }
	for seed := int64(1); seed <= 20; seed++ {
		items := randomItems(150, seed)
		offered := map[Style]map[string]Flip{}
		for _, s := range Styles {
			offered[s] = map[string]Flip{}
			for _, f := range planRisk(items, 400, s) {
				offered[s][key(f)] = f
			}
		}
		for _, pair := range [][2]Style{{StyleAway, StyleCasual}, {StyleCasual, StyleActive}} {
			for k, f := range offered[pair[0]] {
				if _, ok := offered[pair[1]][k]; !ok {
					t.Errorf("seed %d: %s is offered %s (%s) but %s isn't", seed, pair[0], f.Name, f.Play, pair[1])
				}
			}
		}
	}
}

func TestEachStyleGetsItsPlays(t *testing.T) {
	seen := map[Style]map[Play]bool{}
	for _, s := range Styles {
		seen[s] = map[Play]bool{}
		for _, f := range planRisk(randomItems(400, 7), 400, s) {
			seen[s][f.Play] = true
		}
	}
	if len(seen[StyleAway]) != 1 || !seen[StyleAway][PlayHold] {
		t.Fatalf("away: %v", seen[StyleAway])
	}
	if seen[StyleCasual][PlayCamp] || !seen[StyleCasual][PlayCheckIn] {
		t.Fatalf("casual: %v", seen[StyleCasual])
	}
	if !seen[StyleActive][PlayCamp] {
		t.Fatalf("active never camps: %v", seen[StyleActive])
	}
}

func TestRelistQueue(t *testing.T) {
	// 48 a day, 19 hours left: buyers get through 38 units, so relist once more than 38 are
	// cheaper than yours, however big your own stack.
	if q := RelistQueue(48, 19); q != 38 {
		t.Fatalf("%d", q)
	}
	// Hot Lion Chops on Neutral: 133 a day, 19 hours left.
	if q := RelistQueue(133, 19); q != 105 {
		t.Fatalf("%d", q)
	}
	if q := RelistQueue(1, 5); q != 0 {
		t.Fatalf("%d", q)
	}
}

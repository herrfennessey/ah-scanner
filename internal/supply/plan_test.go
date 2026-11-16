package supply

import (
	"math"
	"reflect"
	"testing"
)

const herbFill = "Use: Fill the crate with any bundle from the following list:\n- 20 Peacebloom\n- 10 Earthroot"

func TestParseFill(t *testing.T) {
	got := ParseFill(herbFill)
	want := []Bundle{{20, "Peacebloom"}, {10, "Earthroot"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if got := ParseFill("Use: Open the container."); len(got) != 0 {
		t.Fatalf("garbage parsed: %+v", got)
	}
}

func TestBuyCost(t *testing.T) {
	book := []Level{{100, 5}, {200, 10}}
	if c, ok := BuyCost(book, 8); !ok || c != 1100 {
		t.Fatalf("8 units: %d %v", c, ok)
	}
	if _, ok := BuyCost(book, 20); ok {
		t.Fatal("20 units should not be coverable")
	}
}

func TestCrateFavor(t *testing.T) {
	for name, want := range map[string]int{
		"Waylaid Crate: Journeyman Fabrics":   20,
		"Waylaid Crate: Earthly Expert Herbs": 15,
		"Waylaid Crate: Apprentice Ore":       5,
		"Waylaid Crate: Expert Parts":         30,
		"Waylaid Crate: Artisan Parts":        0,
	} {
		if got := CrateFavor(name); got != want {
			t.Errorf("%s: %d, want %d", name, got, want)
		}
	}
}

// prices builds a BookPrices from item id → (name, book).
func prices(items map[int64]struct {
	name string
	book []Level
}) *BookPrices {
	p := &BookPrices{Books: map[int64][]Level{}, ByName: map[string][]int64{}}
	for id, it := range items {
		p.Books[id] = it.book
		p.ByName[it.name] = append(p.ByName[it.name], id)
	}
	return p
}

type item = struct {
	name string
	book []Level
}

func TestCrateRanking(t *testing.T) {
	p := prices(map[int64]item{
		248682: {"Waylaid Crate: Apprentice Herbs", []Level{{10, 3}}},
		248686: {"Waylaid Crate: Journeyman Herbs", []Level{{20, 3}}},
		248699: {"Waylaid Crate: Artisan Curiosities", []Level{{1, 3}}},
		248685: {"Waylaid Crate: Apprentice Curiosities", []Level{{1, 3}}},
		2447:   {"Peacebloom", []Level{{50, 100}}},
		2449:   {"Earthroot", []Level{{60, 100}}},
		3820:   {"Stranglekelp", []Level{{30, 100}}},
	})
	in := Inputs{Prices: p, Names: map[int64]string{
		248682: "Waylaid Crate: Apprentice Herbs", 248686: "Waylaid Crate: Journeyman Herbs",
		248699: "Waylaid Crate: Artisan Curiosities", 248685: "Waylaid Crate: Apprentice Curiosities",
	}, Fills: map[int64]string{
		248682: herbFill, // 20×50=1000 vs 10×60=600 → Earthroot
		248686: "Use: Fill the crate with any bundle from the following list:\n- 10 Stranglekelp",
		248699: "Use: Fill the crate with any bundle from the following list:\n- 10 Stranglekelp",
		248685: "Use: Open it.",
	}}
	r := Plan(in)
	if len(r.Crates) != 4 {
		t.Fatalf("%d crates", len(r.Crates))
	}
	// Journeyman: (20 + 300) / 10 = 32 per Favor; Apprentice: (10 + 600) / 5 = 122.
	if r.Crates[0].ID != 248686 || r.Crates[0].Total != 320 || r.Crates[0].PerFavor != 32 {
		t.Fatalf("first %+v", r.Crates[0])
	}
	c := r.Crates[1]
	if c.ID != 248682 || c.Best == nil || c.Best.Name != "Earthroot" || c.Best.Cost != 600 || c.PerFavor != 122 || len(c.Options) != 2 {
		t.Fatalf("second %+v", c)
	}
	for _, c := range r.Crates[2:] {
		if c.PerFavor != 0 || c.Note == "" {
			t.Fatalf("unrankable crate should come last with a note: %+v", c)
		}
	}
}

func writInputs(p *BookPrices, names map[int64]string, rep map[int64]int) Inputs {
	return Inputs{Prices: p, Names: names, Rep: rep, Pouch: map[string]int64{"Journeyman": 3300, "Expert": 7000, "Artisan": 7500}}
}

func TestWritNet(t *testing.T) {
	p := prices(map[int64]item{
		264021: {"Craftsman's Writ: Golden Rod", []Level{{500, 1}}},
		11128:  {"Golden Rod", []Level{{2000, 4}}},
	})
	r := Plan(writInputs(p, map[int64]string{264021: "Craftsman's Writ: Golden Rod"}, map[int64]int{264021: 75}))
	w := r.Writs[0]
	if !w.OK || w.Net != 800 || w.Tier != "Journeyman" || w.Rep != 75 || math.Abs(w.PerRep+10.667) > 0.01 || w.ItemID != 11128 {
		t.Fatalf("%+v", w)
	}
	if w.Customer.Zone != "Arathi Highlands" || w.Customer.NPC != "Muggol Breezebeard" || w.Continent != "Eastern Kingdoms" {
		t.Fatalf("customer %+v continent %q", w.Customer, w.Continent)
	}
}

func TestWritNotEnoughOnAH(t *testing.T) {
	p := prices(map[int64]item{
		264021: {"Craftsman's Writ: Golden Rod", []Level{{500, 1}}},
		264037: {"Craftsman's Writ: Crafted Solid Shot", []Level{{10, 1}}},
		8069:   {"Crafted Solid Shot", []Level{{1, 50}}}, // 200 needed
	})
	r := Plan(writInputs(p, map[int64]string{264021: "Craftsman's Writ: Golden Rod", 264037: "Craftsman's Writ: Crafted Solid Shot"},
		map[int64]int{264021: 75, 264037: 75}))
	for _, w := range r.Writs {
		if w.OK {
			t.Fatalf("no writ is coverable: %+v", w)
		}
		if w.ID == 264037 && w.ItemOK {
			t.Fatalf("50 of 200 Solid Shot listed must not count as buyable: %+v", w)
		}
	}
}

func TestSolidShotQty(t *testing.T) {
	p := prices(map[int64]item{
		264037: {"Craftsman's Writ: Crafted Solid Shot", []Level{{10, 1}}},
		8069:   {"Crafted Solid Shot", []Level{{1, 300}}},
	})
	r := Plan(writInputs(p, map[int64]string{264037: "Craftsman's Writ: Crafted Solid Shot"}, map[int64]int{264037: 75}))
	if w := r.Writs[0]; w.Qty != 200 || w.ItemCost != 200 || !w.OK {
		t.Fatalf("%+v", w)
	}
}

func TestWritSortOrder(t *testing.T) {
	p := prices(map[int64]item{
		264021: {"Craftsman's Writ: Golden Rod", []Level{{500, 1}}},
		11128:  {"Golden Rod", []Level{{2000, 4}}},
		264014: {"Craftsman's Writ: Shadow Protection Potion", []Level{{100, 1}}},
		6048:   {"Shadow Protection Potion", []Level{{100, 4}}},
		264013: {"Craftsman's Writ: Lesser Mana Potion", []Level{{100, 1}}}, // item not on the AH
	})
	r := Plan(writInputs(p, map[int64]string{264021: "Craftsman's Writ: Golden Rod",
		264014: "Craftsman's Writ: Shadow Protection Potion", 264013: "Craftsman's Writ: Lesser Mana Potion"},
		map[int64]int{264021: 75, 264014: 75, 264013: 75}))
	if r.Writs[0].ID != 264014 || r.Writs[1].ID != 264021 || r.Writs[2].ID != 264013 || r.Writs[2].OK {
		t.Fatalf("order %d %d %d", r.Writs[0].ID, r.Writs[1].ID, r.Writs[2].ID)
	}
}

func TestAmbiguousItemName(t *testing.T) {
	p := prices(map[int64]item{
		100: {"Mana Potion", nil},
		200: {"Mana Potion", []Level{{50, 2}}},
	})
	if id, ok := p.ItemID("Mana Potion"); !ok || id != 200 {
		t.Fatalf("listed copy should win: %d", id)
	}
	p.Books[200] = nil
	if id, _ := p.ItemID("Mana Potion"); id != 100 {
		t.Fatalf("no listings: lowest id should win, got %d", id)
	}
}

func TestWritRepUnknown(t *testing.T) {
	p := prices(map[int64]item{
		264062: {"Craftsman's Writ: Mana Potion", []Level{{100, 1}}},
		3827:   {"Mana Potion", []Level{{100, 1}}},
		264131: {"Craftsman's Writ: Huge Thorium Battleaxe", []Level{{100, 1}}},
		15294:  {"Huge Thorium Battleaxe", []Level{{100, 1}}},
	})
	r := Plan(writInputs(p, map[int64]string{264062: "Craftsman's Writ: Mana Potion", 264131: "Craftsman's Writ: Huge Thorium Battleaxe"}, nil))
	for _, w := range r.Writs {
		switch w.ID {
		case 264062: // Expert in the dataset (Riverglades, Spek)
			if w.Tier != "Expert" || w.Rep != 125 || w.Pouch != 7000 {
				t.Fatalf("dataset tier: %+v", w)
			}
		case 264131: // no customer row and no quest rep
			if w.Tier != "" || w.Rep != 0 || w.PerRep != 0 {
				t.Fatalf("unknown tier: %+v", w)
			}
		}
	}
}

func TestSealedWrit(t *testing.T) {
	p := prices(map[int64]item{
		262889: {"Sealed Expert's Writ", []Level{{2000, 3}}},
		264062: {"Craftsman's Writ: Mana Potion", []Level{{100, 1}}},
		3827:   {"Mana Potion", []Level{{1000, 1}}},
		// The named writ isn't listed, but its item is: it still counts toward the average.
		264060: {"Craftsman's Writ: Catseye Draught", nil},
		3825:   {"Catseye Draught", []Level{{3000, 1}}},
	})
	r := Plan(writInputs(p, map[int64]string{262889: "Sealed Expert's Writ",
		264062: "Craftsman's Writ: Mana Potion", 264060: "Craftsman's Writ: Catseye Draught"},
		map[int64]int{264062: 125, 264060: 125}))
	var s *Writ
	for i := range r.Sealed {
		if r.Sealed[i].ID == 262889 {
			s = &r.Sealed[i]
		}
	}
	// 7000 pouch − 2000 sealed − mean(1000, 3000)
	if s == nil || !s.OK || s.Net != 3000 || s.Rep != 125 {
		t.Fatalf("%+v", s)
	}
}

func TestReceiptBenchmark(t *testing.T) {
	p := prices(map[int64]item{
		248686: {"Waylaid Crate: Journeyman Herbs", []Level{{1000, 3}}},
		248687: {"Waylaid Crate: Journeyman Ore", []Level{{1, 3}}}, // cheapest empty crate; its fill isn't priced
		3820:   {"Stranglekelp", []Level{{10, 10}}},
	})
	in := Inputs{Prices: p, Names: map[int64]string{248686: "Waylaid Crate: Journeyman Herbs", 248687: "Waylaid Crate: Journeyman Ore"},
		Fills: map[int64]string{248686: "Use: Fill the crate with any bundle from the following list:\n- 10 Stranglekelp"}}
	r := Plan(in)
	// best PerFavor = (1000 + 100)/10 = 110; cheapest empty crate = 1 → (15×110 + 1)/50 = 33.02
	if !r.Receipt.OK || math.Abs(r.Receipt.PerRep-33.02) > 0.001 {
		t.Fatalf("%+v best %+v", r.Receipt, r.Crates[0])
	}
}

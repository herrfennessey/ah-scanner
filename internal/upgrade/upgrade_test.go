package upgrade

import "testing"

func reset() {
	checkedMu.Lock()
	checked = map[string]Checked{}
	checkedMu.Unlock()
}

// One fixed-stat item per slot (ItemSparse allocation, item level and quality → the stat
// Blizzard's item API shows), from the 663-item harvest of 2026-11-18. If a client build
// changes the slot columns, this fails and `go generate` alone isn't enough.
func TestColumnsReproduceBlizzardStats(t *testing.T) {
	for _, c := range []struct {
		id        int64
		name, inv string
		ilvl, q   int
		share     int
		stat      string
		blizzard  float64
	}{
		{763, "Ice-covered Bracers", "WRIST", 12, 2, 10000, "AGILITY", 1},
		{935, "Night Watch Shortsword", "WEAPON", 20, 3, 8000, "STAMINA", 4},
		{1077, "Defias Mage Ring", "FINGER", 26, 2, 8000, "INTELLECT", 4},
		{1913, "Studded Blackjack", "WEAPONMAINHAND", 12, 2, 10000, "STRENGTH", 1},
		{2300, "Embossed Leather Vest", "CHEST", 12, 2, 10000, "STAMINA", 2},
		{2303, "Handstitched Leather Pants", "LEGS", 10, 2, 10000, "AGILITY", 1},
		{2307, "Fine Leather Boots", "FEET", 18, 2, 7500, "AGILITY", 3},
		{2308, "Fine Leather Cloak", "CLOAK", 15, 2, 10000, "STAMINA", 2},
		{2312, "Fine Leather Gloves", "HAND", 15, 2, 6667, "INTELLECT", 2},
		{2546, "Royal Frostmane Girdle", "WAIST", 13, 2, 10000, "INTELLECT", 2},
		{2572, "Red Linen Robe", "ROBE", 10, 2, 10000, "INTELLECT", 1},
		{3041, `"Mage-Eye" Blunderbuss`, "RANGEDRIGHT", 31, 2, 10000, "AGILITY", 4},
		{3480, "Rough Bronze Shoulders", "SHOULDER", 22, 2, 8000, "STAMINA", 4},
		{3487, "Heavy Copper Broadsword", "TWOHWEAPON", 19, 2, 10000, "STAMINA", 6},
		{3836, "Green Iron Helm", "HEAD", 34, 2, 8462, "STAMINA", 11},
		{4820, "Guardian Buckler", "SHIELD", 25, 2, 6000, "STAMINA", 3},
		{4837, "Strength of Will", "HOLDABLE", 30, 2, 10000, "SPIRIT", 6},
		{5029, "Talisman of the Naga Lord", "NECK", 47, 2, 1818, "STRENGTH", 2},
		{11303, "Fine Shortbow", "RANGED", 16, 2, 10000, "AGILITY", 1},
	} {
		col, ok := slotColumn(c.inv)
		if !ok {
			t.Errorf("%s: %s is not a harvested slot", c.name, c.inv)
			continue
		}
		if got, _ := statAt(c.share, c.ilvl, c.q, col); got != c.blizzard {
			t.Errorf("%s (%d, %s): %s %v, Blizzard shows %v", c.name, c.id, c.inv, c.stat, got, c.blizzard)
		}
	}
}

// Sine's Scaled Leather Shoulders of the Bear: the armory shows +6 Strength, +6 Stamina.
func TestSuffixMatchesTheArmory(t *testing.T) {
	reset()
	st, exact, _ := SuffixStats("b12722", 32, 2, "SHOULDER")
	if !exact || st["STRENGTH"] != 6 || st["STAMINA"] != 6 {
		t.Fatalf("%v exact=%v", st, exact)
	}
	if lines, exact, _ := SuffixLines("b12722", 32, 2, "TRINKET"); exact || len(lines) != 2 || lines[0].Min > lines[0].Max {
		t.Fatalf("an unharvested slot gives a range: %+v exact=%v", lines, exact)
	}
}

func TestCheckConfirmsOrCorrects(t *testing.T) {
	reset()
	base := map[string]float64{}
	// The guess (column 1) matches the armory.
	if c, ok := Check("SHOULDER", "b12722", 32, 2, base, map[string]float64{"STRENGTH": 6, "STAMINA": 6}, "Sine"); !ok || c.Status != "checked" {
		t.Fatalf("%+v %v", c, ok)
	}
	// A trinket reading like column 2 (7 × 0.6666 = 5) corrects the guess (column 1: 6).
	if c, _ := Check("TRINKET", "b12722", 32, 2, base, map[string]float64{"STRENGTH": 5, "STAMINA": 5}, "Alt"); c.Status != "corrected" || c.Column != 2 {
		t.Fatalf("%+v", c)
	}
	if col, exact := slotColumn("TRINKET"); col != 2 || !exact {
		t.Fatalf("corrected column not used: %d %v", col, exact)
	}
	// One odd item doesn't undo a confirmed slot.
	if c, _ := Check("SHOULDER", "b12722", 32, 2, base, map[string]float64{"STRENGTH": 99}, "Odd"); c.Status != "checked" {
		t.Fatalf("%+v", c)
	}
}

func item(id int64, inv string, price int64, req int, stats map[string]float64) Item {
	return Item{ID: id, Name: inv, InvType: inv, ReqLevel: req, Price: price, Stats: stats}
}

func TestFindRanksAndFilters(t *testing.T) {
	w := Presets["feral"].Weights
	eq := map[string][]Item{
		"HEAD":      {item(1, "HEAD", 0, 30, map[string]float64{"AGILITY": 5})},
		"FINGER":    {item(2, "FINGER", 0, 30, map[string]float64{"STAMINA": 10}), item(3, "FINGER", 0, 30, map[string]float64{"STRENGTH": 2})},
		"MAIN_HAND": {{ID: 4, InvType: "TWOHWEAPON", DPS: 30, Stats: map[string]float64{}}},
	}
	cands := []Item{
		item(10, "HEAD", 10000, 38, map[string]float64{"AGILITY": 15}),   // +20 for 1g
		item(11, "HEAD", 1000, 25, map[string]float64{"AGILITY": 8}),     // +6 for 10s: best per gold
		item(12, "FINGER", 5000, 35, map[string]float64{"STRENGTH": 6}),  // vs the weaker ring (Str 2 → 4): +8
		item(13, "HOLDABLE", 100, 20, map[string]float64{"AGILITY": 20}), // off hand while wielding a two-hander
		item(14, "HEAD", 100, 38, map[string]float64{"AGILITY": 6}),      // +2: under the minimum gain
		item(15, "HEAD", 1000, 41, map[string]float64{"AGILITY": 30}),    // level 41: only with Ahead
		item(16, "HEAD", 900000, 38, map[string]float64{"AGILITY": 40}),  // over the max price
	}
	o := Options{Level: 38, MinGain: 3, MaxPrice: 500000, HealthPerSt: 10}
	rows := Find(eq, cands, w, o)
	ids := []int64{}
	for _, r := range rows {
		ids = append(ids, r.Item.ID)
	}
	if len(rows) != 3 || rows[0].Item.ID != 11 || rows[0].PerGold != 60 {
		t.Fatalf("per gold: %v %+v", ids, rows)
	}
	for _, r := range rows {
		if r.Item.ID == 12 && (r.Gain != 8 || r.Replaces[0].ID != 3) {
			t.Fatalf("ring should replace the weaker one: %+v", r)
		}
	}
	o.Ahead = 3
	ahead := false
	got := Find(eq, cands, w, o)
	for _, r := range got {
		ahead = ahead || (r.Item.ID == 15 && r.Ahead)
	}
	if len(got) != 4 || !ahead {
		t.Fatalf("ahead: %+v", got)
	}
	o.Ahead, o.BuyOnce, o.MaxBelow = 0, true, 10
	got = Find(eq, cands, w, o)
	if len(got) != 2 || got[0].Item.ID != 10 { // item 11 (level 25) is too far below; biggest gain first
		t.Fatalf("buy once: %+v", got)
	}
}

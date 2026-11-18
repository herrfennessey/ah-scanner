// Package upgrade finds cost-effective gear upgrades for a character on the auction house.
//
// Scoring is pure (no DB, no HTTP): internal/web loads the character and the listings and calls
// Find. Suffix stats come from the client tables in data.go (go generate).
package upgrade

//go:generate go run gen.go -build 1.60.1.70959

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// alloc is one stat of a suffix bonus list: stat id and its share of the budget in 1/10000.
type alloc struct{ stat, share int }

// Preset is a named set of stat weights in score points (AP for Feral).
type Preset struct {
	Name    string             `json:"name"`
	Weights map[string]float64 `json:"weights"` // stat type (STRENGTH, …) or "DPS"
	Sources map[string]string  `json:"sources"`
}

// Presets ships Feral (cat); the weights and their sources are in the design doc.
var Presets = map[string]Preset{
	"feral": {
		Name: "Feral (cat)",
		Weights: map[string]float64{"DPS": 14, "STRENGTH": 2, "AGILITY": 2, "ATTACK_POWER": 1,
			"CRIT_RATING": 20.0 / 14, "HIT_RATING": 2, "STAMINA": 1, "INTELLECT": 0, "SPIRIT": 0},
		Sources: map[string]string{
			"DPS":          "14 AP = 1 weapon DPS (guide 2)",
			"STRENGTH":     "1 Str = 2 AP (guides 1 and 2)",
			"AGILITY":      "Agility = Strength (guide 1)",
			"ATTACK_POWER": "unit",
			"CRIT_RATING":  "1% crit ≈ 20 AP (guide 1), 14 rating per 1% (client combat ratings)",
			"HIT_RATING":   "estimate: 1% hit ≈ 20 AP, 10 rating per 1%; upper bound below the raid cap",
			"STAMINA":      "our default for solo levelling: half a Strength point (guide 1: last for cat DPS, first for tanking; solo you take every hit). Adjust to taste",
			"INTELLECT":    "0 by default: mana for shifting out and healing, no damage (guide 1). Adjust to taste",
			"SPIRIT":       "0 by default: out-of-combat regeneration, no damage (guide 1). Adjust to taste",
		},
	},
}

// Druid proficiencies from the client's SkillRaceClassInfo (build 1.60.1.70959): armor Cloth,
// Leather; weapons Daggers, Fist Weapons, Maces, Two-Handed Maces, Staves, Polearms (level 20).
// Armor subclass 0 (Miscellaneous: rings, necks, cloaks…) and 8 (Idol, worn by Sine) too.
// Only druids so far: other classes need their own sourced proficiencies and presets.
var druidArmor = map[int]bool{0: true, 1: true, 2: true, 8: true}
var druidWeapons = map[int]bool{4: true, 5: true, 6: true, 10: true, 13: true, 15: true}

// Usable reports whether a druid can equip an item of this class/subclass.
func Usable(class, subclass int) bool {
	switch class {
	case 4:
		return druidArmor[subclass]
	case 2:
		return druidWeapons[subclass]
	}
	return false
}

// Slot groups inventory types into the character's equipment slots.
func Slot(invType string) string {
	switch invType {
	case "HEAD", "NECK", "SHOULDER", "WAIST", "LEGS", "FEET", "WRIST", "HAND":
		return invType
	case "CHEST", "ROBE":
		return "CHEST"
	case "CLOAK":
		return "BACK"
	case "FINGER":
		return "FINGER"
	case "TRINKET":
		return "TRINKET"
	case "TWOHWEAPON", "WEAPON", "WEAPONMAINHAND":
		return "MAIN_HAND"
	case "HOLDABLE", "WEAPONOFFHAND", "SHIELD":
		return "OFF_HAND"
	case "RELIC", "RANGED", "RANGEDRIGHT", "THROWN":
		return "RANGED"
	}
	return ""
}

// harvested: inventory types whose column was solved from fixed-stat items. For every item with
// both a Blizzard preview (stats) and an ItemSparse row (StatPercentEditor allocations), exactly
// one column reproduced the API stats for 100% of that slot's items (663 items, build
// 1.60.1.70959, 2026-11-18): e.g. HEAD 68/68 on 0, SHOULDER 53/53 on 1, WRIST 43/43 on 2,
// WEAPON 39/39 on 3, RANGEDRIGHT 7/7 on 4. TRINKET, RELIC, THROWN and WEAPONOFFHAND had no samples.
var harvested = map[string]bool{"HEAD": true, "CHEST": true, "ROBE": true, "LEGS": true, "TWOHWEAPON": true,
	"SHOULDER": true, "HAND": true, "WAIST": true, "FEET": true, "WRIST": true, "CLOAK": true, "NECK": true,
	"FINGER": true, "HOLDABLE": true, "SHIELD": true, "WEAPON": true, "WEAPONMAINHAND": true, "RANGED": true, "RANGEDRIGHT": true}

// guessColumn is the RandPropPoints column for an inventory type; ok is false for unknown types.
func guessColumn(invType string) (int, bool) {
	switch invType {
	case "HEAD", "CHEST", "ROBE", "LEGS", "TWOHWEAPON":
		return 0, true
	case "SHOULDER", "WAIST", "FEET", "HAND", "TRINKET":
		return 1, true
	case "WRIST", "NECK", "CLOAK", "FINGER", "HOLDABLE", "SHIELD", "WEAPONOFFHAND":
		return 2, true
	case "WEAPON", "WEAPONMAINHAND":
		return 3, true
	case "RANGED", "RANGEDRIGHT", "THROWN", "RELIC":
		return 4, true
	}
	return 0, false
}

// Checked is what a real character's gear proved about one inventory type's column.
type Checked struct {
	InvType string `json:"inv_type"`
	Column  int    `json:"column"`
	By      string `json:"by"`     // "Sine Wave: Scaled Leather Shoulders of the Bear"
	Status  string `json:"status"` // "checked", "corrected" (the guess was wrong), "mismatch"
	Detail  string `json:"detail,omitempty"`
}

var (
	checkedMu sync.Mutex
	checked   = map[string]Checked{} // in memory: a restart forgets them until characters are loaded again
)

// CheckedSlots lists what's been confirmed so far.
func CheckedSlots() []Checked {
	checkedMu.Lock()
	defer checkedMu.Unlock()
	out := make([]Checked, 0, len(checked))
	for _, c := range checked {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InvType < out[j].InvType })
	return out
}

// slotColumn is the column for an inventory type: confirmed by a character's gear, or the guess.
func slotColumn(invType string) (int, bool) {
	checkedMu.Lock()
	c, ok := checked[invType]
	checkedMu.Unlock()
	if ok && c.Status != "mismatch" {
		return c.Column, true
	}
	col, _ := guessColumn(invType)
	return col, harvested[invType]
}

// Check compares an equipped suffix item's real stats (from the armory) with the calculation at
// every column. A match on the guess confirms it; a match on exactly one other column corrects
// it; anything else is a mismatch and the slot stays approximate.
func Check(invType, variant string, ilvl, quality int, base, actual map[string]float64, by string) (Checked, bool) {
	guess, ok := guessColumn(invType)
	if !ok {
		return Checked{}, false
	}
	matches := func(col int) bool {
		suf, _ := suffixAt(variant, ilvl, quality, col)
		if len(suf) == 0 {
			return false
		}
		for _, name := range bonusStat {
			if base[name]+suf[name] != actual[name] {
				return false
			}
		}
		return true
	}
	if s, _ := suffixAt(variant, ilvl, quality, guess); len(s) == 0 {
		return Checked{}, false // no scorable suffix stats on it: proves nothing
	}
	c := Checked{InvType: invType, Column: guess, By: by, Status: "checked"}
	if !matches(guess) {
		var hit []int
		for col := 0; col < 5; col++ {
			if col != guess && matches(col) {
				hit = append(hit, col)
			}
		}
		if len(hit) == 1 {
			c.Column, c.Status = hit[0], "corrected"
		} else {
			c.Status, c.Detail = "mismatch", fmt.Sprintf("armory %v, no single column fits", actual)
		}
	}
	checkedMu.Lock()
	defer checkedMu.Unlock()
	if old, ok := checked[invType]; ok && old.Status != "mismatch" && c.Status == "mismatch" {
		return old, true // one odd item doesn't undo a confirmed column
	}
	checked[invType] = c
	return c, true
}

// bonusStat names ItemBonus stat ids, derived from the suffix names in the same client data
// ("of Agility" = 3, "of Strength" = 4, "of Intellect" = 5, "of Spirit" = 6, "of Stamina" = 7,
// "of Power" = 38).
var bonusStat = map[int]string{3: "AGILITY", 4: "STRENGTH", 5: "INTELLECT", 6: "SPIRIT", 7: "STAMINA", 38: "ATTACK_POWER"}

// SuffixStats computes a variant's suffix stats. verified is false when the slot column is still
// a guess; unknown counts bonus stats the score can't use (spell power, healing, resistances…).
func SuffixStats(variant string, ilvl, quality int, invType string) (stats map[string]float64, verified bool, unknown int) {
	col, verified := slotColumn(invType)
	stats, unknown = suffixAt(variant, ilvl, quality, col)
	if stats == nil {
		return nil, false, 0
	}
	return stats, verified, unknown
}

// statAt is one stat's value: its share of the item's budget for its level, quality and slot
// column, rounded to the nearest point (as in game).
func statAt(share, ilvl, quality, col int) (float64, bool) {
	q := quality - 2 // Good = 2, Superior = 3, Epic = 4
	budget, ok := randProp[ilvl]
	if q < 0 || q > 2 || !ok || col < 0 || col > 4 {
		return 0, false
	}
	return math.Floor(float64(budget[q][col])*float64(share)/10000 + 0.5), true
}

func suffixAt(variant string, ilvl, quality, col int) (map[string]float64, int) {
	if _, ok := statAt(0, ilvl, quality, col); !ok {
		return nil, 0
	}
	stats, unknown := map[string]float64{}, 0
	for _, part := range strings.Split(variant, ",") {
		ids, ok := strings.CutPrefix(part, "b")
		if !ok {
			continue
		}
		for _, s := range strings.Split(ids, ":") {
			id, _ := strconv.ParseInt(s, 10, 64)
			for _, a := range bonusAllocs[id] {
				name, ok := bonusStat[a.stat]
				if !ok {
					unknown++
					continue
				}
				v, _ := statAt(a.share, ilvl, quality, col)
				stats[name] += v
			}
		}
	}
	return stats, unknown
}

// SuffixLine is one suffix stat for a tooltip: exact when the slot's column is checked on real
// gear, otherwise the range over every column the slot could use.
type SuffixLine struct {
	Stat  string  `json:"stat"`
	Value float64 `json:"value"` // at the checked or guessed column
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

// SuffixLines describes a variant's suffix stats for display; other counts bonus stats the game
// data has that aren't named here (spell power, healing, resistances…).
func SuffixLines(variant string, ilvl, quality int, invType string) (lines []SuffixLine, exact bool, other int) {
	col, exact := slotColumn(invType)
	at, other := suffixAt(variant, ilvl, quality, col)
	if at == nil {
		return nil, false, 0
	}
	for _, name := range []string{"STRENGTH", "AGILITY", "STAMINA", "INTELLECT", "SPIRIT", "ATTACK_POWER"} {
		v, ok := at[name]
		if !ok {
			continue
		}
		l := SuffixLine{Stat: name, Value: v, Min: v, Max: v}
		if !exact {
			for c := 0; c < 5; c++ {
				x, _ := suffixAt(variant, ilvl, quality, c)
				l.Min, l.Max = math.Min(l.Min, x[name]), math.Max(l.Max, x[name])
			}
		}
		lines = append(lines, l)
	}
	return lines, exact, other
}

// Item is one piece of gear: equipped, or a candidate listed now.
type Item struct {
	ID       int64              `json:"id"`
	Variant  string             `json:"variant,omitempty"`
	Name     string             `json:"name"`
	Quality  int                `json:"quality"`
	Icon     string             `json:"icon,omitempty"`
	InvType  string             `json:"inv_type"`
	ReqLevel int                `json:"req_level"`
	Stats    map[string]float64 `json:"stats"`
	DPS      float64            `json:"dps,omitempty"`
	Flags    []string           `json:"flags,omitempty"`
	Approx   bool               `json:"approx,omitempty"` // suffix stats from an unchecked slot column
	Price    int64              `json:"price,omitempty"`
	House    string             `json:"house,omitempty"`
}

// Score is the weighted value of an item; Stamina, Intellect and Spirit count only when weighted.
func Score(it Item, w map[string]float64) float64 {
	s := it.DPS * w["DPS"]
	for k, v := range it.Stats {
		s += v * w[k]
	}
	return math.Round(s*10) / 10
}

// Row is one upgrade.
type Row struct {
	Slot     string  `json:"slot"`
	Item     Item    `json:"item"`
	Replaces []Item  `json:"replaces"`
	Gain     float64 `json:"gain"`
	PerGold  float64 `json:"per_gold"`
	Health   float64 `json:"health"`
	Mana     float64 `json:"mana"`
	Spirit   float64 `json:"spirit"`
	Ahead    bool    `json:"ahead"` // can't be worn yet (next levels)
}

// Options are the page's filters.
type Options struct {
	Level       int
	Ahead       int // extra levels to include
	MinGain     float64
	MaxPrice    int64 // copper, 0 = no cap
	BuyOnce     bool  // biggest gain first, and skip items far below the character's level
	MaxBelow    int   // with BuyOnce: skip items requiring more than this many levels below
	HealthPerSt float64
}

// Find compares candidates with the equipped gear and returns upgrades, best gain per gold first.
func Find(equipped map[string][]Item, cands []Item, w map[string]float64, o Options) []Row {
	var out []Row
	for _, c := range cands {
		if c.ReqLevel > o.Level+o.Ahead || c.Price <= 0 || (o.MaxPrice > 0 && c.Price > o.MaxPrice) {
			continue
		}
		if o.BuyOnce && o.MaxBelow > 0 && c.ReqLevel < o.Level-o.MaxBelow {
			continue // soon outlevelled: buy once, not every few levels
		}
		slot := Slot(c.InvType)
		if slot == "" || (slot == "OFF_HAND" && twoHanded(equipped)) {
			continue // wielding a two-hander: no one-hander + off-hand pairing yet
		}
		replaces := current(equipped, slot, c.InvType, w)
		var old float64
		var oldStats = map[string]float64{}
		for _, r := range replaces {
			old += Score(r, w)
			for k, v := range r.Stats {
				oldStats[k] += v
			}
		}
		gain := Score(c, w) - old
		if gain < o.MinGain {
			continue
		}
		out = append(out, Row{Slot: slot, Item: c, Replaces: replaces, Gain: math.Round(gain*10) / 10,
			PerGold: math.Round(gain/(float64(c.Price)/10000)*10) / 10,
			Health:  (c.Stats["STAMINA"] - oldStats["STAMINA"]) * o.HealthPerSt,
			Mana:    (c.Stats["INTELLECT"] - oldStats["INTELLECT"]) * 15,
			Spirit:  c.Stats["SPIRIT"] - oldStats["SPIRIT"],
			Ahead:   c.ReqLevel > o.Level})
	}
	sort.Slice(out, func(i, j int) bool {
		if o.BuyOnce && out[i].Gain != out[j].Gain {
			return out[i].Gain > out[j].Gain
		}
		if o.BuyOnce {
			return out[i].Item.Price < out[j].Item.Price
		}
		return out[i].PerGold > out[j].PerGold
	})
	return out
}

func twoHanded(eq map[string][]Item) bool {
	for _, it := range eq["MAIN_HAND"] {
		if it.InvType == "TWOHWEAPON" {
			return true
		}
	}
	return false
}

// current is what a candidate would replace: the weaker ring or trinket, main hand plus off
// hand for a two-hander.
func current(eq map[string][]Item, slot, invType string, w map[string]float64) []Item {
	have := eq[slot]
	switch {
	case slot == "FINGER" || slot == "TRINKET":
		if len(have) < 2 {
			return nil // an empty ring or trinket slot
		}
		if Score(have[0], w) <= Score(have[1], w) {
			return have[:1]
		}
		return have[1:2]
	case invType == "TWOHWEAPON":
		return append(append([]Item{}, have...), eq["OFF_HAND"]...)
	}
	return have
}

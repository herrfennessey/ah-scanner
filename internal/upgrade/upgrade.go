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
	Icon    string             `json:"icon"`    // the spec's spell icon (client SpellMisc.SpellIconFileDataID)
	Weights map[string]float64 `json:"weights"` // stat type (STRENGTH, …) or "DPS"
	Sources map[string]string  `json:"sources"`
	// Ignore lists stats the spec doesn't use at all: not scored and not flagged as "not
	// scored" either (a healer doesn't care that a robe also has spell damage).
	Ignore []string `json:"ignore,omitempty"`
}

// Presets: Feral (cat) and Restoration, each weight with its source. A new spec is a preset here
// plus its model (see internal/web: the page lists the specs from /api/upgrades/specs). Icons are
// the client's SpellIconFileDataID on Blizzard's icon server: Cat Form's matches its spell media.
var Presets = map[string]Preset{
	// Feral (cat): attack power is the unit. Strength, Agility, crit, hit and weapon DPS are worked
	// out per character by the cat DPS model in feral.go (internal/web feralModel). Stamina is the
	// player's survivability choice for solo levelling, which the DPS model doesn't cover.
	"feral": {
		Name:    "Feral (cat)",
		Icon:    "https://render.worldofwarcraft.com/forever-eu/icons/56/132115.jpg", // Cat Form (768)
		Weights: map[string]float64{"ATTACK_POWER": 1, "STAMINA": 1, "INTELLECT": 0, "SPIRIT": 0},
		Sources: map[string]string{
			"ATTACK_POWER": "the unit: +1 attack power",
			"STAMINA":      "your survivability choice for solo levelling (the DPS model doesn't value it)",
			"INTELLECT":    "no damage: mana for shifting out and healing",
			"SPIRIT":       "no damage: out-of-combat regeneration",
		},
		Ignore: []string{"SPELL_POWER", "SPELL_HEALING_DONE", "SPELL_DAMAGE_DONE", "MANA_REGENERATION",
			"BONUS_STAT_FIRE_DAMAGE_DONE", "BONUS_STAT_FROST_DAMAGE_DONE", "BONUS_STAT_SHADOW_DAMAGE_DONE",
			"BONUS_STAT_NATURE_DAMAGE_DONE"},
	},
	// Restoration: healing power is the unit. Spirit, MP5, Intellect and crit are worked out per
	// character by the model in resto.go (internal/web restoModel); Stamina is ignored, as there.
	// Forever items split healing power into Spell Healing and Spell Power (which covers healing
	// too), so both count as healing power.
	"resto": {
		Name:    "Restoration",
		Icon:    "https://render.worldofwarcraft.com/forever-eu/icons/56/136041.jpg", // Healing Touch (5185)
		Weights: map[string]float64{"SPELL_HEALING_DONE": 1, "SPELL_POWER": 1},
		Sources: map[string]string{
			"SPELL_HEALING_DONE": "the unit: +1 healing power",
			"SPELL_POWER":        "spell power covers healing: counts as healing power",
		},
		Ignore: []string{"STRENGTH", "AGILITY", "ATTACK_POWER", "HIT_RATING", "SPELL_DAMAGE_DONE", "STAMINA",
			"BONUS_STAT_FIRE_DAMAGE_DONE", "BONUS_STAT_FROST_DAMAGE_DONE", "BONUS_STAT_SHADOW_DAMAGE_DONE",
			"BONUS_STAT_NATURE_DAMAGE_DONE", "HEALTH_REGEN"},
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

// ProficiencyLevel is the level a druid learns the weapon skill at (Polearms: 20, from the same
// SkillRaceClassInfo rows); 0 when it is known from the start.
func ProficiencyLevel(class, subclass int) int {
	if class == 2 && subclass == 6 {
		return 20
	}
	return 0
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
	if ok {
		// Real gear contradicted the calculation (and no item ever confirmed it): keep scoring at
		// the column but show it as uncertain, even in a harvested slot.
		return c.Column, false
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
	// Test against the column in use (an earlier correction, else the guess), so a corrected slot
	// is confirmed by the next item rather than corrected again.
	cur, _ := slotColumn(invType)
	if s, _ := suffixAt(variant, ilvl, quality, cur); len(s) == 0 {
		return Checked{}, false // no scorable suffix stats on it: proves nothing
	}
	c := Checked{InvType: invType, Column: cur, By: by, Status: "checked"}
	if cur != guess {
		c.Status = "corrected"
	}
	if !matches(cur) {
		var hit []int
		for col := 0; col < 5; col++ {
			if col != cur && matches(col) {
				hit = append(hit, col)
			}
		}
		switch {
		case len(hit) == 1 && harvested[invType]:
			// The harvested column reproduced every fixed-stat item of the slot; one item
			// doesn't overturn it.
			c.Status, c.Detail = "mismatch", fmt.Sprintf("armory %v fits column %d, not the harvested %d", actual, hit[0], cur)
		case len(hit) == 1:
			c.Column, c.Status = hit[0], "corrected"
		default:
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
var bonusStat = map[int]string{3: "AGILITY", 4: "STRENGTH", 5: "INTELLECT", 6: "SPIRIT", 7: "STAMINA", 38: "ATTACK_POWER",
	// The rest are matched like the slot columns: for each id, one Blizzard stat type equals the
	// computed value on every fixed-stat item carrying it (2026-11-18): 41 Spell Healing 36/36,
	// 42 Spell Damage 36/36, 45 Spell Power 188/188, 32 crit 23/23, 31 hit 9/9, 43 MP5 1/1 …
	// Ids with no samples (46 "of Regeneration", 84 holy, 89 arcane) stay unnamed.
	12: "DEFENSE_SKILL_RATING", 13: "DODGE_RATING", 31: "HIT_RATING", 32: "CRIT_RATING", 39: "RANGED_ATTACK_POWER",
	41: "SPELL_HEALING_DONE", 42: "SPELL_DAMAGE_DONE", 43: "MANA_REGENERATION", 45: "SPELL_POWER", 50: "EXTRA_ARMOR",
	51: "FIRE_RESISTANCE", 52: "FROST_RESISTANCE", 54: "SHADOW_RESISTANCE", 55: "NATURE_RESISTANCE", 56: "ARCANE_RESISTANCE",
	85: "BONUS_STAT_FIRE_DAMAGE_DONE", 86: "BONUS_STAT_NATURE_DAMAGE_DONE", 87: "BONUS_STAT_FROST_DAMAGE_DONE",
	88: "BONUS_STAT_SHADOW_DAMAGE_DONE"}

// statOrder is the display order of suffix stats in tooltips.
var statOrder = []string{"STRENGTH", "AGILITY", "STAMINA", "INTELLECT", "SPIRIT", "ATTACK_POWER", "RANGED_ATTACK_POWER",
	"CRIT_RATING", "HIT_RATING", "SPELL_HEALING_DONE", "SPELL_DAMAGE_DONE", "SPELL_POWER", "MANA_REGENERATION",
	"DEFENSE_SKILL_RATING", "DODGE_RATING", "EXTRA_ARMOR", "FIRE_RESISTANCE", "FROST_RESISTANCE", "SHADOW_RESISTANCE",
	"NATURE_RESISTANCE", "ARCANE_RESISTANCE", "BONUS_STAT_FIRE_DAMAGE_DONE", "BONUS_STAT_NATURE_DAMAGE_DONE",
	"BONUS_STAT_FROST_DAMAGE_DONE", "BONUS_STAT_SHADOW_DAMAGE_DONE"}

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
	for _, name := range statOrder {
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
	// Unscored lists stats the item has that the weights don't value (and the spec doesn't
	// ignore), e.g. a resistance: shown so the gain isn't the whole story.
	Unscored []string `json:"unscored,omitempty"`
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
	Ignore      []string // the preset's ignored stats (see Preset.Ignore)
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
			Mana:    (c.Stats["INTELLECT"] - oldStats["INTELLECT"]) * 15, // 15 mana per Intellect (guide 1)
			Spirit:  c.Stats["SPIRIT"] - oldStats["SPIRIT"],
			Ahead:   c.ReqLevel > o.Level, Unscored: unscored(c, w, o.Ignore)})
	}
	// Candidates arrive in map order: break ties fully so the list (and each slot's best) is stable.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if o.BuyOnce && a.Gain != b.Gain {
			return a.Gain > b.Gain
		}
		if !o.BuyOnce && a.PerGold != b.PerGold {
			return a.PerGold > b.PerGold
		}
		if a.Item.Price != b.Item.Price {
			return a.Item.Price < b.Item.Price
		}
		if a.Item.ID != b.Item.ID {
			return a.Item.ID < b.Item.ID
		}
		return a.Item.Variant < b.Item.Variant
	})
	return out
}

// unscored names the stats of an item the weights give nothing for, minus the utility stats
// (shown as columns) and the ones the spec ignores; known stats in display order, then the rest.
func unscored(it Item, w map[string]float64, ignore []string) []string {
	skip := map[string]bool{"STAMINA": true, "INTELLECT": true, "SPIRIT": true}
	for _, k := range ignore {
		skip[k] = true
	}
	listed := map[string]bool{}
	var out, extra []string
	for _, k := range statOrder {
		listed[k] = true
		if it.Stats[k] > 0 && w[k] == 0 && !skip[k] {
			out = append(out, k)
		}
	}
	for k, v := range it.Stats { // typed stats no suffix carries, e.g. HEALTH_REGEN
		if v > 0 && w[k] == 0 && !skip[k] && !listed[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
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

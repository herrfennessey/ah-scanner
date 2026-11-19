package web

// The Upgrades page: a character's armory gear against what is listed now. Scoring is in
// internal/upgrade.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/herrfennessey/ah-scanner/internal/suffix"
	"github.com/herrfennessey/ah-scanner/internal/upgrade"
)

type character struct {
	Name     string                    `json:"name"`
	Level    int                       `json:"level"`
	Class    string                    `json:"class"`
	Race     string                    `json:"race"`
	ILvl     int                       `json:"equipped_item_level"`
	Surname  string                    `json:"surname"`
	Faction  string                    `json:"faction"`
	Render   string                    `json:"render"`
	Equipped map[string][]upgrade.Item `json:"equipped"`
	Doll     map[string]*dollItem      `json:"doll"` // by armory slot (HEAD, FINGER_1, …)
	Stats    map[string]float64        `json:"stats"`
	Specs    map[string]specGroup      `json:"specs"` // "Restoration", "Feral Combat", … (dual spec)
	Loaded   int64                     `json:"loaded"`
}

// specGroup is one talent group from the armory: both dual-spec groups come back whichever is
// active, so Resto talents are known while logged out in Feral.
type specGroup struct {
	Active  bool                   `json:"active"`
	Talents map[upgrade.Talent]int `json:"talents"` // the talents the Resto model uses
}

// talentByName maps the armory's talent names to the Resto model's talents.
var talentByName = map[string]upgrade.Talent{"Heart of the Wild": upgrade.HotW, "Naturalist": upgrade.Naturalist,
	"Bloom": upgrade.Bloom, "Gift of Nature": upgrade.GoN, "Swiftmend": upgrade.Swiftmend,
	"Improved Rejuvenation": upgrade.ImpRejuv, "Living Spirit": upgrade.LivingSpirit, "Nature's Swiftness": upgrade.NS,
	"Reflection": upgrade.Reflection, "Improved Regrowth": upgrade.ImpRegrowth, "Wild Growth": upgrade.WildGrowth,
	"Tranquil Spirit": upgrade.Tranq,
	"Furor":           upgrade.Furor, "Feral Swiftness": upgrade.FeralSwiftness, "Feral Instinct": upgrade.FeralInstinct,
	"Feral Charge": upgrade.FeralCharge, "Sharpened Claws": upgrade.SharpenedClaws, "Shredding Attacks": upgrade.ShreddingAttacks,
	"Predatory Discipline": upgrade.PredatoryDiscipline, "Predatory Strikes": upgrade.PredatoryStrikes,
	"Blood Frenzy": upgrade.BloodFrenzy, "Leader of the Pack": upgrade.LeaderOfThePack, "Natural Instinct": upgrade.NaturalInstinct,
	"Rend and Tear": upgrade.RendAndTear, "Shifting Power": upgrade.ShiftingPower, "Savage Fury": upgrade.SavageFury,
	"Ferocity": upgrade.Ferocity}

// dollItem is an equipped item as the paper doll shows it.
type dollItem struct {
	upgrade.Item
	ILvl    int    `json:"ilvl"`
	Enchant string `json:"enchant,omitempty"`
}

var charCache sync.Map // slug → *character, 10 minutes

type apiStat struct {
	Type struct {
		Type string `json:"type"`
	} `json:"type"`
	Value float64 `json:"value"`
}

type apiSpell struct {
	Description string `json:"description"`
}

func spellFlags(sp []apiSpell) []string {
	var f []string
	for _, s := range sp {
		switch {
		case strings.Contains(s.Description, "Chance on hit"):
			f = append(f, "proc")
		case strings.HasPrefix(s.Description, "Use:"):
			f = append(f, "use")
		case strings.HasPrefix(s.Description, "Equip:"):
			f = append(f, "equip")
		}
	}
	return f
}

// statsOf sums typed stats; the ones the weights don't value are named per row (upgrade.Row.Unscored).
func statsOf(in []apiStat) map[string]float64 {
	out := map[string]float64{}
	for _, s := range in {
		if s.Type.Type != "" {
			out[s.Type.Type] += s.Value
		}
	}
	return out
}

func (s *Server) loadCharacter(ctx context.Context, slug string, fresh bool) (*character, error) {
	if c, ok := charCache.Load(slug); ok && !fresh && time.Since(time.Unix(c.(*character).Loaded, 0)) < 10*time.Minute {
		return c.(*character), nil
	}
	if s.API == nil {
		return nil, errors.New("no Blizzard client configured")
	}
	ns := url.Values{"namespace": {"profile-forever-" + s.Cfg.Region}}
	var prof struct {
		Name  string `json:"name"`
		Level int    `json:"level"`
		Class struct {
			Name string `json:"name"`
		} `json:"character_class"`
		Race struct {
			Name string `json:"name"`
		} `json:"race"`
		ILvl    int `json:"equipped_item_level"`
		Faction struct {
			Type string `json:"type"`
		} `json:"faction"`
	}
	if err := s.API.GetJSON(ctx, "/profile/wow/character/"+url.PathEscape(slug), ns, &prof); err != nil {
		return nil, err
	}
	var eq struct {
		Items []struct {
			Slot struct {
				Type string `json:"type"`
			} `json:"slot"`
			Item struct {
				ID int64 `json:"id"`
			} `json:"item"`
			Name    string  `json:"name"`
			Bonus   []int64 `json:"bonus_list"`
			Quality struct {
				Type string `json:"type"`
			} `json:"quality"`
			InvType struct {
				Type string `json:"type"`
			} `json:"inventory_type"`
			Stats  []apiStat  `json:"stats"`
			Spells []apiSpell `json:"spells"`
			Weapon struct {
				DPS struct {
					Value float64 `json:"value"`
				} `json:"dps"`
			} `json:"weapon"`
			Level struct {
				Value int `json:"value"`
			} `json:"level"`
			Enchants []struct {
				Display string `json:"display_string"`
			} `json:"enchantments"`
		} `json:"equipped_items"`
	}
	if err := s.API.GetJSON(ctx, "/profile/wow/character/"+url.PathEscape(slug)+"/equipment", ns, &eq); err != nil {
		return nil, err
	}
	c := &character{Name: prof.Name, Level: prof.Level, Class: prof.Class.Name, Race: prof.Race.Name, ILvl: prof.ILvl,
		Faction: strings.ToLower(prof.Faction.Type), Equipped: map[string][]upgrade.Item{}, Doll: map[string]*dollItem{},
		Stats: map[string]float64{}, Loaded: time.Now().Unix()}
	var media struct {
		Character struct {
			Surname string `json:"surname"`
		} `json:"character"`
		Assets []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"assets"`
	}
	if s.API.GetJSON(ctx, "/profile/wow/character/"+url.PathEscape(slug)+"/character-media", ns, &media) == nil {
		c.Surname = media.Character.Surname
		for _, a := range media.Assets {
			if a.Key == "main-raw" {
				c.Render = a.Value
			}
		}
	}
	var stats map[string]json.RawMessage
	statsOK := s.API.GetJSON(ctx, "/profile/wow/character/"+url.PathEscape(slug)+"/statistics", ns, &stats) == nil
	if statsOK {
		for k, raw := range stats {
			var n float64
			var obj struct {
				Effective *float64 `json:"effective"`
				Value     *float64 `json:"value"`
			}
			switch {
			case json.Unmarshal(raw, &n) == nil:
				c.Stats[k] = n
			case json.Unmarshal(raw, &obj) == nil && obj.Effective != nil:
				c.Stats[k] = *obj.Effective
			case obj.Value != nil:
				c.Stats[k] = *obj.Value
			}
		}
	}
	var specs specsResponse
	c.Specs = map[string]specGroup{}
	if s.API.GetJSON(ctx, "/profile/wow/character/"+url.PathEscape(slug)+"/specializations", ns, &specs) == nil {
		c.Specs = specGroups(specs)
	}
	for _, e := range eq.Items {
		slot := e.Slot.Type
		switch {
		case strings.HasPrefix(slot, "FINGER"):
			slot = "FINGER"
		case strings.HasPrefix(slot, "TRINKET"):
			slot = "TRINKET"
		case slot == "HANDS":
			slot = "HAND"
		}
		st := statsOf(e.Stats)
		var variant string
		if len(e.Bonus) > 0 {
			parts := make([]string, len(e.Bonus))
			for i, b := range e.Bonus {
				parts[i] = strconv.FormatInt(b, 10)
			}
			variant = "b" + strings.Join(parts, ":")
		}
		it := upgrade.Item{ID: e.Item.ID, Variant: variant, Name: e.Name,
			Quality: qualities[e.Quality.Type], InvType: e.InvType.Type, Stats: st, DPS: e.Weapon.DPS.Value,
			Flags: spellFlags(e.Spells)}
		s.Store.DB.QueryRowContext(ctx, `SELECT COALESCE(icon,'') FROM item WHERE id=?`, it.ID).Scan(&it.Icon)
		if it.Icon == "" {
			it.Icon = s.itemIcon(ctx, it.ID)
		}
		// The armory's stats include enchants (e.g. "Stamina +3 and Armor +24"), which the
		// calculation can't separate: only unenchanted items test the suffix columns.
		if variant != "" && len(e.Enchants) == 0 {
			s.checkSuffix(ctx, c.Name+" "+c.Surname, e.Name, it, e.Level.Value)
		}
		d := &dollItem{Item: it, ILvl: e.Level.Value}
		for _, en := range e.Enchants {
			d.Enchant = en.Display
		}
		c.Doll[e.Slot.Type] = d
		c.Equipped[slot] = append(c.Equipped[slot], it)
	}
	if statsOK { // a failed /statistics isn't cached: the next request retries it
		charCache.Store(slug, c)
	}
	return c, nil
}

// checkSuffix tests the suffix calculation against an equipped item's real stats: the armory
// gives base + suffix, the item's own tooltip gives the base.
func (s *Server) checkSuffix(ctx context.Context, who, name string, it upgrade.Item, ilvl int) {
	var raw sql.NullString
	s.Store.DB.QueryRowContext(ctx, `SELECT tooltip FROM item WHERE id=?`, it.ID).Scan(&raw)
	tip := []byte(raw.String)
	if !raw.Valid || raw.String == "null" {
		fetched, err := s.API.Item(ctx, it.ID, s.Cfg.StaticNamespaces, true)
		if err != nil || len(fetched.Tooltip) == 0 {
			return
		}
		tip = fetched.Tooltip
	}
	var t struct {
		Stats []apiStat `json:"stats"`
	}
	if json.Unmarshal(tip, &t) != nil {
		return
	}
	base := statsOf(t.Stats)
	if c, ok := upgrade.Check(it.InvType, it.Variant, ilvl, it.Quality, base, it.Stats, strings.TrimSpace(who)+": "+name); ok && c.Status != "checked" {
		s.Log.Warn("suffix column", "inv_type", c.InvType, "status", c.Status, "column", c.Column, "by", c.By, "detail", c.Detail)
	}
}

// itemIcon asks the item media endpoint for an icon the collector hasn't stored.
func (s *Server) itemIcon(ctx context.Context, id int64) string {
	var m struct {
		Assets []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"assets"`
	}
	for _, ns := range s.Cfg.StaticNamespaces {
		if s.API.GetJSON(ctx, "/data/wow/media/item/"+strconv.FormatInt(id, 10), url.Values{"namespace": {ns}}, &m) == nil {
			for _, a := range m.Assets {
				if a.Key == "icon" {
					return a.Value
				}
			}
		}
	}
	return ""
}

var qualities = map[string]int{"POOR": 0, "COMMON": 1, "UNCOMMON": 2, "RARE": 3, "EPIC": 4, "LEGENDARY": 5}

// candidates: every equippable (item, variant) listed now on the tradeable houses, cheapest house.
func (s *Server) candidates(ctx context.Context) ([]upgrade.Item, error) {
	hs, err := s.houseList(ctx)
	if err != nil {
		return nil, err
	}
	best := map[[2]string]upgrade.Item{}
	ids := map[int64]bool{}
	for _, h := range hs {
		if h.ViewOnly {
			continue
		}
		rows, err := s.buildRows(ctx, h.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if (r.Class != "Armor" && r.Class != "Weapon") || r.Min <= 0 {
				continue
			}
			k := [2]string{strconv.FormatInt(r.ID, 10), r.Variant}
			if b, ok := best[k]; ok && b.Price <= r.Min {
				continue
			}
			best[k] = upgrade.Item{ID: r.ID, Variant: r.Variant, Name: r.Name, Quality: r.Quality, Icon: r.Icon,
				ReqLevel: r.ReqLevel, Price: r.Min, House: h.Faction}
			ids[r.ID] = true
		}
	}
	type tip struct {
		ilvl    int
		invType string
		class   int
		sub     int
		stats   map[string]float64
		flags   []string
		dps     float64
	}
	tips := map[int64]*tip{}
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT id, COALESCE(item_level,0), tooltip FROM item WHERE class IN ('Armor','Weapon') AND tooltip IS NOT NULL AND tooltip != 'null'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var ilvl int
		var raw string
		if rows.Scan(&id, &ilvl, &raw) != nil || !ids[id] {
			continue
		}
		var t struct {
			InvType struct {
				Type string `json:"type"`
			} `json:"inventory_type"`
			Class struct {
				ID int `json:"id"`
			} `json:"item_class"`
			Sub struct {
				ID int `json:"id"`
			} `json:"item_subclass"`
			Stats  []apiStat  `json:"stats"`
			Spells []apiSpell `json:"spells"`
			Weapon struct {
				DPS struct {
					Value float64 `json:"value"`
				} `json:"dps"`
			} `json:"weapon"`
		}
		if json.Unmarshal([]byte(raw), &t) != nil {
			continue
		}
		tips[id] = &tip{ilvl, t.InvType.Type, t.Class.ID, t.Sub.ID, statsOf(t.Stats), spellFlags(t.Spells), t.Weapon.DPS.Value}
	}
	rows.Close()
	var out []upgrade.Item
	for _, it := range best {
		t := tips[it.ID]
		if t == nil || !upgrade.Usable(t.class, t.sub) {
			continue
		}
		it.InvType, it.DPS = t.invType, t.dps
		if lvl := upgrade.ProficiencyLevel(t.class, t.sub); lvl > it.ReqLevel {
			it.ReqLevel = lvl // e.g. polearms: not wieldable before the skill is learned
		}
		it.Stats = map[string]float64{}
		for k, v := range t.stats {
			it.Stats[k] = v
		}
		it.Flags = append([]string{}, t.flags...)
		if it.Variant != "" {
			st, verified, unknown := upgrade.SuffixStats(it.Variant, t.ilvl, it.Quality, t.invType)
			if st == nil && suffix.Name(it.Variant) != "" {
				it.Flags = append(it.Flags, "suffix not computed")
			}
			for k, v := range st {
				it.Stats[k] += v
			}
			it.Approx = len(st) > 0 && !verified // only when there are suffix stats to be off
			if unknown > 0 {
				it.Flags = append(it.Flags, "suffix stat not scored")
			}
		}
		out = append(out, it)
	}
	return out, nil
}

func (s *Server) apiUpgrades(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// "Sine Wave", "sine wave" or "sine-wave": the armory's slug is first-last, lower case.
	slug := strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(q.Get("character"), "-", " "))), "-")
	if slug == "" {
		fail(w, 400, errors.New("character is required"))
		return
	}
	preset, ok := upgrade.Presets[q.Get("preset")]
	if !ok {
		preset = upgrade.Presets["feral"]
	}
	// w_STAMINA=1.5 and so on: the page's edited weights override the preset's.
	c, err := s.loadCharacter(r.Context(), slug, q.Get("fresh") == "1")
	if err != nil {
		fail(w, 502, err)
		return
	}
	weights := map[string]float64{}
	for k, v := range preset.Weights {
		weights[k] = v
	}
	// Restoration: Spirit, MP5, Intellect and crit come from the model on this character's sheet
	// (upgrade.ComputeResto) for the chosen content.
	var model any
	switch q.Get("preset") {
	case "resto":
		m, err := restoModel(c, q.Get("content"))
		if err != nil {
			fail(w, 422, fmt.Errorf("can't work out Restoration weights for this character: %w", err))
			return
		}
		for k, v := range m.weights {
			weights[k] = v
		}
		model = m
	default:
		m, err := feralModel(c, q.Get("content"))
		if err != nil {
			fail(w, 422, fmt.Errorf("can't work out Feral weights for this character: %w", err))
			return
		}
		for k, v := range m.weights {
			weights[k] = v
		}
		model = m
	}
	base := map[string]float64{} // before the page's own edits: what an edit is compared with
	for k, v := range weights {
		base[k] = v
	}
	for k := range q {
		if stat, ok := strings.CutPrefix(k, "w_"); ok {
			if v, err := strconv.ParseFloat(q.Get(k), 64); err == nil && v >= 0 && v <= 100 {
				weights[stat] = v
			}
		}
	}
	cands, err := s.candidates(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	o := upgrade.Options{Level: c.Level, MinGain: 3, HealthPerSt: 10, Ignore: preset.Ignore}
	if c.Race == "Tauren" {
		o.HealthPerSt = 10.5 // Endurance (guide 1)
	}
	if q.Get("buy_once") == "1" {
		o.BuyOnce, o.MaxBelow = true, 10
	}
	if q.Get("ahead") == "1" {
		o.Ahead = 3
	}
	if v, err := strconv.ParseFloat(q.Get("min_gain"), 64); err == nil {
		o.MinGain = v
	}
	if v, err := strconv.ParseFloat(q.Get("max_price"), 64); err == nil && v > 0 {
		o.MaxPrice = int64(v * 10000)
	}
	writeJSON(w, map[string]any{"character": c, "preset": preset, "rows": upgrade.Find(c.Equipped, cands, weights, o), "weights": weights,
		"candidates": len(cands), "checked": upgrade.CheckedSlots(), "model": model, "base_weights": base})
}

// feralModelOut is the cat DPS model's result for the page: the weights it set and why.
type feralModelOut struct {
	Kind    string                `json:"kind"` // "feral"
	Content string                `json:"content"`
	Profile upgrade.CatProfile    `json:"profile"`
	Sheet   map[string]float64    `json:"sheet"` // Cat Form inputs worked out from the armory
	Talents string                `json:"talents"`
	Result  upgrade.CatWeightsOut `json:"result"`
	weights map[string]float64
}

// feralModel runs the cat model on the character. The armory sheet is caster form, so the Cat
// Form values follow the spec: AP = 2×Str (×Heart of the Wild in cat) + Agi + 2×level − 20 +
// Predatory Strikes (0.5×rank×level) + gear AP; crit = sheet crit + Sharpened Claws (3%/rank) +
// Leader of the Pack (3%, its tooltip); hit from gear rating (10 = 1%); weapon DPS from the main hand.
func feralModel(c *character, content string) (feralModelOut, error) {
	if err := needStats(c, "strength", "agility", "melee_crit"); err != nil {
		return feralModelOut{}, err
	}
	prof, ok := upgrade.CatProfiles[content]
	if !ok {
		content, prof = "dungeon", upgrade.CatProfiles["dungeon"]
	}
	talents, tsrc := upgrade.DefaultFeralTalents(c.Level), "default levelling build"
	if g, ok := c.Specs["Feral Combat"]; ok && len(g.Talents) > 0 {
		talents, tsrc = g.Talents, "Feral talents from the armory"
	}
	L := float64(c.Level)
	var gearAP, hitRating, wdps float64
	for slot, items := range c.Equipped {
		for _, it := range items {
			gearAP += it.Stats["ATTACK_POWER"]
			hitRating += it.Stats["HIT_RATING"]
			if slot == "MAIN_HAND" {
				wdps = math.Max(wdps, it.DPS)
			}
		}
	}
	str := c.Stats["strength"] * (1 + 0.02*float64(talents[upgrade.HotW]))
	ap := 2*str + c.Stats["agility"] + 2*L - 20 + 0.5*float64(talents[upgrade.PredatoryStrikes])*L + gearAP
	crit := c.Stats["melee_crit"] + 3*float64(talents[upgrade.SharpenedClaws]) + 3*float64(talents[upgrade.LeaderOfThePack])
	in := upgrade.CatInputs{Level: c.Level, Delta: prof.Delta, Armor: 60 * float64(c.Level+prof.Delta), AP: ap, CritPct: crit,
		HitPct: hitRating / 10, WeaponDPS: wdps, Behind: prof.Behind, Rake: prof.Rake, RipShare: prof.RipShare, Talents: talents}
	res, err := upgrade.CatWeights(in)
	if err != nil {
		return feralModelOut{}, err
	}
	return feralModelOut{Kind: "feral", Content: content, Profile: prof, Talents: tsrc, Result: res,
		Sheet: map[string]float64{"level": L, "ap": ap, "crit": crit, "hit": in.HitPct, "wdps": wdps, "armor": in.Armor},
		// Crit and hit are per 1%; items carry rating at 14 and 10 per 1% (client combat ratings).
		// No listed item carries expertise or haste, so those weights are shown but not applied.
		weights: map[string]float64{"STRENGTH": res.Str, "AGILITY": res.Agi, "CRIT_RATING": res.Crit / 14,
			"HIT_RATING": res.Hit / 10, "DPS": res.WeaponDPS}}, nil
}

// restoModelOut is the Restoration model's result for the page: the weights it set and why.
type restoModelOut struct {
	Kind    string               `json:"kind"` // "resto"
	Content string               `json:"content"`
	Profile upgrade.Profile      `json:"profile"`
	Sheet   map[string]float64   `json:"sheet"` // the inputs taken from the armory
	Talents string               `json:"talents"`
	Result  upgrade.RestoWeights `json:"result"`
	weights map[string]float64
}

// restoModel runs upgrade.ComputeResto on the character's sheet: level, Intellect, Spirit, spell
// crit and max mana from the armory statistics, +healing and MP5 summed from the equipped gear.
func restoModel(c *character, content string) (restoModelOut, error) {
	if err := needStats(c, "intellect", "spirit", "spell_crit"); err != nil {
		return restoModelOut{}, err
	}
	prof, ok := upgrade.RestoProfiles[content]
	if !ok {
		content, prof = "dungeon", upgrade.RestoProfiles["dungeon"]
	}
	in := upgrade.DefaultRestoInputs(c.Level, prof)
	in.Int, in.Spirit, in.CritPct, in.ManaOverride = c.Stats["intellect"], c.Stats["spirit"], c.Stats["spell_crit"], c.Stats["power"]
	talents := "default levelling build"
	if resto, ok := c.Specs["Restoration"]; ok && len(resto.Talents) > 0 {
		in.Talents, talents = resto.Talents, "Restoration talents from the armory"
		if !resto.Active {
			// The armory's sheet is computed with the active (other) spec's talents: rescale
			// Intellect and Spirit to the Resto group's Heart of the Wild (+2%/rank) and Living
			// Spirit (+5%/rank), and the mana pool by 15 per Intellect point gained.
			var act map[upgrade.Talent]int
			for _, g := range c.Specs {
				if g.Active {
					act = g.Talents
				}
			}
			intel := in.Int / (1 + 0.02*float64(act[upgrade.HotW])) * (1 + 0.02*float64(resto.Talents[upgrade.HotW]))
			in.ManaOverride += 15 * (intel - in.Int)
			in.Int = intel
			in.Spirit = in.Spirit / (1 + 0.05*float64(act[upgrade.LivingSpirit])) * (1 + 0.05*float64(resto.Talents[upgrade.LivingSpirit]))
			talents += " (sheet rescaled from the active spec)"
		}
	}
	for _, items := range c.Equipped {
		for _, it := range items {
			in.HP += it.Stats["SPELL_HEALING_DONE"] + it.Stats["SPELL_POWER"]
			in.MP5 += it.Stats["MANA_REGENERATION"]
		}
	}
	res, err := upgrade.ComputeResto(in)
	if err != nil {
		return restoModelOut{}, err
	}
	return restoModelOut{Kind: "resto", Content: content, Profile: prof, Result: res, Talents: talents,
		Sheet: map[string]float64{"level": float64(in.Level), "int": in.Int, "spirit": in.Spirit, "hp": in.HP,
			"mp5": in.MP5, "crit": in.CritPct, "mana": in.ManaOverride},
		// Crit is per 1%; items carry rating at 14 per 1% (client combat ratings table).
		weights: map[string]float64{"SPELL_HEALING_DONE": res.HP, "SPELL_POWER": res.HP, "SPIRIT": res.Spirit,
			"MANA_REGENERATION": res.MP5, "INTELLECT": res.Int, "CRIT_RATING": res.Crit / 14}}, nil
}

// needStats fails when the armory sheet lacks a stat a model reads: scoring with it as 0 would
// rank the wrong purchases.
func needStats(c *character, keys ...string) error {
	var missing []string
	for _, k := range keys {
		if _, ok := c.Stats[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the armory sheet lacks %s (try Load again)", strings.Join(missing, ", "))
	}
	return nil
}

// points is the ranks spent in a talent group's modelled talents.
func points(t map[upgrade.Talent]int) (n int) {
	for _, r := range t {
		n += r
	}
	return n
}

// apiUpgradeSpecs lists the specs the Upgrades page offers, for its picker.
func (s *Server) apiUpgradeSpecs(w http.ResponseWriter, r *http.Request) {
	type spec struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Icon string `json:"icon"`
	}
	out := []spec{}
	for k, p := range upgrade.Presets {
		out = append(out, spec{k, p.Name, p.Icon})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

// specsResponse is the armory's /specializations: one entry per dual-spec group.
type specsResponse struct {
	Groups []struct {
		Active bool `json:"is_active"`
		Specs  []struct {
			Spec struct {
				Name string `json:"name"`
			} `json:"specialization"`
			Talents []struct {
				Talent struct {
					Name string `json:"name"`
				} `json:"talent"`
				Rank int `json:"talent_rank"`
			} `json:"talents"`
		} `json:"specializations"`
	} `json:"specialization_groups"`
}

// specGroups turns the armory's groups into the talents per spec. A group can list points in
// several trees (a 7/34/10 Feral build has Heart of the Wild in the Restoration tree): its talents
// are all of them, and it is named after the tree with the most points. If both groups are in one
// spec, the one with more points spent wins, the active one on a tie.
func specGroups(r specsResponse) map[string]specGroup {
	out := map[string]specGroup{}
	for _, g := range r.Groups {
		sg := specGroup{Active: g.Active, Talents: map[upgrade.Talent]int{}}
		name, most := "", -1
		for _, sp := range g.Specs {
			spent := 0
			for _, t := range sp.Talents {
				spent += t.Rank
				if k, ok := talentByName[t.Talent.Name]; ok {
					sg.Talents[k] += t.Rank
				}
			}
			if spent > most {
				name, most = sp.Spec.Name, spent
			}
		}
		if name == "" {
			continue
		}
		if old, ok := out[name]; ok {
			if p, q := points(old.Talents), points(sg.Talents); p > q || p == q && old.Active {
				continue
			}
		}
		out[name] = sg
	}
	return out
}

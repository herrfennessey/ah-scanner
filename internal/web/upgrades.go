package web

// The Upgrades page: a character's armory gear against what is listed now. Scoring is in
// internal/upgrade.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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
	Loaded   int64                     `json:"loaded"`
}

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

// typed stats the score or the utility columns use; anything else is flagged.
var known = map[string]bool{"STRENGTH": true, "AGILITY": true, "ATTACK_POWER": true, "CRIT_RATING": true,
	"HIT_RATING": true, "STAMINA": true, "INTELLECT": true, "SPIRIT": true}

func statsOf(in []apiStat) (map[string]float64, []string) {
	out := map[string]float64{}
	var flags []string
	for _, s := range in {
		if s.Type.Type == "" {
			continue
		}
		out[s.Type.Type] += s.Value
		if !known[s.Type.Type] {
			flags = append(flags, strings.ToLower(strings.ReplaceAll(s.Type.Type, "_", " ")))
		}
	}
	return out, flags
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
	if s.API.GetJSON(ctx, "/profile/wow/character/"+url.PathEscape(slug)+"/statistics", ns, &stats) == nil {
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
	icons := map[int64]string{}
	if rows, err := s.Store.DB.QueryContext(ctx, `SELECT id, icon FROM item WHERE icon IS NOT NULL AND icon != ''`); err == nil {
		for rows.Next() {
			var id int64
			var ic string
			if rows.Scan(&id, &ic) == nil {
				icons[id] = ic
			}
		}
		rows.Close()
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
		st, flags := statsOf(e.Stats)
		var variant string
		if len(e.Bonus) > 0 {
			parts := make([]string, len(e.Bonus))
			for i, b := range e.Bonus {
				parts[i] = strconv.FormatInt(b, 10)
			}
			variant = "b" + strings.Join(parts, ":")
		}
		it := upgrade.Item{ID: e.Item.ID, Variant: variant, Name: e.Name, Icon: icons[e.Item.ID],
			Quality: qualities[e.Quality.Type], InvType: e.InvType.Type, Stats: st, DPS: e.Weapon.DPS.Value,
			Flags: append(flags, spellFlags(e.Spells)...)}
		if it.Icon == "" {
			it.Icon = s.itemIcon(ctx, it.ID)
		}
		if variant != "" {
			s.checkSuffix(ctx, c.Name+" "+c.Surname, e.Name, it, e.Level.Value)
		}
		d := &dollItem{Item: it, ILvl: e.Level.Value}
		for _, en := range e.Enchants {
			d.Enchant = en.Display
		}
		c.Doll[e.Slot.Type] = d
		c.Equipped[slot] = append(c.Equipped[slot], it)
	}
	charCache.Store(slug, c)
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
	base, _ := statsOf(t.Stats)
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
		st, flags := statsOf(t.Stats)
		tips[id] = &tip{ilvl, t.InvType.Type, t.Class.ID, t.Sub.ID, st, append(flags, spellFlags(t.Spells)...), t.Weapon.DPS.Value}
	}
	rows.Close()
	var out []upgrade.Item
	for _, it := range best {
		t := tips[it.ID]
		if t == nil || !upgrade.Usable(t.class, t.sub) {
			continue
		}
		it.InvType, it.DPS = t.invType, t.dps
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
			it.Approx = st != nil && !verified
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
	weights := map[string]float64{}
	for k, v := range preset.Weights {
		weights[k] = v
	}
	for k := range q {
		if stat, ok := strings.CutPrefix(k, "w_"); ok {
			if v, err := strconv.ParseFloat(q.Get(k), 64); err == nil && v >= 0 && v <= 100 {
				weights[stat] = v
			}
		}
	}
	c, err := s.loadCharacter(r.Context(), slug, q.Get("fresh") == "1")
	if err != nil {
		fail(w, 502, err)
		return
	}
	cands, err := s.candidates(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	o := upgrade.Options{Level: c.Level, MinGain: 3, HealthPerSt: 10}
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
		"candidates": len(cands), "checked": upgrade.CheckedSlots()})
}

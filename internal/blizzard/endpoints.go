package blizzard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/herrfennessey/ah-scanner/internal/store"
)

// HouseRef is one auction house from the ruleset index.
type HouseRef struct {
	ID      int64
	Name    string
	Faction string // horde | alliance | blackwater | "" if unknown
}

// Index fetches the house list. The trailing slash on the path is required by the API.
func (c *Client) Index(ctx context.Context, rulesetID int, namespace string) ([]HouseRef, json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.GetJSON(ctx, RulesetPath(rulesetID, ""), url.Values{"namespace": {namespace}}, &raw); err != nil {
		return nil, nil, err
	}
	houses, err := ParseIndex(raw)
	return houses, raw, err
}

// ParseIndex tolerantly extracts houses from the index body: any top-level array of objects
// with an id and a name. Faction comes from a faction field if present, else from the name.
func ParseIndex(raw []byte) ([]HouseRef, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // prefer the obvious keys
		return pref(keys[i]) < pref(keys[j]) || (pref(keys[i]) == pref(keys[j]) && keys[i] < keys[j])
	})
	for _, k := range keys {
		var arr []map[string]json.RawMessage
		if json.Unmarshal(top[k], &arr) != nil || len(arr) == 0 {
			continue
		}
		var out []HouseRef
		for _, e := range arr {
			var id int64
			if json.Unmarshal(e["id"], &id) != nil {
				continue
			}
			name := localized(e["name"])
			fac := factionOf(e["faction"])
			if fac == "" {
				fac = factionFromName(name)
			}
			out = append(out, HouseRef{ID: id, Name: name, Faction: fac})
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("index: no array of houses with id/name found (keys: %s)", strings.Join(keys, ", "))
}

func pref(k string) int {
	switch k {
	case "auctions", "houses", "auction_houses":
		return 0
	}
	return 1
}

// localized accepts "name" as a string or as a {locale: string} map.
func localized(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"en_GB", "en_US"} {
			if v := m[k]; v != "" {
				return v
			}
		}
		for _, v := range m {
			return v
		}
	}
	return ""
}

func factionOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var o struct {
			Type string          `json:"type"`
			Name json.RawMessage `json:"name"`
		}
		if json.Unmarshal(raw, &o) == nil {
			s = o.Type
			if s == "" {
				s = localized(o.Name)
			}
		}
	}
	return factionFromName(s)
}

func factionFromName(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "blackwater"), strings.Contains(l, "neutral"), strings.Contains(l, "goblin"):
		return "blackwater"
	case strings.Contains(l, "horde"):
		return "horde"
	case strings.Contains(l, "alliance"):
		return "alliance"
	}
	return ""
}

// AuctionsPath returns the gear (commodities=false) or commodities path for a house.
func AuctionsPath(rulesetID int, houseID int64, commodities bool) string {
	p := RulesetPath(rulesetID, strconv.FormatInt(houseID, 10))
	if commodities {
		p += "/commodities"
	}
	return p
}

var qualities = map[string]int{"POOR": 0, "COMMON": 1, "UNCOMMON": 2, "RARE": 3, "EPIC": 4, "LEGENDARY": 5, "ARTIFACT": 6, "HEIRLOOM": 7}

// Item looks up an item's name, class and tooltip (preview_item), trying each static namespace
// in order. haveIcon skips the media request when the icon is already stored.
func (c *Client) Item(ctx context.Context, id int64, namespaces []string, haveIcon bool) (store.Item, error) {
	var lastErr error
	for _, ns := range namespaces {
		var raw struct {
			Name      json.RawMessage `json:"name"`
			SellPrice int64           `json:"sell_price"`
			Level     int             `json:"level"`
			ReqLevel  int             `json:"required_level"`
			Quality   struct {
				Type string `json:"type"`
			} `json:"quality"`
			ItemClass struct {
				Name json.RawMessage `json:"name"`
			} `json:"item_class"`
			ItemSubclass struct {
				Name json.RawMessage `json:"name"`
			} `json:"item_subclass"`
			PreviewItem json.RawMessage `json:"preview_item"`
		}
		err := c.GetJSON(ctx, "/data/wow/item/"+strconv.FormatInt(id, 10), url.Values{"namespace": {ns}}, &raw)
		if err != nil {
			lastErr = err
			continue
		}
		it := store.Item{ID: id, Name: localized(raw.Name), Class: localized(raw.ItemClass.Name), Subclass: localized(raw.ItemSubclass.Name), SellPrice: raw.SellPrice,
			ItemLevel: raw.Level, ReqLevel: raw.ReqLevel, Tooltip: raw.PreviewItem}
		if q, ok := qualities[raw.Quality.Type]; ok {
			it.Quality = q
		}
		if it.Name != "" {
			if !haveIcon {
				it.Icon = c.itemIcon(ctx, ns, id)
			}
			return it, nil
		}
		lastErr = fmt.Errorf("item %d: no name in %s", id, ns)
	}
	return store.Item{ID: id}, lastErr
}

// itemIcon returns the icon URL from the item media endpoint, or "" (icons are optional).
func (c *Client) itemIcon(ctx context.Context, ns string, id int64) string {
	var raw struct {
		Assets []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"assets"`
	}
	if err := c.GetJSON(ctx, "/data/wow/media/item/"+strconv.FormatInt(id, 10), url.Values{"namespace": {ns}}, &raw); err != nil {
		return ""
	}
	for _, a := range raw.Assets {
		if a.Key == "icon" {
			return a.Value
		}
	}
	return ""
}

// ErrNoReputation means a quest exists but awards no reputation with the faction asked about.
var ErrNoReputation = errors.New("no reputation with that faction")

// Quest returns the reputation a quest awards with one faction, trying each static namespace.
func (c *Client) Quest(ctx context.Context, id int64, namespaces []string, factionID int64) (int, error) {
	var lastErr error
	for _, ns := range namespaces {
		var raw struct {
			Rewards struct {
				Reputations []struct {
					Reward struct {
						ID int64 `json:"id"`
					} `json:"reward"`
					Value int `json:"value"`
				} `json:"reputations"`
			} `json:"rewards"`
		}
		if err := c.GetJSON(ctx, "/data/wow/quest/"+strconv.FormatInt(id, 10), url.Values{"namespace": {ns}}, &raw); err != nil {
			lastErr = err
			continue
		}
		for _, r := range raw.Rewards.Reputations {
			if r.Reward.ID == factionID {
				return r.Value, nil
			}
		}
		return 0, fmt.Errorf("quest %d, faction %d: %w", id, factionID, ErrNoReputation)
	}
	return 0, lastErr
}

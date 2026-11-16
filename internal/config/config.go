// Package config loads config.yaml and the Blizzard credentials from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// House selects one auction house from the index.
type House struct {
	// ID pins the house. The index lists several houses per faction with identical names
	// (one per realm group), so the ID is the only reliable key. It is validated against
	// the index at startup.
	ID      int64  `yaml:"id"`
	Faction string `yaml:"faction"` // horde | alliance | blackwater
	Name    string `yaml:"name"`    // optional exact name, to disambiguate when several houses share a faction
	// Cut is the AH cut on sales (0.05 faction, 0.15 Blackwater). Defaults by faction.
	Cut *float64 `yaml:"cut"`
	// DepositFactor multiplies deposit_rate for this house: Blackwater's deposit is 5× a
	// faction house's. Defaults by faction (5 Blackwater, 1 otherwise).
	DepositFactor *float64 `yaml:"deposit_factor"`
	// ViewOnly marks a house you can't trade on (the other faction on a PvP realm). It is
	// still scanned, because its prices and sales drive demand on the shared Blackwater house.
	ViewOnly bool `yaml:"view_only"`
	// PriceOnly records prices and supply without tracking individual auctions (no sales
	// numbers). Saves disk; implies ViewOnly.
	PriceOnly bool `yaml:"price_only"`
}

// Tradeable reports whether the player can buy and sell on this house.
func (h House) Tradeable() bool { return !h.ViewOnly && !h.PriceOnly }

// AHCut returns the configured or default auction house cut for this house.
func (h House) AHCut() float64 {
	if h.Cut != nil {
		return *h.Cut
	}
	if strings.EqualFold(h.Faction, "blackwater") {
		return 0.15
	}
	return 0.05
}

// DepositMult returns the configured or default deposit multiplier for this house.
func (h House) DepositMult() float64 {
	if h.DepositFactor != nil {
		return *h.DepositFactor
	}
	if strings.EqualFold(h.Faction, "blackwater") {
		return 5
	}
	return 1
}

// Supply tunes the Durotar Supply and Logistics planner.
type Supply struct {
	// CoinPouch overrides the copper a Craftsman's Writ's Coin Pouch is assumed to hold, by tier
	// (journeyman, expert, artisan). The API doesn't say; defaults are guide estimates.
	CoinPouch map[string]int64 `yaml:"coin_pouch"`
}

type Alert struct {
	URL       string `yaml:"url"`        // ntfy topic URL or Home Assistant webhook; POSTed a plain-text message
	AfterFail int    `yaml:"after_fail"` // consecutive failed cycles before alerting (default 3)
}

// Schedule holds standard 5-field cron expressions, evaluated in Europe/Berlin. Collect may
// instead be "follow": poll each house just after it publishes (see Follow).
type Schedule struct {
	Collect string `yaml:"collect"` // snapshot cycle, or "follow"
	Nightly string `yaml:"nightly"` // rollup, retention, backup
}

// Follow tunes the publish-following collect mode.
type Follow struct {
	DefaultOffset time.Duration `yaml:"default_offset"` // publish time past the hour until learned (observed :18:50)
	Lead          time.Duration `yaml:"lead"`           // poll this long after the expected publish
	Retry         time.Duration `yaml:"retry"`          // re-poll interval while a publish is due
	Window        time.Duration `yaml:"window"`         // how long to keep retrying a late publish
	CatchUp       time.Duration `yaml:"catch_up"`       // slow poll interval once a publish is very late
}

// IsFollow reports whether a collect schedule means publish-following mode.
func IsFollow(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "follow") }

type Config struct {
	Region       string        `yaml:"region"`
	RulesetID    int           `yaml:"ruleset_id"`
	Houses       []House       `yaml:"houses"`
	Schedule     Schedule      `yaml:"schedule"`
	Follow       Follow        `yaml:"follow"`
	Jitter       time.Duration `yaml:"jitter"` // random ± offset applied to each collect run
	RawRetention time.Duration `yaml:"raw_retention"`
	DataDir      string        `yaml:"data_dir"`
	Locale       string        `yaml:"locale"`

	DynamicNamespace    string   `yaml:"dynamic_namespace"` // default dynamic-forever-<region>
	StaticNamespaces    []string `yaml:"static_namespaces"` // tried in order for item names
	ItemLookupsPerCycle int      `yaml:"item_lookups_per_cycle"`

	// AmbiguousWeight is the share of ambiguous (cancelled / expired-or-sold) units counted in sold_est.
	AmbiguousWeight float64 `yaml:"ambiguous_weight"`

	// Retention. item_daily is kept forever.
	AuctionRetentionDays int `yaml:"auction_retention_days"` // closed auctions and partial-sale events
	SnapshotFullDays     int `yaml:"snapshot_full_days"`     // item_snapshot at full resolution
	SnapshotThinHours    int `yaml:"snapshot_thin_hours"`    // after that, keep one snapshot per this many hours
	SnapshotMaxDays      int `yaml:"snapshot_max_days"`      // item_snapshot dropped entirely after this
	BackupKeep           int `yaml:"backup_keep"`            // nightly zstd-compressed backups kept

	// StaleAfter flags a feed whose polls succeed but bring no new data (endpoint frozen).
	StaleAfter time.Duration `yaml:"stale_after"`

	// DepositRate estimates a faction house's AH deposit as a share of the item's vendor sell
	// price (unconfirmed for Forever; set it from what the in-game posting window shows).
	// Each house scales it by its deposit_factor.
	DepositRate float64 `yaml:"deposit_rate"`

	Alert Alert `yaml:"alert"`

	Supply Supply `yaml:"supply"`

	Listen string `yaml:"listen"` // address for the web UI and /mcp, e.g. ":4653"; empty = neither

	// MCPToken, if set (env AHC_MCP_TOKEN), is required as "Authorization: Bearer <token>" on /mcp.
	MCPToken string `yaml:"-"`

	// From the environment, never from the file.
	ClientID     string `yaml:"-"`
	ClientSecret string `yaml:"-"`

	// Endpoint overrides, for tests.
	APIBase  string `yaml:"api_base"`
	TokenURL string `yaml:"token_url"`
}

func Default() Config {
	return Config{
		Region:               "eu",
		RulesetID:            1,
		Houses:               []House{{ID: 113, Faction: "horde"}, {ID: 116, Faction: "blackwater"}},
		Schedule:             Schedule{Collect: "*/30 * * * *", Nightly: "0 5 * * *"},
		Jitter:               3 * time.Minute,
		RawRetention:         48 * time.Hour,
		DataDir:              "/data",
		Locale:               "en_GB",
		ItemLookupsPerCycle:  4000,
		AmbiguousWeight:      0.25,
		AuctionRetentionDays: 30,
		SnapshotFullDays:     30,
		SnapshotThinHours:    6,
		SnapshotMaxDays:      180,
		BackupKeep:           3,
		StaleAfter:           3 * time.Hour,
		DepositRate:          0.20,
		Listen:               ":4653",
		Alert:                Alert{AfterFail: 3},
	}
}

// Load reads the YAML file at path (missing file = defaults) and the credentials from the environment.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return c, err
	}
	if c.DynamicNamespace == "" {
		c.DynamicNamespace = "dynamic-forever-" + c.Region
	}
	if len(c.StaticNamespaces) == 0 {
		c.StaticNamespaces = []string{"static-forever-" + c.Region, "static-classic1x-" + c.Region}
	}
	if c.APIBase == "" {
		c.APIBase = "https://" + c.Region + ".api.blizzard.com"
	}
	if c.TokenURL == "" {
		c.TokenURL = "https://oauth.battle.net/token"
	}
	if c.Alert.AfterFail <= 0 {
		c.Alert.AfterFail = 3
	}
	if v := os.Getenv("AHC_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	c.ClientID = os.Getenv("BLIZZARD_CLIENT_ID")
	c.ClientSecret = os.Getenv("BLIZZARD_CLIENT_SECRET")
	c.MCPToken = os.Getenv("AHC_MCP_TOKEN")
	for name, expr := range map[string]string{"collect": c.Schedule.Collect, "nightly": c.Schedule.Nightly} {
		if name == "collect" && IsFollow(expr) {
			continue
		}
		if _, err := ParseCron(expr); err != nil {
			return c, fmt.Errorf("schedule.%s %q: %w", name, expr, err)
		}
	}
	for i, h := range c.Houses {
		f := strings.ToLower(h.Faction)
		if f != "horde" && f != "alliance" && f != "blackwater" {
			return c, fmt.Errorf("houses[%d]: unknown faction %q", i, h.Faction)
		}
		c.Houses[i].Faction = f
	}
	return c, nil
}

// RequireCredentials errors unless the Blizzard client credentials are set.
func (c Config) RequireCredentials() error {
	if c.ClientID == "" || c.ClientSecret == "" {
		return errors.New("BLIZZARD_CLIENT_ID and BLIZZARD_CLIENT_SECRET must be set")
	}
	return nil
}

func (c Config) DBPath() string     { return filepath.Join(c.DataDir, "ahc.db") }
func (c Config) RawDir() string     { return filepath.Join(c.DataDir, "raw") }
func (c Config) SamplesDir() string { return filepath.Join(c.DataDir, "samples") }
func (c Config) BackupDir() string  { return filepath.Join(c.DataDir, "backup") }
func (c Config) StatusPath() string { return filepath.Join(c.DataDir, "status.json") }

// ParseCron parses a standard 5-field cron expression (minute hour day month weekday).
func ParseCron(expr string) (cron.Schedule, error) {
	return cron.ParseStandard(expr)
}

// Package supply plans Durotar Supply and Logistics hand-ins: Waylaid Crates for Merchant's
// Favor and Craftsman's Writs for reputation, priced from the Horde auction house.
//
// Facts here are sourced, never written from memory. The Blizzard API is authoritative for
// crate fill lists (item tooltips) and writ reputation (quests); the rest comes from the guides
// cited beside each value. Unknown values are left unknown.
package supply

// Item id ranges, from the Blizzard item API (static-forever-eu), checked against Horde AH data
// on 2026-11-16.
var (
	CrateIDs = idRange(248682, 248699, 248702, 248713) // "Waylaid Crate: <Tier> <Family>"
	WritIDs  = idRange(264011, 264160)                 // "Craftsman's Writ: <Item>"
)

// SealedWrits open into a random named order of their tier (item ids from the item API).
var SealedWrits = map[int64]string{262888: "Journeyman", 262889: "Expert", 262890: "Artisan"}

// QuestOffset maps a writ item to its daily quest: quest id = item id − QuestOffset
// (verified on the quest API and Wowhead: 264021→94221, 264037→94237, 264014→94214).
const QuestOffset = 169800

// Forged Delivery Receipt: 15 Merchant's Favor (needs Friendly), slapped on an empty crate for
// 50 reputation. Sources: https://www.wow-professions.com/forever/merchants-favor-durotar-supply-azeroth-commerce
// and https://forevercompanion.io/reputations
const (
	ReceiptFavor = 15
	ReceiptRep   = 50
)

// DSLFaction is Durotar Supply and Logistics' reputation faction id (quest API).
const DSLFaction = 2587

// DefaultPouch is the Coin Pouch a writ pays, in copper: the midpoint of the guide's ranges
// (Journeyman ~22–45s, Expert ~50–90s, Artisan ~75s on average).
// Source: https://conquestcapped.com/guides/wow-forever/wow-forever-craftsmans-writs/
var DefaultPouch = map[string]int64{"Journeyman": 3300, "Expert": 7000, "Artisan": 7500}

// tierRep is reputation per writ tier (quest API rewards: 75 / 125 / 200).
var tierRep = map[string]int{"Journeyman": 75, "Expert": 125, "Artisan": 200}

// TierForRep returns the writ tier a quest's reputation reward implies, or "".
func TierForRep(rep int) string {
	for t, r := range tierRep {
		if r == rep {
			return t
		}
	}
	return ""
}

// RequestedQty is how many of the requested item a writ asks for: one, except Crafted Solid
// Shot (200, https://www.wowhead.com/forever/quest=94237/craftsmans-writ-crafted-solid-shot).
func RequestedQty(item string) int64 {
	if item == "Crafted Solid Shot" {
		return 200
	}
	return 1
}

// Customer is who takes a named writ, and where.
type Customer struct {
	Tier   string `json:"tier"`
	Zone   string `json:"zone"`
	NPC    string `json:"npc"`
	Coords string `json:"coords"`
}

// Customers by requested item name. Source (read 2026-11-16):
// https://conquestcapped.com/guides/wow-forever/wow-forever-craftsmans-writs/
// Writs missing here have no known customer yet; empty NPC/Coords are unknown.
var Customers = map[string]Customer{
	"Elixir of Lesser Agility":    {Tier: "Journeyman", Zone: "Stonetalon Mountains", NPC: "Ziz Fizziks", Coords: "59.0, 62.4"},
	"Rough Bronze Leggings":       {Tier: "Journeyman", Zone: "Stonetalon Mountains", NPC: "Ziz Fizziks", Coords: "59.0, 62.4"},
	"Shadow Goggles":              {Tier: "Journeyman", Zone: "Stonetalon Mountains", NPC: "Ziz Fizziks", Coords: "59.0, 62.4"},
	"Heavy Quiver":                {Tier: "Journeyman", Zone: "Stonetalon Mountains", NPC: "Ziz Fizziks", Coords: "59.0, 62.4"},
	"Red Woolen Boots":            {Tier: "Journeyman", Zone: "Stonetalon Mountains", NPC: "Ziz Fizziks", Coords: "59.0, 62.4"},
	"Draught of Water Breathing":  {Tier: "Journeyman", Zone: "Wetlands", NPC: "Kixxle", Coords: "50.2, 37.8"},
	"Green Iron Boots":            {Tier: "Journeyman", Zone: "Wetlands", NPC: "Kixxle", Coords: "50.2, 37.8"},
	"Crafted Solid Shot":          {Tier: "Journeyman", Zone: "Wetlands", NPC: "Kixxle", Coords: "50.2, 37.8"},
	"Red Whelp Gloves":            {Tier: "Journeyman", Zone: "Wetlands", NPC: "Kixxle", Coords: "50.2, 37.8"},
	"Spider Silk Slippers":        {Tier: "Journeyman", Zone: "Wetlands", NPC: "Kixxle", Coords: "50.2, 37.8"},
	"Shadow Protection Potion":    {Tier: "Journeyman", Zone: "Duskwood", NPC: "Kzixx", Coords: "81.8, 19.8"},
	"Minor Recombobulator":        {Tier: "Journeyman", Zone: "Duskwood", NPC: "Kzixx", Coords: "81.8, 19.8"},
	"Dark Leather Tunic":          {Tier: "Journeyman", Zone: "Duskwood", NPC: "Kzixx", Coords: "81.8, 19.8"},
	"Small Silk Pack":             {Tier: "Journeyman", Zone: "Duskwood", NPC: "Kzixx", Coords: "81.8, 19.8"},
	"Silver Skeleton Key":         {Tier: "Journeyman", Zone: "Duskwood", NPC: "Battered Trunk (object)", Coords: ""},
	"Elixir of Defense":           {Tier: "Journeyman", Zone: "Thousand Needles", NPC: "Rizzle Brassbolts", Coords: ""},
	"Big Bronze Knife":            {Tier: "Journeyman", Zone: "Thousand Needles", NPC: "\"Plucky\" Johnson", Coords: "79.6, 75.6"},
	"Big Bronze Bomb":             {Tier: "Journeyman", Zone: "Thousand Needles", NPC: "Razzeric", Coords: ""},
	"Barbaric Gloves":             {Tier: "Journeyman", Zone: "Thousand Needles", NPC: "Goblin Pit Boss", Coords: ""},
	"Greater Adept's Robe":        {Tier: "Journeyman", Zone: "Thousand Needles", NPC: "Magus Tirth", Coords: ""},
	"Elixir of Wisdom":            {Tier: "Journeyman", Zone: "Arathi Highlands", NPC: "Muggol Breezebeard", Coords: "48.8, 55.8"},
	"Golden Rod":                  {Tier: "Journeyman", Zone: "Arathi Highlands", NPC: "Muggol Breezebeard", Coords: "48.8, 55.8"},
	"Explosive Sheep":             {Tier: "Journeyman", Zone: "Arathi Highlands", NPC: "Alyce", Coords: "34.0, 23.2"},
	"Fine Leather Pants":          {Tier: "Journeyman", Zone: "Arathi Highlands", NPC: "Alyce", Coords: "34.0, 23.2"},
	"Blue Overalls":               {Tier: "Journeyman", Zone: "Arathi Highlands", NPC: "Alyce", Coords: "34.0, 23.2"},
	"Lesser Mana Potion":          {Tier: "Journeyman", Zone: "Ashenvale", NPC: "Ash'alari", Coords: "60.2, 72.6"},
	"Iridescent Hammer":           {Tier: "Journeyman", Zone: "Ashenvale", NPC: "Cowardly Peon", Coords: "78.4, 68.8"},
	"Azure Silk Hood":             {Tier: "Journeyman", Zone: "Ashenvale", NPC: "Cowardly Peon", Coords: "78.4, 68.8"},
	"Moonsight Rifle":             {Tier: "Journeyman", Zone: "Ashenvale", NPC: "Grembly", Coords: ""},
	"Herbalist's Gloves":          {Tier: "Journeyman", Zone: "Ashenvale", NPC: "Murache", Coords: ""},
	"Lesser Troll's Blood Elixir": {Tier: "Journeyman", Zone: "Stranglethorn Vale", NPC: "Short John Mithril", Coords: "29.6, 47.4"},
	"Green Iron Gauntlets":        {Tier: "Journeyman", Zone: "Stranglethorn Vale", NPC: "\"Sea Wolf\" MacKinley", Coords: ""},
	"Gnomish Universal Remote":    {Tier: "Journeyman", Zone: "Stranglethorn Vale", NPC: "Scooty", Coords: "27.6, 77.4"},
	"Dark Leather Shoulders":      {Tier: "Journeyman", Zone: "Stranglethorn Vale", NPC: "Narkk", Coords: ""},
	"Colorful Kilt":               {Tier: "Journeyman", Zone: "Stranglethorn Vale", NPC: "Hekshi", Coords: ""},
	"Pearl-Handled Dagger":        {Tier: "Journeyman", Zone: "Alterac Mountains", NPC: "Alan Sneeks", Coords: "30.6, 61.0"},
	"Ornate Spyglass":             {Tier: "Journeyman", Zone: "Alterac Mountains", NPC: "Alan Sneeks", Coords: "30.6, 61.0"},
	"Lesser Wizard's Robe":        {Tier: "Journeyman", Zone: "Alterac Mountains", NPC: "Alan Sneeks", Coords: "30.6, 61.0"},
	"Heavy Leather Ball":          {Tier: "Journeyman", Zone: "Alterac Mountains", NPC: "Sos'mol", Coords: "38.4, 61.2"},
	"Phoenix Pants":               {Tier: "Journeyman", Zone: "Alterac Mountains", NPC: "Bath'rah the Windwatcher", Coords: ""},
	"Free Action Potion":          {Tier: "Journeyman", Zone: "Hillsbrad Foothills", NPC: "Webbed Mudsnout Gnoll", Coords: "62.2, 69.2"},
	"Heavy Bronze Mace":           {Tier: "Journeyman", Zone: "Hillsbrad Foothills", NPC: "Gorgrash", Coords: "80.0, 48.2"},
	"Practice Lock":               {Tier: "Journeyman", Zone: "Hillsbrad Foothills", NPC: "Gorgrash", Coords: "80.0, 48.2"},
	"Pilferer's Gloves":           {Tier: "Journeyman", Zone: "Hillsbrad Foothills", NPC: "Kris Legace", Coords: ""},
	"Lesser Invisibility Potion":  {Tier: "Expert", Zone: "Desolace", NPC: "Centaur Pariah", Coords: ""},
	"Inlaid Mithril Cylinder":     {Tier: "Expert", Zone: "Desolace", NPC: "Bibbly F'utzbuckle", Coords: ""},
	"Advanced Target Dummy":       {Tier: "Expert", Zone: "Desolace", NPC: "Bibbly F'utzbuckle", Coords: ""},
	"Guardian Cloak":              {Tier: "Expert", Zone: "Desolace", NPC: "Azore Aldamort", Coords: ""},
	"Shadoweave Gloves":           {Tier: "Expert", Zone: "Desolace", NPC: "Willow", Coords: ""},
	"Lesser Stoneshield Potion":   {Tier: "Expert", Zone: "Dustwallow Marsh", NPC: "Mudcrush Durtfeet", Coords: ""},
	"Steel Weapon Chain":          {Tier: "Expert", Zone: "Dustwallow Marsh", NPC: "Ugbert", Coords: ""},
	"Craftsman's Monocle":         {Tier: "Expert", Zone: "Dustwallow Marsh", NPC: "The Ravenous", Coords: ""},
	"Dusky Bracers":               {Tier: "Expert", Zone: "Dustwallow Marsh", NPC: "The Ravenous", Coords: ""},
	"Crimson Silk Robe":           {Tier: "Expert", Zone: "Dustwallow Marsh", NPC: "Tabetha", Coords: ""},
	"Nature Protection Potion":    {Tier: "Expert", Zone: "Felwood", NPC: "Greta Mosshoof", Coords: ""},
	"Ornate Mithril Shoulders":    {Tier: "Expert", Zone: "Felwood", NPC: "Taronn Redfeather", Coords: ""},
	"Goblin Land Mine":            {Tier: "Expert", Zone: "Felwood", NPC: "Jessir Moonbow", Coords: ""},
	"Wild Leather Helmet":         {Tier: "Expert", Zone: "Felwood", NPC: "Ivy Leafrunner", Coords: ""},
	"Shadoweave Robe":             {Tier: "Expert", Zone: "Felwood", NPC: "Eridan Bluewind", Coords: ""},
	"Great Rage Potion":           {Tier: "Expert", Zone: "Moonglade", NPC: "Dendrite Starblaze", Coords: ""},
	"Moonsteel Broadsword":        {Tier: "Expert", Zone: "Moonglade", NPC: "Meliri", Coords: ""},
	"Unstable Trigger":            {Tier: "Expert", Zone: "Moonglade", NPC: "Narianna", Coords: ""},
	"Quickdraw Quiver":            {Tier: "Expert", Zone: "Moonglade", NPC: "Narianna", Coords: ""},
	"Star Belt":                   {Tier: "Expert", Zone: "Moonglade", NPC: "Mylentha Riverbend", Coords: ""},
	"Mana Potion":                 {Tier: "Expert", Zone: "Riverglades", NPC: "Spek", Coords: ""},
	"Golden Scale Coif":           {Tier: "Expert", Zone: "Riverglades", NPC: "Spek", Coords: ""},
	"Flash Bomb":                  {Tier: "Expert", Zone: "Riverglades", NPC: "Spek", Coords: ""},
	"Swift Boots":                 {Tier: "Expert", Zone: "Riverglades", NPC: "Spek", Coords: ""},
	"Black Mageweave Vest":        {Tier: "Expert", Zone: "Riverglades", NPC: "Spek", Coords: ""},
	"Fire Protection Potion":      {Tier: "Expert", Zone: "Searing Gorge", NPC: "Durganon", Coords: ""},
	"Solid Sharpening Stone":      {Tier: "Expert", Zone: "Searing Gorge", NPC: "Master Smith Burninate", Coords: ""},
	"Fire Goggles":                {Tier: "Expert", Zone: "Searing Gorge", NPC: "Durganon", Coords: ""},
	"Nightscape Shoulders":        {Tier: "Expert", Zone: "Searing Gorge", NPC: "Evonice Sootsmoker", Coords: ""},
	"Red Mageweave Pants":         {Tier: "Expert", Zone: "Searing Gorge", NPC: "Lookout Captain Lolo Longstriker", Coords: ""},
	"Goblin Rocket Fuel":          {Tier: "Expert", Zone: "Tanaris", NPC: "Alchemist Pestlezugg", Coords: ""},
	"Truesilver Rod":              {Tier: "Expert", Zone: "Tanaris", NPC: "Spigot Operator Luglunket", Coords: ""},
	"Gyromatic Micro-Adjuster":    {Tier: "Expert", Zone: "Tanaris", NPC: "Jhordy Lapforge", Coords: ""},
	"Big Voodoo Robe":             {Tier: "Expert", Zone: "Tanaris", NPC: "Yeh'kinya", Coords: ""},
	"White Bandit Mask":           {Tier: "Expert", Zone: "Tanaris", NPC: "Vizzklick", Coords: ""},
	"Frost Protection Potion":     {Tier: "Expert", Zone: "Badlands", NPC: "Lotwil Veriatus", Coords: ""},
	"Steel Plate Helm":            {Tier: "Expert", Zone: "Badlands", NPC: "Furious Shadowforge Digger", Coords: ""},
	"Comfortable Leather Hat":     {Tier: "Expert", Zone: "Badlands", NPC: "Garek", Coords: ""},
	"Mageweave Bag":               {Tier: "Expert", Zone: "Badlands", NPC: "Rigglefuzz", Coords: ""},
	"Shadow Oil":                  {Tier: "Expert", Zone: "Swamp of Sorrows", NPC: "Mukomonji", Coords: ""},
	"Mithril Frag Bomb":           {Tier: "Expert", Zone: "Swamp of Sorrows", NPC: "Bullets Bigblast", Coords: ""},
	"Big Voodoo Mask":             {Tier: "Expert", Zone: "Swamp of Sorrows", NPC: "Mukomonji", Coords: ""},
	"Orange Martial Shirt":        {Tier: "Expert", Zone: "Swamp of Sorrows", NPC: "Bullets Bigblast", Coords: ""},
	"Elixir of Agility":           {Tier: "Expert", Zone: "The Hinterlands", NPC: "Juo", Coords: ""},
	"Mechanical Repair Kit":       {Tier: "Expert", Zone: "The Hinterlands", NPC: "Rhapsody Shindigger", Coords: ""},
	"Turtle Scale Breastplate":    {Tier: "Expert", Zone: "The Hinterlands", NPC: "Juo", Coords: ""},
	"Cindercloth Robe":            {Tier: "Expert", Zone: "The Hinterlands", NPC: "Atal'ai Exile", Coords: ""},
	"Wildvine Potion":             {Tier: "Expert", Zone: "Un'Goro Crater", NPC: "Spark Nilminer", Coords: ""},
	"Heavy Mithril Axe":           {Tier: "Expert", Zone: "Un'Goro Crater", NPC: "Torwa Pathfinder", Coords: ""},
	"Thick Leather Ammo Pouch":    {Tier: "Expert", Zone: "Un'Goro Crater", NPC: "Nergal", Coords: ""},
	"Earthen Silk Belt":           {Tier: "Expert", Zone: "Un'Goro Crater", NPC: "Petra Grossen", Coords: ""},
	"Catseye Draught":             {Tier: "Expert", Zone: "Feralas", NPC: "Scout Raroul", Coords: ""},
	"Blue Glittering Axe":         {Tier: "Expert", Zone: "Feralas", NPC: "Scout Raroul", Coords: ""},
	"Catseye Ultra Goggles":       {Tier: "Expert", Zone: "Feralas", NPC: "Scout Raroul", Coords: ""},
	"Mechanical Dragonling":       {Tier: "Expert", Zone: "Badlands", NPC: "", Coords: ""},
	"Wild Leather Shoulders":      {Tier: "Expert", Zone: "Feralas", NPC: "", Coords: ""},
	"Enchanted Mageweave Pouch":   {Tier: "Expert", Zone: "Felwood", NPC: "", Coords: ""},
	"Dreamless Sleep Potion":      {Tier: "Artisan", Zone: "Blasted Lands", NPC: "Kum'isha the Collector", Coords: ""},
	"Radiant Belt":                {Tier: "Artisan", Zone: "Blasted Lands", NPC: "Kum'isha the Collector", Coords: ""},
	"Runecloth Bag":               {Tier: "Artisan", Zone: "Blasted Lands", NPC: "Kum'isha the Collector", Coords: ""},
	"Draught of Detect Demon":     {Tier: "Artisan", Zone: "Deadwind Pass", NPC: "Hastings", Coords: ""},
	"Runic Leather Headband":      {Tier: "Artisan", Zone: "Deadwind Pass", NPC: "Hastings", Coords: ""},
	"Tuxedo Jacket":               {Tier: "Artisan", Zone: "Deadwind Pass", NPC: "Hastings", Coords: ""},
	"Gift of Arthas":              {Tier: "Artisan", Zone: "Eastern Plaguelands", NPC: "Adon", Coords: ""},
	"Wicked Leather Bracers":      {Tier: "Artisan", Zone: "Eastern Plaguelands", NPC: "Quartermaster Miranda Breechlock", Coords: ""},
	"Truesilver Transformer":      {Tier: "Artisan", Zone: "Silithus", NPC: "Khur Hornstriker", Coords: ""},
	"Shadoweave Mask":             {Tier: "Artisan", Zone: "Silithus", NPC: "Hermit Ortell", Coords: ""},
	"Potion of Demonslaying":      {Tier: "Artisan", Zone: "Azshara", NPC: "Archmage Xylem", Coords: ""},
	"Runecloth Boots":             {Tier: "Artisan", Zone: "Burning Steppes", NPC: "Gorzeeki Wildeyes", Coords: ""},
	"Runic Leather Bracers":       {Tier: "Artisan", Zone: "Western Plaguelands", NPC: "Fallen Adventurer", Coords: ""},
	"Mithril Spurs":               {Tier: "Artisan", Zone: "Deadwind Pass", NPC: "", Coords: ""},
	"Soul Pouch":                  {Tier: "Artisan", Zone: "Winterspring", NPC: "", Coords: ""},
}

// Continents by zone, each from the zone's Warcraft Wiki page (infobox or lead; for zones whose
// infobox names a sub-region such as Lordaeron or Khaz Modan, the page's "Regions of the Eastern
// Kingdoms" listing).
var Continents = map[string]string{
	"Stonetalon Mountains": "Kalimdor",         // https://warcraft.wiki.gg/wiki/Stonetalon_Mountains
	"Thousand Needles":     "Kalimdor",         // https://warcraft.wiki.gg/wiki/Thousand_Needles
	"Ashenvale":            "Kalimdor",         // https://warcraft.wiki.gg/wiki/Ashenvale
	"Desolace":             "Kalimdor",         // https://warcraft.wiki.gg/wiki/Desolace
	"Dustwallow Marsh":     "Kalimdor",         // https://warcraft.wiki.gg/wiki/Dustwallow_Marsh
	"Felwood":              "Kalimdor",         // https://warcraft.wiki.gg/wiki/Felwood
	"Moonglade":            "Kalimdor",         // https://warcraft.wiki.gg/wiki/Moonglade
	"Tanaris":              "Kalimdor",         // https://warcraft.wiki.gg/wiki/Tanaris
	"Un'Goro Crater":       "Kalimdor",         // https://warcraft.wiki.gg/wiki/Un%27Goro_Crater
	"Feralas":              "Kalimdor",         // https://warcraft.wiki.gg/wiki/Feralas
	"Silithus":             "Kalimdor",         // https://warcraft.wiki.gg/wiki/Silithus
	"Azshara":              "Kalimdor",         // https://warcraft.wiki.gg/wiki/Azshara
	"Winterspring":         "Kalimdor",         // https://warcraft.wiki.gg/wiki/Winterspring
	"Wetlands":             "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Wetlands
	"Duskwood":             "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Duskwood
	"Arathi Highlands":     "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Arathi_Highlands
	"Stranglethorn Vale":   "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Stranglethorn_Vale
	"Alterac Mountains":    "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Alterac_Mountains
	"Hillsbrad Foothills":  "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Hillsbrad_Foothills
	"Searing Gorge":        "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Searing_Gorge
	"Badlands":             "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Badlands
	"Swamp of Sorrows":     "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Swamp_of_Sorrows
	"The Hinterlands":      "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Hinterlands
	"Blasted Lands":        "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Blasted_Lands
	"Deadwind Pass":        "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Deadwind_Pass
	"Eastern Plaguelands":  "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Eastern_Plaguelands
	"Burning Steppes":      "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Burning_Steppes
	"Western Plaguelands":  "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Western_Plaguelands
	"Riverglades":          "Eastern Kingdoms", // https://warcraft.wiki.gg/wiki/Riverglades
}

func idRange(bounds ...int64) []int64 {
	var ids []int64
	for i := 0; i+1 < len(bounds); i += 2 {
		for id := bounds[i]; id <= bounds[i+1]; id++ {
			ids = append(ids, id)
		}
	}
	return ids
}

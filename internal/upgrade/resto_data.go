package upgrade

// Tables for the Restoration druid weight model (resto.go), per the resto weights spec of
// 2026-11-19. Update them here when Forever patches.

// restoIPC is Intellect per 1% spell crit by level (index = level, 1–60; foreverdb.net, from the
// Forever client).
var restoIPC = [61]float64{0,
	6.9, 7.2, 7.5, 7.8, 8.1, 8.4, 8.7, 8.7, 9.1, 10.0, 10.6, 11.6, 12.2, 13.1, 14.4, 15.0, 15.9, 16.6, 17.5, 19.0,
	19.7, 20.6, 21.2, 22.5, 23.4, 24.0, 25.0, 25.6, 26.9, 28.4, 29.1, 30.3, 31.0, 31.8, 33.1, 33.8, 34.7, 35.6, 36.5, 38.5,
	39.1, 40.3, 41.0, 42.2, 43.1, 44.1, 45.0, 45.9, 47.2, 48.8, 49.8, 51.0, 51.5, 52.9, 54.1, 54.9, 55.9, 56.8, 58.1, 59.9}

// restoBaseMana is a druid's base mana by level (index = level). Matches the Forever client's
// gametables/basemp.txt (Druid column, build 1.60.1.70959) at all 60 levels.
var restoBaseMana = [61]float64{0,
	50, 57, 65, 74, 84, 95, 107, 120, 134, 149, 165, 182, 200, 219, 239, 260, 282, 305, 329, 354,
	380, 392, 420, 449, 479, 509, 524, 554, 584, 614, 629, 659, 689, 704, 734, 749, 779, 809, 824, 854,
	869, 899, 914, 944, 959, 989, 1004, 1019, 1049, 1064, 1079, 1109, 1124, 1139, 1154, 1169, 1199, 1214, 1229, 1244}

// Spell is one of the modelled heals.
type Spell string

const (
	Rejuv    Spell = "rejuv"
	Regrowth Spell = "regrowth"
	HT       Spell = "ht"
)

// restoOrder is the order spells are summed in (it matters only for float rounding).
var restoOrder = []Spell{Rejuv, Regrowth, HT}

type spellRank struct {
	learn      int
	mana, base float64
	coef       float64
}

type spellData struct {
	hot    bool
	direct float64 // share of the heal that lands directly (Regrowth 43%, assumed)
	ranks  []spellRank
}

// restoSpells: ranks as [learn level, mana, base heal, coefficient] (RankedBoost; the client's
// SpellEffect agrees on mana and coefficients, its average base heals are within ~2%).
var restoSpells = map[Spell]spellData{
	Rejuv: {hot: true, ranks: []spellRank{{4, 25, 32, .8}, {10, 40, 48, .8}, {16, 75, 92, .8}, {22, 105, 128, .8},
		{28, 135, 168, .8}, {34, 160, 204, .8}, {40, 195, 284, .8}, {46, 235, 376, .8}, {52, 280, 496, .8},
		{58, 335, 644, .8}, {60, 360, 776, .8}}},
	Regrowth: {hot: true, direct: 0.43, ranks: []spellRank{{12, 70, 187, .783}, {18, 125, 331, .783},
		{24, 170, 475, .783}, {30, 210, 623, .783}, {36, 250, 779, .783}, {42, 305, 1004, .783},
		{48, 370, 1288, .783}, {54, 445, 1638, .783}, {60, 525, 2015, .783}}},
	HT: {ranks: []spellRank{{1, 25, 47, .429}, {8, 55, 107, .571}, {14, 110, 211, .714}, {20, 190, 376, .857},
		{26, 280, 564, 1}, {32, 350, 727, 1}, {38, 425, 921, 1}, {44, 520, 1199, 1}, {50, 630, 1546, 1},
		{56, 755, 1946, 1}, {60, 840, 2332, 1}}},
}

// Talent names, as in the Maxroll 11/0/40 levelling order.
type Talent string

const (
	HotW         Talent = "hotw"
	Naturalist   Talent = "naturalist"
	Bloom        Talent = "bloom"
	GoN          Talent = "gon"
	Swiftmend    Talent = "swiftmend"
	ImpRejuv     Talent = "imprejuv"
	LivingSpirit Talent = "livingspirit"
	NS           Talent = "ns"
	Reflection   Talent = "reflection"
	ImpRegrowth  Talent = "impregrowth"
	WildGrowth   Talent = "wg"
	Tranq        Talent = "tranq"
)

// restoTalentOrder: the level each talent point lands at (Maxroll 11/0/40, via timesaver.gg).
var restoTalentOrder = map[Talent][]int{
	HotW: {10, 11, 12, 13, 14}, Naturalist: {15, 16, 17, 18, 19}, Bloom: {20}, GoN: {21, 22, 23, 24, 29},
	Swiftmend: {25}, ImpRejuv: {26, 27, 28}, LivingSpirit: {30, 31, 32}, NS: {33}, Reflection: {34, 35, 36},
	ImpRegrowth: {37, 38, 39, 52, 53}, WildGrowth: {40}, Tranq: {56, 57, 58, 59, 60},
}

// reflectionShare is Spirit regen kept while casting per Reflection rank (Kami Labs).
var reflectionShare = [4]float64{0, .17, .33, .50}

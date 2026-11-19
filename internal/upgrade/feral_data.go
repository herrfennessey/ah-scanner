package upgrade

// Tables for the Feral (cat) DPS model (feral.go), per the feral weights spec of 2026-11-19.
// Ranks are from wowsims/forever (sim/core/spelldata/spells_auto_gen.go, built from the Forever
// game files). Update them here when Forever patches.

// feralAPC is Agility per 1% melee crit for druids by level (index = level, 1–60; foreverdb.net,
// from the Forever client).
var feralAPC = [61]float64{0,
	4.9, 4.9, 5.1, 5.1, 5.4, 5.4, 5.6, 5.6, 5.9, 6.3, 6.6, 6.6, 6.8, 6.8, 7.3, 7.3, 7.6, 7.8, 7.8, 8.8,
	8.8, 9.0, 9.3, 9.3, 9.8, 9.8, 10.0, 10.2, 10.2, 11.2, 11.5, 11.5, 11.7, 11.9, 12.2, 12.4, 12.7, 12.7, 12.9, 13.9,
	14.1, 14.1, 14.4, 14.6, 15.1, 15.4, 15.4, 15.6, 15.8, 16.8, 17.1, 17.3, 17.6, 17.6, 18.1, 18.3, 18.6, 18.8, 19.0, 20.0}

// catRank is one ability rank: the level it's learned at and up to two values (see each table).
type catRank struct {
	learn int
	a, b  float64
}

const (
	clawCost, shredCost, rakeCost, ripCost, biteCost = 45.0, 60.0, 40.0, 30.0, 35.0
	clawPct, shredPct                                = 1.4, 2.0 // × paw damage
	rakeAP, ripAP, biteAP                            = 0.065, 0.0114, 0.037
	rakeTicks, rakeDur, ripTicks, ripDur             = 3.0, 9.0, 6.0, 12.0
)

var (
	clawRanks  = []catRank{{20, 27, 0}, {28, 39, 0}, {38, 57, 0}, {48, 88, 0}, {58, 115, 0}}                        // a = flat damage
	shredRanks = []catRank{{20, 22, 0}, {30, 32, 0}, {38, 44, 0}, {46, 64, 0}, {54, 80, 0}}                         // a = flat damage
	rakeRanks  = []catRank{{24, 27, 20}, {34, 39, 26}, {44, 56, 32}, {54, 76, 42}}                                  // a = direct, b = tick
	ripRanks   = []catRank{{20, 4, 5.5}, {28, 5, 9}, {36, 8, 10.5}, {44, 10, 15.8}, {52, 14, 22.5}, {60, 19, 31.9}} // a = tick, b = per CP
	biteRanks  = []catRank{{24, 17, 30}, {32, 27, 45}, {40, 40, 74}, {48, 62, 115}, {56, 87, 160}, {60, 102, 181}}  // a = base, b = per CP
)

// Feral talents (Heart of the Wild is HotW, shared with Restoration).
const (
	Furor               Talent = "furor"
	FeralSwiftness      Talent = "feralswiftness"
	FeralInstinct       Talent = "feralinstinct"
	FeralCharge         Talent = "feralcharge"
	SharpenedClaws      Talent = "sharpenedclaws"
	ShreddingAttacks    Talent = "shreddingattacks"
	PredatoryDiscipline Talent = "predatorydiscipline"
	PredatoryStrikes    Talent = "predatorystrikes"
	BloodFrenzy         Talent = "bloodfrenzy"
	LeaderOfThePack     Talent = "leaderofthepack"
	NaturalInstinct     Talent = "naturalinstinct"
	RendAndTear         Talent = "rendandtear"
	ShiftingPower       Talent = "shiftingpower"
	SavageFury          Talent = "savagefury"
	Ferocity            Talent = "ferocity"
)

// feralTalentOrder: the level each point lands at (Maxroll 7/34/10 Feral, via timesaver.gg).
var feralTalentOrder = map[Talent][]int{
	Furor: {10, 11, 12, 13, 14}, FeralSwiftness: {15, 16}, FeralInstinct: {17, 18, 19}, FeralCharge: {20},
	SharpenedClaws: {21, 22}, ShreddingAttacks: {23, 24, 34}, PredatoryDiscipline: {25}, PredatoryStrikes: {26, 27, 28},
	BloodFrenzy: {29, 33}, LeaderOfThePack: {30}, NaturalInstinct: {31, 32}, RendAndTear: {35, 36, 37, 38, 39},
	ShiftingPower: {40}, HotW: {41, 42, 43, 44, 45}, SavageFury: {58, 59}, Ferocity: {60},
}

package upgrade

import (
	"errors"
	"math"
)

// The Restoration model turns a character sheet and a content profile into per-point gear
// weights relative to +1 healing power, over one mana bar (T seconds of combat between drinks).
// Two regimes are blended by Pressure: throughput (you finish with mana to spare: only bigger
// casts count) and mana-limited (total healing = total mana × healing per mana). Port of the
// spec's model.js; TestRestoVectors pins it to the spec's test vectors.

// RestoInputs is the character sheet and content profile. Spec §2 lists the defaults.
type RestoInputs struct {
	Level                int
	Int, Spirit, HP, MP5 float64 // sheet totals after talents and buffs
	CritPct              float64 // sheet spell crit %
	ManaOverride         float64 // max mana from the sheet; 0 = calculate it
	T                    float64 // combat seconds per mana bar
	Pressure             float64 // 0–1: how often the fight ends because you went OOM
	FSR                  float64 // share of combat inside the five-second rule
	InnervateOn          bool
	InnCount             float64 // self-Innervates per bar
	Mix                  map[Spell]float64
	Rank                 map[Spell]int // rank index; -1 or missing = highest learned
	Talents              map[Talent]int
	SpiCoef, RegenBase   float64 // mana/s per Spirit outside the FSR; constant mana/s
	BloomEff             float64 // share of Bloom that isn't overheal
	BloomGoN             bool    // Gift of Nature applies to Bloom
}

// Profile is a content preset: combat seconds per bar, pressure, FSR, Innervates per bar.
type Profile struct {
	Name                  string
	T, Pressure, FSR, Inn float64
}

// RestoProfiles are the spec's defaults per content type.
var RestoProfiles = map[string]Profile{
	"dungeon":  {"Dungeon", 180, 0.7, 0.8, 1},
	"questing": {"Questing", 60, 0.2, 0.6, 0},
	"boss":     {"Boss", 300, 1.0, 0.9, 1},
}

// DefaultRestoInputs fills everything but the sheet for a level and profile.
func DefaultRestoInputs(level int, p Profile) RestoInputs {
	return RestoInputs{Level: level, CritPct: 5, T: p.T, Pressure: p.Pressure, FSR: p.FSR,
		InnervateOn: p.Inn > 0, InnCount: p.Inn, Mix: map[Spell]float64{Rejuv: 50, Regrowth: 30, HT: 20},
		Talents: DefaultTalents(level), SpiCoef: 0.1, RegenBase: 7.5, BloomEff: 1, BloomGoN: true}
}

// RestoWeights are per gear point, relative to +1 healing power. Parts explains Int and Spirit.
type RestoWeights struct {
	HP, Spirit, MP5, Int, Crit float64            // Crit is per 1% crit
	Parts                      map[string]float64 // intCrit, intMana, spiBloom, spiRegen, spiInn
	M, MaxMana, HPM            float64
}

// DefaultTalents is the talents bought by a level in the Maxroll levelling order.
func DefaultTalents(level int) map[Talent]int {
	t := map[Talent]int{}
	for k, levels := range restoTalentOrder {
		for _, l := range levels {
			if l <= level {
				t[k]++
			}
		}
	}
	return t
}

func maxRank(s Spell, level int) int {
	i := -1
	for j, r := range restoSpells[s].ranks {
		if r.learn <= level {
			i = j
		}
	}
	return i
}

// ComputeResto runs the model. It fails on a level outside 10–60 or a spell mix summing to 0.
func ComputeResto(in RestoInputs) (RestoWeights, error) {
	L := in.Level
	if L < 10 || L > 60 {
		return RestoWeights{}, errors.New("level must be 10–60")
	}
	t := in.Talents
	if t == nil {
		t = DefaultTalents(L)
	}
	hotw, ls, gon := 0.02*float64(t[HotW]), 0.05*float64(t[LivingSpirit]), 0.02*float64(t[GoN])
	refl := reflectionShare[min(max(t[Reflection], 0), 3)]

	maxMana := in.ManaOverride
	// Max mana is never below base mana: a smaller sheet value isn't a mana pool (missing, or
	// the energy or rage of a form), so calculate it instead.
	if maxMana < restoBaseMana[L] {
		maxMana = restoBaseMana[L] + math.Min(in.Int, 20) + 15*math.Max(in.Int-20, 0)
	}
	regenOut := in.RegenBase + in.SpiCoef*in.Spirit // mana/s outside the FSR
	fNorm := (1 - in.FSR) + in.FSR*refl             // share of Spirit regen you actually get
	effRegen := regenOut*fNorm + in.MP5/5
	innOn := in.InnervateOn && L >= 40
	M := maxMana + effRegen*in.T
	if innOn {
		M += 20 * regenOut * (5 - fNorm) * in.InnCount
	}

	var sum float64
	for _, s := range restoOrder {
		if in.Mix[s] > 0 {
			sum += in.Mix[s]
		}
	}
	if sum <= 0 {
		return RestoWeights{}, errors.New("the spell mix sums to 0")
	}
	crit := in.CritPct / 100
	var HPM, dHP, dCrit, dBloom float64
	for _, s := range restoOrder {
		share := in.Mix[s] / sum
		if !(share > 0) {
			continue
		}
		sp := restoSpells[s]
		ri, ok := in.Rank[s]
		if !ok || ri < 0 {
			ri = maxRank(s, L)
		}
		if ri < 0 || ri >= len(sp.ranks) {
			continue
		}
		r := sp.ranks[ri]
		coef, mana := r.coef, r.mana
		if r.learn < 20 {
			coef *= 1 - float64(20-r.learn)*0.0375
		}
		if s == HT {
			mana *= 1 - 0.02*float64(t[Tranq])
		}
		mult := 1 + gon
		if s == Rejuv {
			mult *= 1 + 0.05*float64(t[ImpRejuv])
		}
		critMult := 1 + 0.5*crit
		critChance := crit
		if s == Regrowth {
			critChance += 0.1 * float64(t[ImpRegrowth])
			critMult = 1 + 0.5*(sp.direct*math.Min(1, critChance)+(1-sp.direct)*crit)
		}
		nc := (r.base + coef*in.HP) * mult // non-crit heal
		H := nc * critMult
		var bloom float64
		if t[Bloom] > 0 && sp.hot {
			bloom = in.BloomEff
			if in.BloomGoN {
				bloom *= 1 + gon
			}
		}
		HPM += share * (H + bloom*in.Spirit) / mana
		dHP += share * coef * mult * critMult / mana
		critable := nc
		if s == Regrowth { // full crit gain unless the direct part is already at 100% (as model.js)
			critable = nc * (1 - sp.direct)
			if critChance < 1 {
				critable += nc * sp.direct
			}
		}
		dCrit += share * 0.005 * critable / mana // per +1% crit; crits heal 150%
		dBloom += share * bloom / mana
	}
	if dHP <= 0 {
		return RestoWeights{}, errors.New("no learned spell in the mix")
	}

	gInt, gSpi, p := 1+hotw, 1+ls, in.Pressure
	manaPart := HPM / (M * dHP) // healing value of 1 mana, in healing-power units
	w := RestoWeights{HP: 1, Crit: dCrit / dHP, M: M, MaxMana: maxMana, HPM: HPM}
	intCrit := gInt / restoIPC[L] * w.Crit
	intMana := 15 * gInt * manaPart
	spiBloom := gSpi * dBloom / dHP
	spiRegen := gSpi * in.SpiCoef * fNorm * in.T * manaPart
	var spiInn float64
	if innOn {
		spiInn = gSpi * in.SpiCoef * (5 - fNorm) * 20 * in.InnCount * manaPart
	}
	w.Int = intCrit + p*intMana
	w.Spirit = spiBloom + p*(spiRegen+spiInn)
	w.MP5 = p * (in.T / 5) * manaPart
	w.Parts = map[string]float64{"intCrit": intCrit, "intMana": p * intMana, "spiBloom": spiBloom,
		"spiRegen": p * spiRegen, "spiInn": p * spiInn}
	return w, nil
}

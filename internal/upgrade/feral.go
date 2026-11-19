package upgrade

import (
	"errors"
	"math"
)

// The Feral model is an analytic steady-state cat DPS model (energy, combo points, Omen of
// Clarity, the attack table by level difference); weights are finite differences normalised to
// +1 attack power. Port of the feral spec's model.js, plus two talents from Blizzard's tooltips
// the spec left out (Natural Instinct's crit bonus, Ferocity on Rake); TestFeralVectors pins it.

// CatInputs is the Cat Form sheet and the fight.
type CatInputs struct {
	Level, Delta             int     // your level; target level − yours
	Armor                    float64 // target armor
	AP, CritPct              float64 // Cat Form sheet attack power and melee crit
	HitPct, ExpPct, HastePct float64 // from gear (10 rating = 1%)
	WeaponDPS                float64
	Behind                   float64 // share of builders that can be Shred
	Rake                     bool
	RipShare                 float64 // share of finishers that are Rip (≤ one per 12 s)
	ExtraEnergy              float64 // energy/s not modelled
	Talents                  map[Talent]int
}

// CatProfile is a fight preset: level difference, share from behind, Rake, Rip share.
type CatProfile struct {
	Name             string
	Delta            int
	Behind, RipShare float64
	Rake             bool
}

// CatProfiles match the Restoration content names.
var CatProfiles = map[string]CatProfile{
	"questing": {"Questing", 0, 0.3, 0, false},
	"dungeon":  {"Dungeon", 1, 0.6, 0.3, true},
	"boss":     {"Boss", 2, 1, 1, true},
}

// CatResult is the DPS and where it comes from.
type CatResult struct {
	DPS   float64            `json:"dps"`
	Parts map[string]float64 `json:"parts"` // white, builder, rake, rip, fb
	Rates map[string]float64 `json:"rates"` // nb, f, ripRate, fbRate, rakeRate, ooc
}

// CatWeightsOut are per point relative to +1 AP; Crit, Hit, Exp and Haste are per 1%, WeaponDPS
// per 1 DPS.
type CatWeightsOut struct {
	Str, Agi, Crit, Hit, Exp, Haste, WeaponDPS float64
	DPS, PerAP                                 float64
}

// DefaultFeralTalents is the talents bought by a level in the Maxroll levelling order.
func DefaultFeralTalents(level int) map[Talent]int {
	t := map[Talent]int{}
	for k, levels := range feralTalentOrder {
		for _, l := range levels {
			if l <= level {
				t[k]++
			}
		}
	}
	return t
}

func catRankAt(ranks []catRank, level int) int {
	i := -1
	for j, r := range ranks {
		if r.learn <= level {
			i = j
		}
	}
	return i
}

type attackTable struct{ miss, dodge, glance, gMult, supp, auraSupp float64 }

// catTable is the attack table by level difference (wowsims attack_table_test.go).
func catTable(delta int) attackTable {
	d := max(-10, min(3, delta))
	t := attackTable{miss: 0.08, dodge: 0.065, gMult: 0.65}
	if d <= 2 {
		t.miss, t.dodge = 0.05+0.005*float64(d), 0.05+0.005*float64(d)
	}
	if d >= 0 {
		t.glance = []float64{0.10, 0.20, 0.30, 0.40}[d]
	}
	switch {
	case d <= 1:
		t.gMult = 0.95
	case d == 2:
		t.gMult = 0.85
	}
	if d <= 0 {
		t.supp = 0.002 * float64(d)
	} else {
		t.supp = []float64{0.01, 0.02, 0.03}[d-1]
	}
	if d >= 3 {
		t.auraSupp = 0.018
	}
	t.miss, t.dodge = math.Max(0, t.miss), math.Max(0, t.dodge)
	return t
}

func clamp01(x float64) float64 { return math.Min(1, math.Max(0, x)) }

// CatDPS is the steady-state DPS. Zero below level 20 (no Cat Form).
func CatDPS(in CatInputs) CatResult {
	L, t := in.Level, in.Talents
	if L < 20 {
		return CatResult{}
	}
	tb := catTable(in.Delta)
	miss, dodge := math.Max(0, tb.miss-in.HitPct/100), math.Max(0, tb.dodge-in.ExpPct/100)
	land := 1 - miss - dodge
	crit := in.CritPct / 100
	cY, cB := clamp01(crit-tb.supp), clamp01(crit-tb.supp-tb.auraSupp)
	am := 1 - math.Min(0.75, in.Armor/(in.Armor+400+85*float64(L))) // Classic armor formula (assumed)
	D := in.WeaponDPS + in.AP/14                                    // paw damage per 1.0 s swing
	sf, bf := 1+0.05*float64(t[SavageFury]), 0.5*float64(t[BloodFrenzy])
	rClaw, rShred := catRankAt(clawRanks, L), catRankAt(shredRanks, L)
	rRake, rRip, rFb := catRankAt(rakeRanks, L), catRankAt(ripRanks, L), catRankAt(biteRanks, L)
	useRake := in.Rake && rRake >= 0
	effCost := func(c float64) float64 { return c * (land + 0.2*(1-land)) / land } // 80% refund on a miss
	b := in.Behind
	cb := b*effCost(shredCost-6*float64(t[ShreddingAttacks])) + (1-b)*effCost(clawCost-float64(t[Ferocity]))
	k := 1 + cY*bf // CP per landed builder
	ripShare := in.RipShare
	if rFb < 0 {
		ripShare = 1
	}
	swings := 1 + in.HastePct/100
	var ooc, f, nb, rakeRate, ripRate, fbRate float64
	for range 4 {
		E := 10 + in.ExtraEnergy + ooc
		rakeRate = 0
		if useRake {
			rakeRate = 1 / rakeDur
		}
		rakeE := rakeRate * effCost(rakeCost-float64(t[Ferocity])) // Ferocity: Claw and Rake (its tooltip)
		cpRake := rakeRate * (1 + cY*bf)
		rs := ripShare
		cf := rs*effCost(ripCost) + (1-rs)*effCost(biteCost)
		f = math.Max(0, (E-rakeE+cpRake*cb/k)/(5*cb/k+cf))
		if rs*f > 1/ripDur {
			rs = (1 / ripDur) / f
			cf = rs*effCost(ripCost) + (1-rs)*effCost(biteCost)
			f = math.Max(0, (E-rakeE+cpRake*cb/k)/(5*cb/k+cf))
			rs = math.Min(1, (1/ripDur)/f)
		}
		if rFb < 0 {
			f, rs, cf = math.Min(f, 1/ripDur), 1, effCost(ripCost)
		}
		ripRate, fbRate = f*rs, f*(1-rs)
		nb = math.Max(0, (E-rakeE-f*cf)/cb)
		casts := (nb + f + rakeRate) / land
		ooc = (2.0 / 60) * (swings + casts) * cb // Omen of Clarity: 2 PPM, a free builder
	}
	rt := 1.0
	if useRake || ripRate > 0 {
		rt = 1 + 0.03*float64(t[RendAndTear])
	}
	critW := math.Min(cY, math.Max(0, 1-miss-dodge-tb.glance))
	hitW := 1 - miss - dodge - tb.glance - critW
	white := swings * D * am * (hitW + tb.glance*tb.gMult + 2*critW)
	// Natural Instinct: "increases the critical strike damage bonus of your melee abilities by 20%"
	// at 2/2, so +10% of the bonus per rank on Claw, Shred, Rake's hit and Bite. Auto attacks and
	// bleed ticks keep ×2 (our reading of "melee abilities").
	yc, bc := 1+cY*(1+0.1*float64(t[NaturalInstinct])), 1+cB
	claw := (clawPct*D + clawRanks[rClaw].a) * sf * rt * am * yc
	shred := (shredPct*D + shredRanks[rShred].a) * sf * rt * am * yc
	builder := b*shred + (1-b)*claw
	var rake, rip, fb float64
	if useRake {
		r := rakeRanks[rRake]
		rake = (r.a+rakeAP*in.AP)*sf*rt*am*yc + rakeTicks*(r.b+rakeAP*in.AP)*sf*rt*bc
	}
	if rRip >= 0 {
		r := ripRanks[rRip]
		rip = ripTicks * (r.a + r.b*5 + ripAP*4*in.AP) * rt * bc
	}
	if rFb >= 0 {
		r := biteRanks[rFb]
		fb = (r.a + r.b*5 + biteAP*5*in.AP) * rt * am * yc
	}
	parts := map[string]float64{"white": white, "builder": nb * builder, "rake": rakeRate * rake,
		"rip": ripRate * rip, "fb": fbRate * fb}
	return CatResult{DPS: parts["white"] + parts["builder"] + parts["rake"] + parts["rip"] + parts["fb"], Parts: parts,
		Rates: map[string]float64{"nb": nb, "f": f, "ripRate": ripRate, "fbRate": fbRate, "rakeRate": rakeRate, "ooc": ooc}}
}

// CatWeights are the finite-difference weights. It fails below level 20.
func CatWeights(in CatInputs) (CatWeightsOut, error) {
	base := CatDPS(in).DPS
	if base <= 0 {
		return CatWeightsOut{}, errors.New("Cat Form is learned at level 20")
	}
	d := func(mod func(*CatInputs), step float64) float64 {
		x := in
		mod(&x)
		return (CatDPS(x).DPS - base) / step
	}
	perAP := d(func(x *CatInputs) { x.AP += 10 }, 10)
	w := CatWeightsOut{DPS: base, PerAP: perAP,
		Str:       2 * (1 + 0.02*float64(in.Talents[HotW])),
		Agi:       d(func(x *CatInputs) { x.AP += 10; x.CritPct += 10 / feralAPC[in.Level] }, 10) / perAP,
		Crit:      d(func(x *CatInputs) { x.CritPct++ }, 1) / perAP,
		Hit:       d(func(x *CatInputs) { x.HitPct++ }, 1) / perAP,
		Exp:       d(func(x *CatInputs) { x.ExpPct++ }, 1) / perAP,
		Haste:     d(func(x *CatInputs) { x.HastePct++ }, 1) / perAP,
		WeaponDPS: d(func(x *CatInputs) { x.WeaponDPS++ }, 1) / perAP}
	return w, nil
}

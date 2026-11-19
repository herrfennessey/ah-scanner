package upgrade

import (
	"math"
	"testing"
)

// The feral spec's §8 cases (Hit, Exp, Haste and ExtraEnergy 0 unless set; default talents), with the
// values its model.js gives once Natural Instinct and Ferocity on Rake are added (L25 has neither).
func TestFeralVectors(t *testing.T) {
	for _, c := range []struct {
		name                                  string
		in                                    CatInputs
		dps                                   float64
		str, agi, crit, hit, exp, haste, wdps float64
	}{
		{"dungeon trash L40", CatInputs{Level: 40, Delta: 1, Armor: 2460, AP: 400, CritPct: 15, WeaponDPS: 25, Behind: .6, Rake: true, RipShare: .3},
			101.37, 2.000, 1.796, 10.964, 3.985, 3.985, 3.861, 7.768},
		{"boss L40", CatInputs{Level: 40, Delta: 2, Armor: 2520, AP: 400, CritPct: 15, WeaponDPS: 25, Behind: 1, Rake: true, RipShare: 1},
			108.90, 2.000, 1.837, 11.532, 3.887, 3.887, 3.536, 7.193},
		{"questing L40", CatInputs{Level: 40, Delta: 0, Armor: 2400, AP: 400, CritPct: 15, WeaponDPS: 25, Behind: .3, Rake: false, RipShare: 0},
			74.74, 2.000, 1.862, 11.887, 5.561, 5.561, 5.579, 12.638},
		{"trash L25", CatInputs{Level: 25, Delta: 1, Armor: 1560, AP: 250, CritPct: 10, WeaponDPS: 15, Behind: .6, Rake: true, RipShare: .3},
			51.61, 2.000, 1.572, 5.504, 2.619, 2.619, 2.497, 8.284},
		{"boss L60", CatInputs{Level: 60, Delta: 3, Armor: 3731, AP: 1200, CritPct: 25, WeaponDPS: 55, Behind: 1, Rake: true, RipShare: 1, HitPct: 5},
			319.99, 2.200, 2.475, 29.413, 9.415, 9.415, 8.733, 6.864},
	} {
		c.in.Talents = DefaultFeralTalents(c.in.Level)
		w, err := CatWeights(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		round := func(x float64, d int) float64 { p := math.Pow(10, float64(d)); return math.Round(x*p) / p }
		for _, chk := range []struct {
			what      string
			got, want float64
			dec       int
		}{
			{"DPS", w.DPS, c.dps, 2}, {"Str", w.Str, c.str, 3}, {"Agi", w.Agi, c.agi, 3}, {"Crit", w.Crit, c.crit, 3},
			{"Hit", w.Hit, c.hit, 3}, {"Exp", w.Exp, c.exp, 3}, {"Haste", w.Haste, c.haste, 3}, {"WeaponDPS", w.WeaponDPS, c.wdps, 3},
		} {
			if round(chk.got, chk.dec) != chk.want {
				t.Errorf("%s: %s = %.4f, want %v", c.name, chk.what, chk.got, chk.want)
			}
		}
	}
}

func TestCatBelowTwenty(t *testing.T) {
	if _, err := CatWeights(CatInputs{Level: 19, Talents: DefaultFeralTalents(19)}); err == nil {
		t.Error("level 19 has no Cat Form")
	}
}

package upgrade

import (
	"math"
	"testing"
)

// The spec's §7 vectors (crit 5%, InnCount 1, mix 50/30/20, max ranks, default talents).
func TestRestoVectors(t *testing.T) {
	for _, c := range []struct {
		name                         string
		level                        int
		intel, spi, hp, mp5, T, p    float64
		fsr                          float64
		maxMana, M                   float64
		hpm, wSpi, wMP5, wInt, wCrit float64
	}{
		{"L40 default", 40, 150, 140, 60, 5, 180, 0.7, 0.8, 2824, 7218, 3.392, 2.821, 2.743, 1.346, 3.099},
		{"L40 pressure 0", 40, 150, 140, 60, 5, 180, 0, 0.8, 2824, 7218, 3.392, 1.103, 0.000, 0.089, 3.099},
		{"L40 boss", 40, 150, 140, 60, 5, 300, 1, 0.9, 2824, 8585, 3.392, 3.777, 5.492, 1.599, 3.099},
		{"L32 no Reflection", 32, 120, 112, 48, 5, 180, 0.7, 0.8, 2179, 3032, 3.187, 1.660, 4.608, 2.191, 2.168},
		{"L60", 60, 225, 210, 90, 5, 180, 0.7, 0.8, 4339, 10105, 4.187, 3.927, 4.547, 2.224, 7.633},
	} {
		in := DefaultRestoInputs(c.level, Profile{T: c.T, Pressure: c.p, FSR: c.fsr, Inn: 1})
		in.Int, in.Spirit, in.HP, in.MP5 = c.intel, c.spi, c.hp, c.mp5
		w, err := ComputeResto(in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		round := func(x float64, d int) float64 { p := math.Pow(10, float64(d)); return math.Round(x*p) / p }
		for _, chk := range []struct {
			what      string
			got, want float64
			dec       int
		}{
			{"maxMana", w.MaxMana, c.maxMana, 0}, {"M", w.M, c.M, 0}, {"HPM", w.HPM, c.hpm, 3},
			{"W_Spi", w.Spirit, c.wSpi, 3}, {"W_MP5", w.MP5, c.wMP5, 3}, {"W_Int", w.Int, c.wInt, 3}, {"W_Crit", w.Crit, c.wCrit, 3},
		} {
			if round(chk.got, chk.dec) != chk.want {
				t.Errorf("%s: %s = %.4f, want %v", c.name, chk.what, chk.got, chk.want)
			}
		}
		parts := w.Parts["intCrit"] + w.Parts["intMana"]
		if math.Abs(parts-w.Int) > 1e-9 {
			t.Errorf("%s: Int parts %v don't add up to %v", c.name, parts, w.Int)
		}
	}
}

func TestRestoRejectsBadInput(t *testing.T) {
	in := DefaultRestoInputs(40, RestoProfiles["dungeon"])
	in.Mix = map[Spell]float64{}
	if _, err := ComputeResto(in); err == nil {
		t.Error("an empty mix should fail")
	}
	if _, err := ComputeResto(DefaultRestoInputs(9, RestoProfiles["dungeon"])); err == nil {
		t.Error("level 9 should fail")
	}
}

func TestDefaultTalents(t *testing.T) {
	tl := DefaultTalents(40)
	if tl[HotW] != 5 || tl[Reflection] != 3 || tl[ImpRegrowth] != 3 || tl[GoN] != 5 || tl[Tranq] != 0 || tl[WildGrowth] != 1 {
		t.Fatalf("%v", tl)
	}
}

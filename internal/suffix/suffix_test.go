package suffix

import "testing"

func TestLabel(t *testing.T) {
	for _, c := range []struct{ variant, want string }{
		{"", "Brigade Pauldrons"},
		{"b12751", "Brigade Pauldrons of the Bandit"},
		{"b12718", "Brigade Pauldrons of the Eagle"},
		{"b12725", "Brigade Pauldrons of the Boar"},
		{"b1:12725", "Brigade Pauldrons of the Boar"},
		{"b99", "Brigade Pauldrons [b99]"},
		{"r1101", "Brigade Pauldrons [r1101]"},
	} {
		if got := Label("Brigade Pauldrons", c.variant); got != c.want {
			t.Errorf("Label(%q) = %q, want %q", c.variant, got, c.want)
		}
	}
}

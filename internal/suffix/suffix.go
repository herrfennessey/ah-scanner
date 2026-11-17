// Package suffix names random-suffix gear. The auction house keys a suffix item by its bonus
// list (variant "b12751"); the in-game name ("of the Bandit") comes from the client tables in
// names_data.go. Different bonus lists can share a name with different stat splits, so the
// variant stays the key and the name is only for display.
package suffix

//go:generate go run gen.go -build 1.60.1.70959

import (
	"strconv"
	"strings"
)

// Name returns the suffix for a variant key ("b12751" → "of the Bandit"), or "" when no bonus
// list in it is known to carry one.
func Name(variant string) string {
	for _, part := range strings.Split(variant, ",") {
		ids, ok := strings.CutPrefix(part, "b")
		if !ok {
			continue
		}
		for _, s := range strings.Split(ids, ":") {
			id, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				continue
			}
			if n := bonusName[id]; n != "" {
				return n
			}
		}
	}
	return ""
}

// Label appends the suffix to an item name, or the raw variant in brackets when it has none.
func Label(name, variant string) string {
	if variant == "" {
		return name
	}
	if n := Name(variant); n != "" {
		return name + " " + n
	}
	return name + " [" + variant + "]"
}

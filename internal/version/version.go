// Package version reports which build of ahc is running.
package version

import "runtime/debug"

// Version is set at build time: -ldflags "-X github.com/herrfennessey/ah-scanner/internal/version.Version=v1.2.3".
// Release images carry the semantic-release version, other images `git describe` output
// (e.g. v1.2.3-4-gabc1234).
var Version = ""

// String is the build's version, or for a plain `go build` "dev" plus the commit it was built
// from when Go recorded one (with "-dirty" for uncommitted changes).
func String() string {
	if Version != "" {
		return Version
	}
	v := "dev"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" {
		v += "-" + rev[:min(7, len(rev))]
		if dirty {
			v += "-dirty"
		}
	}
	return v
}

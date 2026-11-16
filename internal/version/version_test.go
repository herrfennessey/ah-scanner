package version

import (
	"strings"
	"testing"
)

func TestStringPrefersBuildVersion(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "v1.2.3"
	if got := String(); got != "v1.2.3" {
		t.Fatalf("String() = %q, want v1.2.3", got)
	}
	Version = ""
	if got := String(); !strings.HasPrefix(got, "dev") {
		t.Fatalf("String() = %q, want dev…", got)
	}
}

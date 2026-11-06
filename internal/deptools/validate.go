package deptools

import (
	"regexp"
	"strings"
)

// Ecosystems pinned_version understands.
const (
	EcoNPM       = "npm"
	EcoPyPI      = "pypi"
	EcoGo        = "go"
	EcoMaven     = "maven"
	EcoTerraform = "terraform"
)

var pinEcosystems = []string{EcoNPM, EcoPyPI, EcoGo, EcoMaven, EcoTerraform}

var (
	// A package or module name in any ecosystem uses only these characters.
	genericName = regexp.MustCompile(`^[A-Za-z0-9@/._:+~-]{1,214}$`)
	npmName     = regexp.MustCompile(`^(@[A-Za-z0-9_~-][A-Za-z0-9._~-]*/)?[A-Za-z0-9_~-][A-Za-z0-9._~-]*$`)
	goModule    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9._~-]+)*$`)
)

// validEcosystem reports whether eco is one pinned_version can read.
func validEcosystem(eco string) bool {
	for _, e := range pinEcosystems {
		if e == eco {
			return true
		}
	}
	return false
}

func hasDotSegment(s string) bool {
	for _, seg := range strings.Split(s, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

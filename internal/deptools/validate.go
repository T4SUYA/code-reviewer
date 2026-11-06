package deptools

import (
	"fmt"
	"regexp"
	"strings"
)

// Ecosystems registry_lookup and pinned_version understand.
const (
	EcoNPM       = "npm"
	EcoPyPI      = "pypi"
	EcoGo        = "go"
	EcoMaven     = "maven"
	EcoTerraform = "terraform"
	EcoOpenTofu  = "opentofu"
	EcoDocker    = "docker"
)

var (
	pinEcosystems      = []string{EcoNPM, EcoPyPI, EcoGo, EcoMaven, EcoTerraform}
	registryEcosystems = []string{EcoNPM, EcoPyPI, EcoGo, EcoMaven, EcoTerraform, EcoOpenTofu, EcoDocker}
)

var (
	// A package or module name in any ecosystem uses only these characters.
	genericName = regexp.MustCompile(`^[A-Za-z0-9@/._:+~-]{1,214}$`)
	versionRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]{0,63}$`)

	npmName  = regexp.MustCompile(`^(@[A-Za-z0-9_~-][A-Za-z0-9._~-]*/)?[A-Za-z0-9_~-][A-Za-z0-9._~-]*$`)
	pypiName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	goModule = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9._~-]+)*$`)
	// A Maven coordinate is group:artifact. The group is dot-separated, each
	// part non-empty, because the registry path turns the dots into slashes.
	mavenCoord = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*:[A-Za-z0-9_-][A-Za-z0-9._-]*$`)
	tfSegment  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	dockerRepo = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
)

// validEcosystem reports whether eco is one pinned_version can read.
func validEcosystem(eco string) bool { return contains(pinEcosystems, eco) }

// validRegistryEcosystem reports whether eco is one registry_lookup can query.
func validRegistryEcosystem(eco string) bool { return contains(registryEcosystems, eco) }

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

// validatePackage checks pkg against the name form of the ecosystem. Every
// component of the coordinate (path segment, Maven group and artifact) must be
// more than dots, and nothing that looks like a credential is accepted.
func validatePackage(eco, pkg string) error {
	if !genericName.MatchString(pkg) {
		return fmt.Errorf("invalid package name: use letters, digits and . _ - / @ : + ~ only, at most 214 characters")
	}
	if hasDotSegment(pkg) {
		return fmt.Errorf("invalid package name: no component of it may be . or ..")
	}
	var ok bool
	switch eco {
	case EcoNPM:
		ok = npmName.MatchString(pkg)
	case EcoPyPI:
		ok = pypiName.MatchString(pkg)
	case EcoGo:
		ok = goModule.MatchString(pkg)
	case EcoMaven:
		ok = mavenCoord.MatchString(pkg)
	case EcoTerraform, EcoOpenTofu:
		parts := strings.Split(pkg, "/")
		ok = len(parts) == 2 || len(parts) == 3
		for _, p := range parts {
			ok = ok && tfSegment.MatchString(p)
		}
	case EcoDocker:
		ok = dockerRepo.MatchString(pkg)
	default:
		return fmt.Errorf("unknown ecosystem %q (use one of %s)", eco, strings.Join(registryEcosystems, ", "))
	}
	if !ok {
		return fmt.Errorf("invalid %s package name %q", eco, pkg)
	}
	if looksLikeSecret(pkg) {
		return errSecretShaped("package")
	}
	return nil
}

func validateVersion(v string) error {
	if !versionRe.MatchString(v) {
		return fmt.Errorf("invalid version: use letters, digits and . _ + ~ - only, at most 64 characters")
	}
	if hasDotSegment(v) {
		return fmt.Errorf("invalid version: no component of it may be . or ..")
	}
	if looksLikeSecret(v) {
		return errSecretShaped("version")
	}
	return nil
}

// hasDotSegment reports whether any component of a coordinate, split at "/"
// and ":", is "." or "..".
func hasDotSegment(s string) bool {
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == ':' }) {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func errSecretShaped(what string) error {
	return fmt.Errorf("invalid %s: it looks like a credential or random token, and nothing that does is sent to a registry or docs service", what)
}

package deptools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/OpticDiff/code-reviewer/internal/textsafe"
)

const (
	maxPinFileBytes = 16 << 20
	maxPinWalkDepth = 5
	maxPinWalkFiles = 20000
	maxPinResults   = 40
	maxPinPathRunes = 200
)

var skipPinDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".terraform": true,
	".venv": true, "venv": true, "dist": true, "build": true, "target": true, ".gradle": true,
}

// pin is one version a repository file fixes or constrains for a package.
type pin struct {
	file    string
	version string
	kind    string // "locked", "declared" or "runtime"
}

func pinnedVersionSpec(opts Options) Spec {
	str := map[string]any{"type": "string"}
	return Spec{
		Name:        ToolPinnedVersion,
		Description: "Read the version of a dependency or runtime that the repository's lockfiles and manifests pin. No network.",
		Usage: `Args: {"package": "<name>", "ecosystem": "<optional: npm|pypi|go|maven|terraform>"}
Package forms: npm "react" or "@scope/name"; PyPI "requests"; Go module path; Maven "group:artifact"; Terraform provider "hashicorp/google".
Runtimes: "node", "python" and "go" read .nvmrc, engines, .python-version, requires-python and the go directive.
Reads package-lock.json, pnpm-lock.yaml, yarn.lock, package.json, go.mod, go.sum, uv.lock, poetry.lock, requirements*.txt, pyproject.toml, pom.xml, gradle.lockfile and .terraform.lock.hcl.`,
		InputSchema: objectSchema(map[string]any{"package": str, "ecosystem": str}, "package"),
		Untrusted:   true,
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pkg, ok := stringArg(args, "package")
			if !ok || pkg == "" {
				return "", fmt.Errorf("missing or invalid 'package' argument")
			}
			eco, _ := stringArg(args, "ecosystem")
			return pinnedVersion(ctx, opts.RepoRoot, pkg, eco)
		},
	}
}

func pinnedVersion(ctx context.Context, root, pkg, eco string) (string, error) {
	if !genericName.MatchString(pkg) {
		return "", fmt.Errorf("invalid package name: use letters, digits and . _ - / @ : + ~ only, at most 214 characters")
	}
	if eco != "" && !validEcosystem(eco) {
		return "", fmt.Errorf("unknown ecosystem %q (use one of %s)", eco, strings.Join(pinEcosystems, ", "))
	}
	files, err := findPinFiles(ctx, root)
	if err != nil {
		return "", err
	}
	var pins []pin
	scanned := 0
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		parser := parserFor(filepath.Base(rel))
		if parser == nil || (eco != "" && eco != parser.ecosystem) {
			continue
		}
		data, err := readCapped(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		scanned++
		for _, p := range parser.parse(data, pkg) {
			p.file = rel
			pins = append(pins, p)
		}
	}
	if len(pins) == 0 {
		return fmt.Sprintf("No pin for %q found in %d lockfile or manifest file(s) of the checkout. "+
			"That does not mean the package is unused: it may be pinned elsewhere or not installed from a lockfile.", pkg, scanned), nil
	}
	sort.SliceStable(pins, func(i, j int) bool { return pins[i].file < pins[j].file })
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pins for %q:\n", pkg)
	for i, p := range pins {
		if i == maxPinResults {
			fmt.Fprintf(&sb, "... and %d more\n", len(pins)-i)
			break
		}
		fmt.Fprintf(&sb, "- %s: %s (%s)\n", textsafe.OneLine(p.file, maxPinPathRunes), sanitizeVersion(p.version), p.kind)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

const (
	maxVersionTokens = 8
	maxVersionRunes  = 64
	toolchainSuffix  = " (toolchain)"
	replacedPrefix   = "replaced by "
)

var (
	// versionToken is one comparator of a version or range: an optional
	// operator, then a number-led version, a wildcard or a dist-tag word.
	versionToken   = regexp.MustCompile(`^(?:[<>=!~^]{1,2})?(?:v|go)?(?:[0-9][0-9A-Za-z.*+_~-]*|[*xX]|latest|next)$`)
	versionOp      = regexp.MustCompile(`^[<>=!~^]{1,2}$`)
	replacementDir = regexp.MustCompile(`^[A-Za-z0-9._/~@+-]{1,100}$`)
	constraintTail = regexp.MustCompile(`^(.+) \(constraint (.+)\)$`)
)

// sanitizeVersion keeps version text that parses as a version or a range:
// comparator tokens separated by spaces, commas, "-" or "||". Anything else,
// prose included, becomes "(unparseable)", so repository content cannot
// smuggle sentences into the tool result. The forms the parsers generate
// themselves, "<version> (toolchain)", "<version> (constraint <range>)" and
// "replaced by <module> <version>", are checked part by part.
func sanitizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if rest, ok := strings.CutSuffix(v, toolchainSuffix); ok {
		if clean := sanitizeVersion(rest); clean != unparseableVersion {
			return clean + toolchainSuffix
		}
		return unparseableVersion
	}
	if m := constraintTail.FindStringSubmatch(v); m != nil {
		locked, constraint := sanitizeVersion(m[1]), sanitizeVersion(m[2])
		if locked == unparseableVersion || constraint == unparseableVersion {
			return unparseableVersion
		}
		return locked + " (constraint " + constraint + ")"
	}
	if rest, ok := strings.CutPrefix(v, replacedPrefix); ok {
		target, ver, found := strings.Cut(rest, " ")
		if !found || !replacementDir.MatchString(target) {
			return unparseableVersion
		}
		if clean := sanitizeVersion(ver); clean != unparseableVersion {
			return replacedPrefix + target + " " + clean
		}
		return unparseableVersion
	}
	if v == "" || len([]rune(v)) > maxVersionRunes {
		return unparseableVersion
	}
	fields := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(v, "||", " || "), ",", " , "))
	if len(fields) > maxVersionTokens {
		return unparseableVersion
	}
	sawVersion := false
	for _, f := range fields {
		switch {
		case f == "||" || f == "," || f == "-" || versionOp.MatchString(f):
		case versionToken.MatchString(f):
			sawVersion = true
		default:
			return unparseableVersion
		}
	}
	if !sawVersion {
		return unparseableVersion
	}
	return v
}

const unparseableVersion = "(unparseable)"

func findPinFiles(ctx context.Context, root string) ([]string, error) {
	var files []string
	visited := 0
	rootDepth := strings.Count(filepath.Clean(root), string(filepath.Separator))
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipPinDirs[d.Name()] {
				return filepath.SkipDir
			}
			if strings.Count(path, string(filepath.Separator))-rootDepth > maxPinWalkDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if visited++; visited > maxPinWalkFiles {
			return filepath.SkipAll
		}
		if !d.Type().IsRegular() || parserFor(d.Name()) == nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, err
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxPinFileBytes {
		return nil, fmt.Errorf("file too large")
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type pinParser struct {
	ecosystem string
	parse     func(data []byte, pkg string) []pin
}

func parserFor(name string) *pinParser {
	switch {
	case name == "package-lock.json":
		return &pinParser{EcoNPM, parsePackageLock}
	case name == "package.json":
		return &pinParser{EcoNPM, parsePackageJSON}
	case name == "pnpm-lock.yaml":
		return &pinParser{EcoNPM, parsePnpmLock}
	case name == "yarn.lock":
		return &pinParser{EcoNPM, parseYarnLock}
	case name == ".nvmrc" || name == ".node-version":
		return &pinParser{EcoNPM, func(d []byte, p string) []pin { return runtimeFile(d, p, "node") }}
	case name == "go.mod":
		return &pinParser{EcoGo, parseGoMod}
	case name == "go.sum":
		return &pinParser{EcoGo, parseGoSum}
	case name == "uv.lock" || name == "poetry.lock":
		return &pinParser{EcoPyPI, parseTOMLPackages}
	case strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt"):
		return &pinParser{EcoPyPI, parseRequirements}
	case name == "pyproject.toml":
		return &pinParser{EcoPyPI, parsePyproject}
	case name == ".python-version":
		return &pinParser{EcoPyPI, func(d []byte, p string) []pin { return runtimeFile(d, p, "python") }}
	case name == "pom.xml":
		return &pinParser{EcoMaven, parsePom}
	case name == "gradle.lockfile":
		return &pinParser{EcoMaven, parseGradleLock}
	case name == ".terraform.lock.hcl":
		return &pinParser{EcoTerraform, parseTerraformLock}
	}
	return nil
}

func runtimeFile(data []byte, pkg, runtime string) []pin {
	if !strings.EqualFold(pkg, runtime) {
		return nil
	}
	v := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if v == "" {
		return nil
	}
	return []pin{{version: v, kind: "runtime"}}
}

// --- npm ---

func parsePackageLock(data []byte, pkg string) []pin {
	var lock struct {
		Packages     map[string]struct{ Version string } `json:"packages"`
		Dependencies map[string]struct{ Version string } `json:"dependencies"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return nil
	}
	var pins []pin
	suffix := "node_modules/" + pkg
	for path, p := range lock.Packages {
		if (path == suffix || strings.HasSuffix(path, "/"+suffix)) && p.Version != "" {
			pins = append(pins, pin{version: p.Version, kind: "locked"})
		}
	}
	if d, ok := lock.Dependencies[pkg]; ok && d.Version != "" {
		pins = append(pins, pin{version: d.Version, kind: "locked"})
	}
	return dedupePins(pins)
}

func parsePackageJSON(data []byte, pkg string) []pin {
	var m struct {
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		PeerDependencies     map[string]string `json:"peerDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
		Engines              map[string]string `json:"engines"`
	}
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	var pins []pin
	for _, group := range []map[string]string{m.Dependencies, m.DevDependencies, m.PeerDependencies, m.OptionalDependencies} {
		if v, ok := group[pkg]; ok {
			pins = append(pins, pin{version: v, kind: "declared"})
		}
	}
	if strings.EqualFold(pkg, "node") {
		if v, ok := m.Engines["node"]; ok {
			pins = append(pins, pin{version: v, kind: "runtime"})
		}
	}
	return pins
}

var pnpmKeyVersion = regexp.MustCompile(`^/?(@?[^@\s(]+)@([^()\s]+)`)

func parsePnpmLock(data []byte, pkg string) []pin {
	var pins []pin
	inPackages := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line != "" && line[0] != ' ' && line[0] != '#' {
			inPackages = line == "packages:"
			continue
		}
		if !inPackages || !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		key := strings.Trim(strings.TrimSuffix(strings.TrimSpace(line), ":"), `'"`)
		if m := pnpmKeyVersion.FindStringSubmatch(key); m != nil && m[1] == pkg {
			pins = append(pins, pin{version: m[2], kind: "locked"})
			continue
		}
		// Lockfile v5 keys: /name/1.2.3 or /@scope/name/1.2.3_peers.
		trimmed := strings.TrimPrefix(key, "/")
		if i := strings.LastIndex(trimmed, "/"); i > 0 && trimmed[:i] == pkg {
			pins = append(pins, pin{version: strings.SplitN(trimmed[i+1:], "_", 2)[0], kind: "locked"})
		}
	}
	return dedupePins(pins)
}

func parseYarnLock(data []byte, pkg string) []pin {
	var pins []pin
	match := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] != ' ' {
			match = false
			for _, spec := range strings.Split(strings.TrimSuffix(line, ":"), ",") {
				spec = strings.Trim(strings.TrimSpace(spec), `"`)
				if len(spec) < 2 {
					continue
				}
				if i := strings.Index(spec[1:], "@"); i >= 0 && spec[:i+1] == pkg {
					match = true
				}
			}
			continue
		}
		if match {
			t := strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(t, "version "); ok { // classic: version "1.2.3"
				pins = append(pins, pin{version: strings.Trim(v, `"`), kind: "locked"})
			} else if v, ok := strings.CutPrefix(t, "version: "); ok { // berry: version: 1.2.3
				pins = append(pins, pin{version: strings.Trim(v, `"`), kind: "locked"})
			}
		}
	}
	return dedupePins(pins)
}

// --- Go ---

func parseGoMod(data []byte, pkg string) []pin {
	var pins []pin
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(stripGoComment(line))
		switch {
		case line == "", line == ")", strings.HasSuffix(line, "("):
		case strings.HasPrefix(line, "go ") && strings.EqualFold(pkg, "go"):
			pins = append(pins, pin{version: strings.TrimSpace(line[3:]), kind: "runtime"})
		case strings.HasPrefix(line, "toolchain ") && strings.EqualFold(pkg, "go"):
			pins = append(pins, pin{version: strings.TrimSpace(line[10:]) + " (toolchain)", kind: "runtime"})
		default:
			fields := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(line, "require "), "replace "))
			if len(fields) >= 2 && fields[0] == pkg && !strings.Contains(line, "=>") {
				pins = append(pins, pin{version: fields[1], kind: "locked"})
			} else if i := indexOf(fields, "=>"); i >= 0 && fields[0] == pkg && len(fields) > i+2 {
				pins = append(pins, pin{version: "replaced by " + fields[i+1] + " " + fields[i+2], kind: "locked"})
			}
		}
	}
	return pins
}

func stripGoComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func parseGoSum(data []byte, pkg string) []pin {
	var pins []pin
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == pkg && !strings.HasSuffix(f[1], "/go.mod") {
			pins = append(pins, pin{version: f[1], kind: "locked"})
		}
	}
	return dedupePins(pins)
}

// --- Python ---

func normalizePyPI(name string) string {
	return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "-"))
}

var tomlStringKV = regexp.MustCompile(`^(\w[\w-]*)\s*=\s*"([^"]*)"`)

// parseTOMLPackages reads the [[package]] tables of uv.lock and poetry.lock.
func parseTOMLPackages(data []byte, pkg string) []pin {
	want := normalizePyPI(pkg)
	var pins []pin
	var name, version string
	flush := func() {
		if name != "" && version != "" && normalizePyPI(name) == want {
			pins = append(pins, pin{version: version, kind: "locked"})
		}
		name, version = "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			if line == "[[package]]" {
				flush()
			} else if !strings.HasPrefix(line, "[package.") && !strings.HasPrefix(line, "[[package.") {
				flush()
				name, version = "", ""
			}
			continue
		}
		if m := tomlStringKV.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "name":
				if name == "" {
					name = m[2]
				}
			case "version":
				if version == "" {
					version = m[2]
				}
			}
		}
	}
	flush()
	return pins
}

var requirementLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(\[[^\]]*\])?\s*(===|==|~=|>=|<=|!=|>|<)\s*([^\s;,#\\]+)`)

func parseRequirements(data []byte, pkg string) []pin {
	want := normalizePyPI(pkg)
	var pins []pin
	for _, line := range strings.Split(string(data), "\n") {
		m := requirementLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || normalizePyPI(m[1]) != want {
			continue
		}
		kind := "declared"
		if m[3] == "==" || m[3] == "===" {
			kind = "locked"
		}
		v := m[4]
		if kind == "declared" {
			v = m[3] + v
		}
		pins = append(pins, pin{version: v, kind: kind})
	}
	return pins
}

var requiresPython = regexp.MustCompile(`(?m)^\s*requires-python\s*=\s*["']([^"']+)["']`)

func parsePyproject(data []byte, pkg string) []pin {
	if !strings.EqualFold(pkg, "python") {
		return nil
	}
	if m := requiresPython.FindSubmatch(data); m != nil {
		return []pin{{version: string(m[1]), kind: "runtime"}}
	}
	return nil
}

// --- Maven and Gradle ---

type pomDependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
}

type pomDoc struct {
	Parent     pomDependency `xml:"parent"`
	Properties struct {
		Items []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	} `xml:"properties"`
	Dependencies      []pomDependency `xml:"dependencies>dependency"`
	ManagedDeps       []pomDependency `xml:"dependencyManagement>dependencies>dependency"`
	Plugins           []pomDependency `xml:"build>plugins>plugin"`
	ManagedPlugins    []pomDependency `xml:"build>pluginManagement>plugins>plugin"`
	ProfileDeps       []pomDependency `xml:"profiles>profile>dependencies>dependency"`
	ProfileManagement []pomDependency `xml:"profiles>profile>dependencyManagement>dependencies>dependency"`
}

func parsePom(data []byte, pkg string) []pin {
	var doc pomDoc
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	if dec.Decode(&doc) != nil {
		return nil
	}
	props := map[string]string{}
	for _, it := range doc.Properties.Items {
		props[it.XMLName.Local] = strings.TrimSpace(it.Value)
	}
	var pins []pin
	add := func(d pomDependency, kind string) {
		if d.GroupID+":"+d.ArtifactID != pkg {
			return
		}
		v := strings.TrimSpace(d.Version)
		if v == "" {
			v = "(managed by a parent or BOM)"
		}
		if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
			if resolved, ok := props[v[2:len(v)-1]]; ok {
				v = resolved
			}
		}
		pins = append(pins, pin{version: v, kind: kind})
	}
	add(doc.Parent, "declared")
	for _, group := range [][]pomDependency{doc.Dependencies, doc.ManagedDeps, doc.Plugins, doc.ManagedPlugins, doc.ProfileDeps, doc.ProfileManagement} {
		for _, d := range group {
			add(d, "declared")
		}
	}
	return dedupePins(pins)
}

func parseGradleLock(data []byte, pkg string) []pin {
	var pins []pin
	for _, line := range strings.Split(string(data), "\n") {
		coords, _, _ := strings.Cut(strings.TrimSpace(line), "=")
		parts := strings.Split(coords, ":")
		if len(parts) == 3 && parts[0]+":"+parts[1] == pkg {
			pins = append(pins, pin{version: parts[2], kind: "locked"})
		}
	}
	return dedupePins(pins)
}

// --- Terraform ---

var (
	tfProviderBlock = regexp.MustCompile(`^provider\s+"([^"]+)"\s*\{`)
	tfAttr          = regexp.MustCompile(`^(version|constraints)\s*=\s*"([^"]*)"`)
)

func parseTerraformLock(data []byte, pkg string) []pin {
	var pins []pin
	current := ""
	var locked, constraint string
	flush := func() {
		if current != "" && (current == pkg || strings.HasSuffix(current, "/"+pkg)) && locked != "" {
			v := locked
			if constraint != "" {
				v += " (constraint " + constraint + ")"
			}
			pins = append(pins, pin{version: v, kind: "locked"})
		}
		current, locked, constraint = "", "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if m := tfProviderBlock.FindStringSubmatch(line); m != nil {
			flush()
			current = m[1]
			continue
		}
		if current == "" {
			continue
		}
		if m := tfAttr.FindStringSubmatch(line); m != nil {
			if m[1] == "version" {
				locked = m[2]
			} else {
				constraint = m[2]
			}
		}
	}
	flush()
	return pins
}

func dedupePins(pins []pin) []pin {
	seen := map[pin]bool{}
	var out []pin
	for _, p := range pins {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

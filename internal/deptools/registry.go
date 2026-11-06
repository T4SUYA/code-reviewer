package deptools

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/OpticDiff/code-reviewer/internal/textsafe"
)

var defaultRegistryURLs = map[string]string{
	EcoNPM:       "https://registry.npmjs.org",
	EcoPyPI:      "https://pypi.org",
	EcoGo:        "https://proxy.golang.org",
	EcoMaven:     "https://repo1.maven.org/maven2",
	EcoTerraform: "https://registry.terraform.io",
	EcoOpenTofu:  "https://registry.opentofu.org",
	EcoDocker:    "https://hub.docker.com",
}

// registryLookup answers, for one ecosystem, whether a package and version
// exist. It sends only the validated ecosystem, package and version, and only
// to the host configured for that ecosystem.
type registryLookup struct {
	opts Options
	// authorization, when set, is sent as the Authorization header (docs_lookup only).
	authorization string
}

// lookupResult is what a registry told us about a package and version.
type lookupResult struct {
	packageExists bool
	versionKnown  bool // a version was asked for
	versionExists bool
	published     string // publish date of the asked-for version, if the registry says
	latest        string
	latestDate    string
	deprecated    string // "no", "yes: <reason>" or "not reported by this registry"
	notes         []string
}

// validateRegistryURLs rejects a mirror URL that is not https, or http on a
// loopback address, or that carries credentials, a query or a fragment.
func validateRegistryURLs(urls map[string]string) error {
	for eco, raw := range urls {
		if !validRegistryEcosystem(eco) {
			return fmt.Errorf("unknown ecosystem %q in registry URLs (use one of %s)", eco, strings.Join(registryEcosystems, ", "))
		}
		if _, err := parseBaseURL(raw); err != nil {
			return fmt.Errorf("registry URL for %s: %w", eco, err)
		}
	}
	return nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid URL %q", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("URL %q must not carry credentials, a query or a fragment", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if ip := net.ParseIP(u.Hostname()); !(u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
			return nil, fmt.Errorf("URL %q must use https (http is allowed only for loopback addresses)", raw)
		}
	default:
		return nil, fmt.Errorf("URL %q must use https", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func registryLookupSpec(opts Options) Spec {
	str := map[string]any{"type": "string"}
	rl := registryLookup{opts: opts}
	return Spec{
		Name:        ToolRegistryLookup,
		Description: "Ask a public package registry whether a package and version exist, when the version was published, what the latest version is and whether it is deprecated.",
		Usage: `Args: {"ecosystem": "npm|pypi|go|maven|terraform|opentofu|docker", "package": "<name>", "version": "<optional version or image tag>"}
Package forms: npm "react" or "@scope/name"; PyPI "requests"; Go module path; Maven "group:artifact"; Terraform/OpenTofu provider "hashicorp/google" or module "namespace/name/provider"; Docker image "node" or "library/node" (version is the tag).
Only the ecosystem, package and version are sent to the registry. A failed lookup means "unknown", never "does not exist".`,
		InputSchema:      objectSchema(map[string]any{"ecosystem": str, "package": str, "version": str}, "ecosystem", "package"),
		Timeout:          opts.RegistryTimeout * 2,
		Untrusted:        true,
		ExternalEvidence: true,
		Inconclusive:     func(out string) bool { return strings.Contains(out, "\n"+packageMissingLine) },
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			eco, _ := stringArg(args, "ecosystem")
			pkg, _ := stringArg(args, "package")
			version, _ := stringArg(args, "version")
			return rl.run(ctx, eco, pkg, version)
		},
	}
}

func (rl registryLookup) run(ctx context.Context, eco, pkg, version string) (string, error) {
	if !validRegistryEcosystem(eco) {
		return "", fmt.Errorf("unknown ecosystem %q (use one of %s)", eco, strings.Join(registryEcosystems, ", "))
	}
	if err := validatePackage(eco, pkg); err != nil {
		return "", err
	}
	if version != "" {
		if err := validateVersion(version); err != nil {
			return "", err
		}
	}
	base, err := rl.baseURL(eco)
	if err != nil {
		return "", err
	}
	slog.Info("agent tool registry_lookup", "ecosystem", eco, "package", pkg, "version", version, "host", base.Host)

	var res lookupResult
	switch eco {
	case EcoNPM:
		res, err = rl.npm(ctx, base, pkg, version)
	case EcoPyPI:
		res, err = rl.pypi(ctx, base, pkg, version)
	case EcoGo:
		res, err = rl.goProxy(ctx, base, pkg, version)
	case EcoMaven:
		res, err = rl.maven(ctx, base, pkg, version)
	case EcoTerraform, EcoOpenTofu:
		res, err = rl.terraform(ctx, base, eco, pkg, version)
	case EcoDocker:
		res, err = rl.docker(ctx, base, pkg, version)
	}
	if err != nil {
		return "", fmt.Errorf("lookup failed (result unknown, not evidence that the package or version does not exist): %w", err)
	}
	return formatLookup(eco, pkg, version, res), nil
}

func (rl registryLookup) baseURL(eco string) (*url.URL, error) {
	raw := defaultRegistryURLs[eco]
	if override := rl.opts.RegistryURLs[eco]; override != "" {
		raw = override
	}
	return parseBaseURL(raw)
}

// packageMissingLine is the line registry_lookup prints when the registry has no such package.
const packageMissingLine = "package exists: no"

// Caps on the registry-supplied strings echoed to the model.
const (
	maxRegistryDateRunes   = 40
	maxRegistryVersionRune = 64
	maxRegistryNoteRunes   = 200
)

// formatLookup renders what a registry said. Every string the registry
// supplied (dates, versions, deprecation texts, notes) passes through the
// one-line sanitizer and a cap; the package and version are the validated
// values the caller sent.
func formatLookup(eco, pkg, version string, r lookupResult) string {
	date := func(s string) string { return textsafe.OneLine(s, maxRegistryDateRunes) }
	var sb strings.Builder
	fmt.Fprintf(&sb, "ecosystem: %s\npackage: %s\n", eco, pkg)
	if !r.packageExists {
		sb.WriteString(packageMissingLine + " (the registry has no such package)\n")
		return strings.TrimRight(sb.String(), "\n")
	}
	sb.WriteString("package exists: yes\n")
	if r.versionKnown {
		if r.versionExists {
			fmt.Fprintf(&sb, "version %s: exists", version)
			if r.published != "" {
				fmt.Fprintf(&sb, ", published %s", date(r.published))
			}
			sb.WriteString("\n")
		} else {
			fmt.Fprintf(&sb, "version %s: not published in this registry\n", version)
		}
	}
	if r.latest != "" {
		fmt.Fprintf(&sb, "latest: %s", textsafe.OneLine(r.latest, maxRegistryVersionRune))
		if r.latestDate != "" {
			fmt.Fprintf(&sb, ", published %s", date(r.latestDate))
		}
		sb.WriteString("\n")
	}
	if r.deprecated != "" {
		fmt.Fprintf(&sb, "deprecated: %s\n", textsafe.OneLine(r.deprecated, maxRegistryNoteRunes))
	}
	for _, n := range r.notes {
		fmt.Fprintf(&sb, "note: %s\n", textsafe.OneLine(n, maxRegistryNoteRunes))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// --- HTTP ---

// do sends one request to base+path and returns the status, headers and a
// size-capped body. Redirects stay on the host of base.
func (rl registryLookup) do(ctx context.Context, base *url.URL, method, path string, accept string) (int, http.Header, []byte, error) {
	target, err := url.Parse(base.String() + path)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "code-reviewer-agent-tools")
	if rl.authorization != "" {
		req.Header.Set("Authorization", rl.authorization)
	}
	client := rl.opts.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	c := *client
	c.Timeout = rl.opts.RegistryTimeout
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !strings.EqualFold(r.URL.Host, base.Host) || r.URL.Scheme != base.Scheme {
			return fmt.Errorf("redirect to %s refused: only %s is allowed", r.URL.Host, base.Host)
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, rl.opts.MaxResponseBytes+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if int64(len(body)) > rl.opts.MaxResponseBytes {
		return 0, nil, nil, fmt.Errorf("response larger than %d bytes", rl.opts.MaxResponseBytes)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// getJSON decodes a JSON response. found is false for 404 and 410; any other
// non-200 status is an error.
func (rl registryLookup) getJSON(ctx context.Context, base *url.URL, path, accept string, into any) (found bool, err error) {
	status, _, body, err := rl.do(ctx, base, http.MethodGet, path, accept)
	if err != nil {
		return false, err
	}
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return false, nil
	case status != http.StatusOK:
		return false, fmt.Errorf("registry answered HTTP %d", status)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return false, fmt.Errorf("unreadable registry response: %w", err)
	}
	return true, nil
}

// --- npm ---

func (rl registryLookup) npm(ctx context.Context, base *url.URL, pkg, version string) (lookupResult, error) {
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Deprecated json.RawMessage `json:"deprecated"`
		} `json:"versions"`
		Time map[string]string `json:"time"`
	}
	found, err := rl.getJSON(ctx, base, "/"+url.PathEscape(pkg), "application/json", &doc)
	if err != nil || !found {
		return lookupResult{}, err
	}
	res := lookupResult{packageExists: true, latest: doc.DistTags["latest"], deprecated: "no"}
	res.latestDate = doc.Time[res.latest]
	target := res.latest
	if version != "" {
		_, res.versionExists = doc.Versions[version]
		res.versionKnown = true
		res.published = doc.Time[version]
		target = version
	}
	if v, ok := doc.Versions[target]; ok {
		res.deprecated = npmDeprecation(v.Deprecated)
	}
	return res, nil
}

func npmDeprecation(raw json.RawMessage) string {
	var msg string
	if json.Unmarshal(raw, &msg) == nil && msg != "" {
		return "yes: " + clip(msg, 200)
	}
	return "no"
}

// --- PyPI ---

func (rl registryLookup) pypi(ctx context.Context, base *url.URL, pkg, version string) (lookupResult, error) {
	var doc struct {
		Info struct {
			Version      string `json:"version"`
			Yanked       bool   `json:"yanked"`
			YankedReason string `json:"yanked_reason"`
		} `json:"info"`
		Releases map[string][]struct {
			UploadTime string `json:"upload_time_iso_8601"`
			Yanked     bool   `json:"yanked"`
			Reason     string `json:"yanked_reason"`
		} `json:"releases"`
	}
	found, err := rl.getJSON(ctx, base, "/pypi/"+url.PathEscape(pkg)+"/json", "application/json", &doc)
	if err != nil || !found {
		return lookupResult{}, err
	}
	res := lookupResult{packageExists: true, latest: doc.Info.Version, deprecated: "no"}
	describe := func(ver string) (date, deprecated string, exists bool) {
		files, ok := doc.Releases[ver]
		if !ok || len(files) == 0 {
			return "", "no", false
		}
		date, deprecated = files[0].UploadTime, "no"
		for _, f := range files {
			if f.UploadTime != "" && f.UploadTime < date {
				date = f.UploadTime
			}
			if f.Yanked {
				deprecated = "yes: yanked" + yankReason(f.Reason)
			}
		}
		return date, deprecated, true
	}
	res.latestDate, res.deprecated, _ = describe(doc.Info.Version)
	if version != "" {
		res.versionKnown = true
		res.published, res.deprecated, res.versionExists = describe(version)
	}
	return res, nil
}

func yankReason(r string) string {
	if r == "" {
		return ""
	}
	return " (" + clip(r, 200) + ")"
}

// --- Go module proxy ---

func (rl registryLookup) goProxy(ctx context.Context, base *url.URL, pkg, version string) (lookupResult, error) {
	var latest struct{ Version, Time string }
	mod := "/" + escapeGoPath(pkg)
	found, err := rl.getJSON(ctx, base, mod+"/@latest", "application/json", &latest)
	if err != nil || !found {
		return lookupResult{}, err
	}
	res := lookupResult{packageExists: true, latest: latest.Version, latestDate: latest.Time,
		deprecated: "not reported by this registry (the proxy does not expose go.mod deprecation notices)"}
	if version != "" {
		res.versionKnown = true
		var info struct{ Version, Time string }
		res.versionExists, err = rl.getJSON(ctx, base, mod+"/@v/"+escapeGoPath(version)+".info", "application/json", &info)
		if err != nil {
			return lookupResult{}, err
		}
		res.published = info.Time
	}
	return res, nil
}

// --- Maven Central ---

func (rl registryLookup) maven(ctx context.Context, base *url.URL, pkg, version string) (lookupResult, error) {
	group, artifact, _ := strings.Cut(pkg, ":")
	dir := "/" + strings.ReplaceAll(url.PathEscape(group), ".", "/") + "/" + url.PathEscape(artifact)
	status, _, body, err := rl.do(ctx, base, http.MethodGet, dir+"/maven-metadata.xml", "application/xml")
	if err != nil {
		return lookupResult{}, err
	}
	if status == http.StatusNotFound {
		return lookupResult{}, nil
	}
	if status != http.StatusOK {
		return lookupResult{}, fmt.Errorf("registry answered HTTP %d", status)
	}
	var meta struct {
		Versioning struct {
			Latest   string   `xml:"latest"`
			Release  string   `xml:"release"`
			Versions []string `xml:"versions>version"`
		} `xml:"versioning"`
	}
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&meta); err != nil {
		return lookupResult{}, fmt.Errorf("unreadable registry response: %w", err)
	}
	res := lookupResult{packageExists: true, latest: firstNonEmpty(meta.Versioning.Release, meta.Versioning.Latest),
		deprecated: "not reported by this registry"}
	if version != "" {
		res.versionKnown = true
		for _, v := range meta.Versioning.Versions {
			if v == version {
				res.versionExists = true
			}
		}
		if res.versionExists {
			res.published = rl.mavenPomDate(ctx, base, dir, artifact, version)
		}
	}
	return res, nil
}

// mavenPomDate reads the Last-Modified header of a version's POM. A failure
// leaves the date blank.
func (rl registryLookup) mavenPomDate(ctx context.Context, base *url.URL, dir, artifact, version string) string {
	status, header, _, err := rl.do(ctx, base, http.MethodHead, dir+"/"+url.PathEscape(version)+"/"+url.PathEscape(artifact)+"-"+url.PathEscape(version)+".pom", "")
	if err != nil || status != http.StatusOK {
		return ""
	}
	return header.Get("Last-Modified")
}

// --- Terraform and OpenTofu registries ---

func (rl registryLookup) terraform(ctx context.Context, base *url.URL, eco, pkg, version string) (lookupResult, error) {
	parts := strings.Split(pkg, "/")
	kind := "providers"
	if len(parts) == 3 {
		kind = "modules"
	}
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = url.PathEscape(p)
	}
	root := "/v1/" + kind + "/" + strings.Join(escaped, "/")

	var list struct {
		Versions []struct{ Version string } `json:"versions"`
		Modules  []struct {
			Versions []struct{ Version string } `json:"versions"`
		} `json:"modules"`
	}
	found, err := rl.getJSON(ctx, base, root+"/versions", "application/json", &list)
	if err != nil || !found {
		return lookupResult{}, err
	}
	var versions []string
	for _, v := range list.Versions {
		versions = append(versions, v.Version)
	}
	for _, m := range list.Modules {
		for _, v := range m.Versions {
			versions = append(versions, v.Version)
		}
	}
	if len(versions) == 0 {
		return lookupResult{}, nil
	}
	res := lookupResult{packageExists: true, latest: latestStable(versions), deprecated: "not reported by this registry"}
	if version != "" {
		res.versionKnown = true
		for _, v := range versions {
			if v == version {
				res.versionExists = true
			}
		}
	}
	if eco == EcoTerraform {
		rl.terraformDetails(ctx, base, root, kind, &res, version)
	}
	return res, nil
}

// terraformDetails fills publish dates and module deprecation from the
// registry's per-version document. The document is a registry extension, so a
// failure leaves the fields blank.
func (rl registryLookup) terraformDetails(ctx context.Context, base *url.URL, root, kind string, res *lookupResult, version string) {
	fetch := func(v string) (published string, deprecation json.RawMessage) {
		var doc struct {
			PublishedAt string          `json:"published_at"`
			Deprecation json.RawMessage `json:"deprecation"`
		}
		if ok, err := rl.getJSON(ctx, base, root+"/"+url.PathEscape(v), "application/json", &doc); err != nil || !ok {
			return "", nil
		}
		return doc.PublishedAt, doc.Deprecation
	}
	res.latestDate, _ = fetch(res.latest)
	target := res.latest
	if res.versionKnown && res.versionExists {
		res.published, _ = fetch(version)
		target = version
	}
	if kind == "modules" {
		_, dep := fetch(target)
		res.deprecated = "no"
		if len(dep) > 0 && string(dep) != "null" {
			res.deprecated = "yes: " + clip(string(dep), 200)
		}
	}
}

// --- Docker Hub ---

func (rl registryLookup) docker(ctx context.Context, base *url.URL, pkg, tag string) (lookupResult, error) {
	if !strings.Contains(pkg, "/") {
		pkg = "library/" + pkg
	}
	ns, repo, _ := strings.Cut(pkg, "/")
	root := "/v2/repositories/" + url.PathEscape(ns) + "/" + url.PathEscape(repo)
	var repoDoc struct{ Name string }
	found, err := rl.getJSON(ctx, base, root, "application/json", &repoDoc)
	if err != nil || !found {
		return lookupResult{}, err
	}
	res := lookupResult{packageExists: true, deprecated: "not reported by this registry",
		notes: []string{"Docker Hub offers no cheap latest-tag query; compare tags yourself if needed"}}
	if tag != "" {
		res.versionKnown = true
		var tagDoc struct {
			LastUpdated string `json:"last_updated"`
			TagStatus   string `json:"tag_status"`
		}
		res.versionExists, err = rl.getJSON(ctx, base, root+"/tags/"+url.PathEscape(tag), "application/json", &tagDoc)
		if err != nil {
			return lookupResult{}, err
		}
		res.published = tagDoc.LastUpdated
		if res.versionExists && tagDoc.TagStatus != "" {
			res.notes = append(res.notes, "tag status: "+clip(tagDoc.TagStatus, 40))
		}
	}
	return res, nil
}

// --- helpers ---

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// clip collapses whitespace and cuts s to at most n runes. Escaping happens
// once, when the result is formatted for the model.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

var numericPart = regexp.MustCompile(`^\d+`)

// latestStable returns the highest version by numeric dotted comparison,
// preferring versions without a pre-release suffix.
func latestStable(versions []string) string {
	var stable []string
	for _, v := range versions {
		if !strings.ContainsAny(v, "-+") {
			stable = append(stable, v)
		}
	}
	if len(stable) == 0 {
		stable = versions
	}
	sorted := append([]string(nil), stable...)
	sort.SliceStable(sorted, func(i, j int) bool { return compareVersions(sorted[i], sorted[j]) < 0 })
	return sorted[len(sorted)-1]
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(numericPart.FindString(pa[i]))
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(numericPart.FindString(pb[i]))
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

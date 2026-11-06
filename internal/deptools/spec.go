// Package deptools implements the dependency-grounding tools of the agent
// loop. Each tool answers a factual question about a dependency, so a finding
// that claims an API, option or version does not exist can be checked against
// evidence instead of memory.
//
// pinned_version and read_dependency_source read the checkout and never use
// the network. registry_lookup does, and sends only values that passed the
// checks in validate.go and secret.go: short identifiers, never repository
// code, diff text or paths.
package deptools

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Tool names a user can list under agent_tools.
const (
	ToolPinnedVersion        = "pinned_version"
	ToolRegistryLookup       = "registry_lookup"
	ToolReadDependencySource = "read_dependency_source"
	defaultRegistryTimeout   = 8 * time.Second
	defaultMaxResponseBytes  = 8 << 20
)

// AllNames lists every tool name Build accepts.
var AllNames = []string{ToolPinnedVersion, ToolRegistryLookup, ToolReadDependencySource}

// Spec describes one tool in a form independent of the agent loop's types.
type Spec struct {
	Name        string
	Description string
	Usage       string
	InputSchema map[string]any
	Timeout     time.Duration
	// Untrusted marks output that comes from outside the reviewer: text the
	// repository under review or a dependency controls.
	Untrusted bool
	Execute   func(ctx context.Context, args map[string]any) (string, error)
}

// Options configures the tools. The zero value of every field selects its default.
type Options struct {
	// RepoRoot is the checkout the local tools read.
	RepoRoot string
	// RegistryURLs overrides the base URL per ecosystem (npm, pypi, go, maven,
	// terraform, opentofu, docker), for mirrors. Requests for an ecosystem only
	// go to the host of its base URL.
	RegistryURLs map[string]string
	// RegistryTimeout bounds one registry or docs request (default 8s).
	RegistryTimeout time.Duration
	// MaxResponseBytes caps how much of a registry response is read (default 8 MiB).
	MaxResponseBytes int64
	// GoModCache is the Go module cache directory. Empty asks the environment
	// (GOMODCACHE, then `go env GOMODCACHE`).
	GoModCache string
	// HTTPClient replaces the default client; it must not follow redirects
	// across hosts on its own accord (tests only).
	HTTPClient *http.Client
}

// ValidName reports whether name is a tool Build knows.
func ValidName(name string) bool {
	for _, n := range AllNames {
		if n == name {
			return true
		}
	}
	return false
}

// Build returns the specs for the named tools, in the given order.
func Build(names []string, opts Options) ([]Spec, error) {
	opts = opts.withDefaults()
	if err := validateRegistryURLs(opts.RegistryURLs); err != nil {
		return nil, err
	}
	var specs []Spec
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			return nil, fmt.Errorf("agent tool %q listed twice", name)
		}
		seen[name] = true
		switch name {
		case ToolPinnedVersion:
			specs = append(specs, pinnedVersionSpec(opts))
		case ToolRegistryLookup:
			specs = append(specs, registryLookupSpec(opts))
		case ToolReadDependencySource:
			specs = append(specs, readDependencySourceSpec(opts))
		default:
			return nil, fmt.Errorf("unknown agent tool %q (known: %v)", name, AllNames)
		}
	}
	return specs, nil
}

func (o Options) withDefaults() Options {
	if o.RegistryTimeout <= 0 {
		o.RegistryTimeout = defaultRegistryTimeout
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = defaultMaxResponseBytes
	}
	return o
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		schema["required"] = req
	}
	return schema
}

func stringArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key].(string)
	return v, ok
}

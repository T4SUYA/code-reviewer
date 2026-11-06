// Package deptools implements the dependency-grounding tools of the agent
// loop. Each tool answers a factual question about a dependency, so a finding
// that claims an API, option or version does not exist can be checked against
// evidence instead of memory.
//
// The tools read the checkout and never use the network.
package deptools

import (
	"context"
	"fmt"
	"time"
)

// Tool names a user can list under agent_tools.
const (
	ToolPinnedVersion        = "pinned_version"
	ToolReadDependencySource = "read_dependency_source"
)

// AllNames lists every tool name Build accepts.
var AllNames = []string{ToolPinnedVersion, ToolReadDependencySource}

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
	// RepoRoot is the checkout the tools read.
	RepoRoot string
	// GoModCache is the Go module cache directory. Empty asks the environment
	// (GOMODCACHE, then `go env GOMODCACHE`).
	GoModCache string
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
		case ToolReadDependencySource:
			specs = append(specs, readDependencySourceSpec(opts))
		default:
			return nil, fmt.Errorf("unknown agent tool %q (known: %v)", name, AllNames)
		}
	}
	return specs, nil
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

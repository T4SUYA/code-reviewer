package config

import (
	"fmt"

	"github.com/OpticDiff/code-reviewer/internal/deptools"
)

// AgentToolOptions tunes the dependency tools named in agent_tools. It is read
// only from the operator config: it names filesystem locations the tools read.
// Every field is optional; the zero value selects the defaults.
type AgentToolOptions struct {
	GoModCache string // Go module cache directory for read_dependency_source.
}

// rawAgentToolOptions is the YAML form of AgentToolOptions.
type rawAgentToolOptions struct {
	GoModCache string `yaml:"go_mod_cache"`
}

func (r rawAgentToolOptions) parse() (AgentToolOptions, error) {
	return AgentToolOptions{GoModCache: r.GoModCache}, nil
}

// DependencyToolOptions converts the options for the dependency tools.
func (o AgentToolOptions) DependencyToolOptions() deptools.Options {
	return deptools.Options{GoModCache: o.GoModCache}
}

// validateAgentTools rejects an unknown or repeated tool name.
func validateAgentTools(names []string, o AgentToolOptions) error {
	if _, err := deptools.Build(names, o.DependencyToolOptions()); err != nil {
		return &ConfigError{Err: fmt.Errorf("agent_tools: %w", err)}
	}
	return nil
}

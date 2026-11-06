package config

import (
	"fmt"
	"time"

	"github.com/OpticDiff/code-reviewer/internal/deptools"
)

// AgentToolOptions tunes the dependency tools named in agent_tools. It is read
// only from the operator config: it names URLs the tools call, an environment
// variable whose value they send and filesystem locations they read, none of
// which the repository under review may choose. Every field is optional; the
// zero value selects the defaults.
type AgentToolOptions struct {
	RegistryURLs       map[string]string // Base URL per ecosystem, for mirrors.
	RegistryTimeout    time.Duration     // Timeout of one registry or docs request.
	MaxResponseBytes   int64             // Cap on how much of a registry response is read.
	DocsURL            string            // Context7 base URL.
	DocsAPIKeyEnv      string            // Environment variable holding the Context7 API key; sent only to the default docs host.
	DocsMaxOutputBytes int               // Cap on the documentation text returned.
	GoModCache         string            // Go module cache directory for read_dependency_source.
}

// rawAgentToolOptions is the YAML form of AgentToolOptions.
type rawAgentToolOptions struct {
	RegistryURLs       map[string]string `yaml:"registry_urls"`
	RegistryTimeout    string            `yaml:"registry_timeout"`
	MaxResponseBytes   int64             `yaml:"max_response_bytes"`
	DocsURL            string            `yaml:"docs_url"`
	DocsAPIKeyEnv      string            `yaml:"docs_api_key_env"`
	DocsMaxOutputBytes int               `yaml:"docs_max_output_bytes"`
	GoModCache         string            `yaml:"go_mod_cache"`
}

func (r rawAgentToolOptions) parse() (AgentToolOptions, error) {
	o := AgentToolOptions{
		RegistryURLs: r.RegistryURLs, MaxResponseBytes: r.MaxResponseBytes, DocsURL: r.DocsURL,
		DocsAPIKeyEnv: r.DocsAPIKeyEnv, DocsMaxOutputBytes: r.DocsMaxOutputBytes, GoModCache: r.GoModCache,
	}
	if r.RegistryTimeout != "" {
		d, err := time.ParseDuration(r.RegistryTimeout)
		if err != nil {
			return o, &ConfigError{Err: fmt.Errorf("invalid registry_timeout for agent_tool_options: %w", err)}
		}
		o.RegistryTimeout = d
	}
	return o, nil
}

// DependencyToolOptions converts the options for the dependency tools.
func (o AgentToolOptions) DependencyToolOptions() deptools.Options {
	return deptools.Options{
		RegistryURLs: o.RegistryURLs, RegistryTimeout: o.RegistryTimeout, MaxResponseBytes: o.MaxResponseBytes,
		DocsURL: o.DocsURL, DocsAPIKeyEnv: o.DocsAPIKeyEnv, DocsMaxOutputBytes: o.DocsMaxOutputBytes, GoModCache: o.GoModCache,
	}
}

// validateAgentTools rejects an unknown or repeated tool name, a negative
// limit, and a mirror or docs URL the tools would refuse to call.
func validateAgentTools(names []string, o AgentToolOptions) error {
	switch {
	case o.RegistryTimeout < 0:
		return &ConfigError{Err: fmt.Errorf("agent_tool_options: registry_timeout must not be negative")}
	case o.MaxResponseBytes < 0:
		return &ConfigError{Err: fmt.Errorf("agent_tool_options: max_response_bytes must not be negative")}
	case o.DocsMaxOutputBytes < 0:
		return &ConfigError{Err: fmt.Errorf("agent_tool_options: docs_max_output_bytes must not be negative")}
	}
	if _, err := deptools.Build(names, o.DependencyToolOptions()); err != nil {
		return &ConfigError{Err: fmt.Errorf("agent_tools: %w", err)}
	}
	return nil
}

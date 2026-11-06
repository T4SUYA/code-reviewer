package config

import (
	"fmt"
	"regexp"
	"time"
)

// AgentMCPServer is an MCP server started over stdio whose allowlisted tools
// the agent loop may call as "<name>.<tool>".
type AgentMCPServer struct {
	Name           string
	Command        string
	Args           []string
	EnvAllowlist   []string      // Parent environment variables passed to the server (PATH is always passed).
	ToolsAllowlist []string      // Server tools the agent may call; all others stay hidden.
	CallTimeout    time.Duration // Per-call timeout (0 = default 30s).
	MaxOutputBytes int           // Per-result size cap in bytes (0 = default 16 KiB).
}

// rawAgentMCPServer is the YAML form of AgentMCPServer.
type rawAgentMCPServer struct {
	Name           string   `yaml:"name"`
	Command        string   `yaml:"command"`
	Args           []string `yaml:"args"`
	EnvAllowlist   []string `yaml:"env_allowlist"`
	ToolsAllowlist []string `yaml:"tools_allowlist"`
	CallTimeout    string   `yaml:"call_timeout"`
	MaxOutputBytes int      `yaml:"max_output_bytes"`
}

func (r rawAgentMCPServer) parse() (AgentMCPServer, error) {
	s := AgentMCPServer{
		Name: r.Name, Command: r.Command, Args: r.Args,
		EnvAllowlist: r.EnvAllowlist, ToolsAllowlist: r.ToolsAllowlist,
		MaxOutputBytes: r.MaxOutputBytes,
	}
	if r.CallTimeout != "" {
		d, err := time.ParseDuration(r.CallTimeout)
		if err != nil {
			return s, fmt.Errorf("invalid call_timeout for agent_mcp_servers %q: %w", r.Name, err)
		}
		s.CallTimeout = d
	}
	return s, nil
}

var mcpServerName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// validateAgentMCPServers rejects servers the agent loop could not register
// safely: a name that cannot prefix a tool name, a duplicate name, a missing
// command, or a non-positive limit.
func validateAgentMCPServers(servers []AgentMCPServer) error {
	seen := map[string]bool{}
	for _, s := range servers {
		switch {
		case !mcpServerName.MatchString(s.Name):
			return &ConfigError{Err: fmt.Errorf("invalid agent_mcp_servers name %q: use letters, digits, '-' and '_', starting with a letter", s.Name)}
		case seen[s.Name]:
			return &ConfigError{Err: fmt.Errorf("duplicate agent_mcp_servers name %q", s.Name)}
		case s.Command == "":
			return &ConfigError{Err: fmt.Errorf("agent_mcp_servers %q has no command", s.Name)}
		case s.CallTimeout < 0:
			return &ConfigError{Err: fmt.Errorf("agent_mcp_servers %q: call_timeout must not be negative", s.Name)}
		case s.MaxOutputBytes < 0:
			return &ConfigError{Err: fmt.Errorf("agent_mcp_servers %q: max_output_bytes must not be negative", s.Name)}
		}
		seen[s.Name] = true
	}
	return nil
}

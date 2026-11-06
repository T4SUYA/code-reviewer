package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// operatorConfigEnv names the environment variable that points at the
// operator config file; the --operator-config flag overrides it.
const operatorConfigEnv = "CODE_REVIEWER_OPERATOR_CONFIG"

// operatorOnlyKeys are the settings that start processes or carry secrets and
// URLs. .code-reviewer.yaml lives in the repository under review, and in CI
// that is the merge request head, so its author must not control them. They
// are read only from the operator config.
var operatorOnlyKeys = []string{"agent_mcp_servers", "agent_tool_options"}

// operatorConfig is the YAML form of the operator config file.
type operatorConfig struct {
	AgentMCPServers  []rawAgentMCPServer  `yaml:"agent_mcp_servers"`
	AgentToolOptions *rawAgentToolOptions `yaml:"agent_tool_options"`
}

// warnOperatorOnlyKeys logs each operator-only key found in the repository
// config. The key is not applied.
func warnOperatorOnlyKeys(raw map[string]interface{}) {
	for _, key := range operatorOnlyKeys {
		if _, ok := raw[key]; ok {
			slog.Warn("ignoring operator-only setting in .code-reviewer.yaml: set it in the operator config (--operator-config, "+operatorConfigEnv+" or the user config file)", "key", key)
		}
	}
}

// userConfigPath is the per-user operator config:
// $XDG_CONFIG_HOME/code-reviewer/config.yaml, or ~/.config/code-reviewer/config.yaml.
func userConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "code-reviewer", "config.yaml")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "code-reviewer", "config.yaml")
	}
	return ""
}

// loadOperatorConfig applies the operator-trusted settings from the user
// config file and from the file named by --operator-config or the environment.
// An explicit file inside the checkout is refused in CI mode: the checkout is
// the head of the change under review.
func (c *Config) loadOperatorConfig() error {
	if path := userConfigPath(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := c.applyOperatorConfig(path, data); err != nil {
				return err
			}
		}
	}

	path := c.OperatorConfig
	if path == "" {
		path = os.Getenv(operatorConfigEnv)
	}
	if path == "" {
		return c.validateOperatorSettings()
	}
	if c.CIMode && insideCheckout(path) {
		slog.Warn("ignoring operator config inside the checkout: in CI the checkout is the change under review; keep the file outside it", "path", path)
		return c.validateOperatorSettings()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading operator config %q: %w", path, err)
	}
	if err := c.applyOperatorConfig(path, data); err != nil {
		return err
	}
	return c.validateOperatorSettings()
}

// validateOperatorSettings checks the settings the operator config contributes.
func (c *Config) validateOperatorSettings() error {
	if err := validateAgentMCPServers(c.AgentMCPServers); err != nil {
		return err
	}
	return validateAgentTools(c.AgentTools, c.AgentToolOptions)
}

func (c *Config) applyOperatorConfig(path string, data []byte) error {
	var oc operatorConfig
	if err := yaml.Unmarshal(data, &oc); err != nil {
		return fmt.Errorf("parsing operator config %q: %w", path, err)
	}
	for _, raw := range oc.AgentMCPServers {
		server, err := raw.parse()
		if err != nil {
			return err
		}
		c.AgentMCPServers = append(c.AgentMCPServers, server)
	}
	if oc.AgentToolOptions != nil {
		opts, err := oc.AgentToolOptions.parse()
		if err != nil {
			return err
		}
		c.AgentToolOptions = opts
	}
	return nil
}

// insideCheckout reports whether path lies within the repository checkout:
// the nearest ancestor of the working directory holding .git, or the working
// directory itself when there is none.
func insideCheckout(path string) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return true
	}
	root := cwd
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			root = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

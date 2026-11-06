// Package mcp connects the agent loop to Model Context Protocol servers over
// stdio and exposes their allowlisted tools as plain Go values.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// DefaultCallTimeout bounds one tool call when ServerConfig.CallTimeout is unset.
	DefaultCallTimeout = 30 * time.Second
	// DefaultMaxOutputBytes caps one tool result when ServerConfig.MaxOutputBytes is unset.
	DefaultMaxOutputBytes = 16 * 1024
	// connectTimeout bounds process start, the initialize handshake and tool discovery.
	connectTimeout = 30 * time.Second
)

// ServerConfig describes one MCP server started as a child process.
type ServerConfig struct {
	Name           string        // Prefix of the registered tool names: "<Name>.<tool>".
	Command        string        // Executable to start.
	Args           []string      // Arguments passed to Command.
	EnvAllowlist   []string      // Names of the parent environment variables the child receives (PATH and HOME are always passed).
	ToolsAllowlist []string      // Server tool names the agent may use; every other tool is hidden and refused.
	CallTimeout    time.Duration // Per-call timeout (default DefaultCallTimeout).
	MaxOutputBytes int           // Per-result size cap in bytes (default DefaultMaxOutputBytes).
}

// ToolInfo describes one server tool the agent may call.
type ToolInfo struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Client is a connected MCP server restricted to its allowlisted tools.
type Client struct {
	cfg     ServerConfig
	cmd     *exec.Cmd
	session *sdk.ClientSession
	allowed map[string]bool
	tools   []ToolInfo
}

// alwaysPassedEnv are the variables every server receives: PATH to find its
// own tools and HOME, which package runners such as npx need for their cache
// and configuration.
var alwaysPassedEnv = []string{"PATH", "HOME"}

// Env returns the child process environment: PATH, HOME and the allowlisted
// variables that are set in the parent, and nothing else.
func Env(allowlist []string, lookup func(string) (string, bool)) []string {
	names := append(append([]string{}, alwaysPassedEnv...), allowlist...)
	seen := map[string]bool{}
	var env []string
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if v, ok := lookup(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// Connect starts the server, completes the MCP handshake and discovers its
// tools, keeping only the allowlisted ones.
func Connect(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if cfg.Name == "" || cfg.Command == "" {
		return nil, errors.New("mcp server needs a name and a command")
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = DefaultCallTimeout
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = DefaultMaxOutputBytes
	}

	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	cmd := exec.Command(cfg.Command, cfg.Args...) //nolint:gosec // command comes from the user's own config
	cmd.Env = Env(cfg.EnvAllowlist, os.LookupEnv)
	startInOwnProcessGroup(cmd)

	client := sdk.NewClient(&sdk.Implementation{Name: "code-reviewer"}, nil)
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to mcp server %q: %w", cfg.Name, err)
	}

	c := &Client{cfg: cfg, cmd: cmd, session: session, allowed: map[string]bool{}}
	for _, name := range cfg.ToolsAllowlist {
		c.allowed[name] = true
	}
	if err := c.discover(ctx); err != nil {
		_ = session.Close()
		killProcessGroup(cmd)
		return nil, err
	}
	return c, nil
}

func (c *Client) discover(ctx context.Context) error {
	for tool, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("listing tools of mcp server %q: %w", c.cfg.Name, err)
		}
		if !c.allowed[tool.Name] {
			continue
		}
		schema, _ := toMap(tool.InputSchema)
		c.tools = append(c.tools, ToolInfo{Name: tool.Name, Description: tool.Description, InputSchema: schema})
	}
	return nil
}

// Tools returns the allowlisted tools the server offers.
func (c *Client) Tools() []ToolInfo { return c.tools }

// Call runs an allowlisted tool and returns its text output, truncated to the
// configured size cap. A tool the allowlist does not name is refused without
// contacting the server.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	if !c.allowed[tool] {
		return "", fmt.Errorf("tool %q is not allowed on mcp server %q", tool, c.cfg.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CallTimeout)
	defer cancel()

	res, err := c.session.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("mcp tool %s.%s timed out after %s", c.cfg.Name, tool, c.cfg.CallTimeout)
		}
		return "", fmt.Errorf("mcp tool %s.%s: %w", c.cfg.Name, tool, err)
	}

	var sb strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(text.Text)
		}
	}
	out := truncate(sb.String(), c.cfg.MaxOutputBytes)
	if res.IsError {
		return "", errors.New(out)
	}
	return out, nil
}

// Close ends the session and stops the server process together with every
// process it started: a launcher such as npx leaves the real server running
// as a grandchild unless the whole process group is killed.
func (c *Client) Close() error {
	err := c.session.Close()
	killProcessGroup(c.cmd)
	return err
}

// truncate cuts s to at most max bytes on a UTF-8 boundary and marks the cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[... truncated at %d bytes ...]", max)
}

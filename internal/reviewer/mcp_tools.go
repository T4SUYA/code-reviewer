package reviewer

import (
	"context"
	"log/slog"

	"github.com/OpticDiff/code-reviewer/internal/mcp"
	"github.com/OpticDiff/code-reviewer/internal/textsafe"
)

// maxToolDescriptionRunes caps a tool description supplied by an MCP server.
const maxToolDescriptionRunes = 300

// registerMCPTools starts each configured MCP server and registers its
// allowlisted tools as "<server>.<tool>". A server that fails to start, or a
// tool whose name is already taken, is skipped with a warning so the loop
// still runs with the remaining tools. The returned function stops every
// server that started.
func registerMCPTools(ctx context.Context, reg *ToolRegistry, servers []mcp.ServerConfig) (stop func()) {
	var clients []*mcp.Client
	for _, cfg := range servers {
		client, err := mcp.Connect(ctx, cfg)
		if err != nil {
			slog.Warn("agent loop: mcp server unavailable, continuing without it", "server", cfg.Name, "error", err)
			continue
		}
		clients = append(clients, client)
		for _, info := range client.Tools() {
			name := info.Name
			err := reg.Register(Tool{
				Name:            cfg.Name + "." + name,
				Description:     textsafe.OneLine(info.Description, maxToolDescriptionRunes),
				InputSchema:     schemaOrEmptyObject(info.InputSchema),
				Timeout:         cfg.CallTimeout,
				UntrustedOutput: true,
				Execute: func(ctx context.Context, args map[string]any) (string, error) {
					return client.Call(ctx, name, args)
				},
			})
			if err != nil {
				slog.Warn("agent loop: mcp tool skipped", "server", cfg.Name, "tool", name, "error", err)
			}
		}
	}
	return func() {
		for _, c := range clients {
			_ = c.Close() //nolint:errcheck // the process is being stopped; nothing to recover
		}
	}
}

func schemaOrEmptyObject(schema map[string]any) map[string]any {
	if schema == nil {
		return ObjectSchema(map[string]any{})
	}
	return schema
}

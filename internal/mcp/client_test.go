package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const fakeServerEnv = "CODE_REVIEWER_FAKE_MCP_SERVER"

// TestMain turns the test binary into a small stdio MCP server when the
// fake-server variable is set, so the client is tested against a real child
// process speaking the protocol.
func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}
	os.Exit(m.Run())
}

type textArgs struct {
	Text string `json:"text" jsonschema:"text to echo"`
}

func textResult(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

func runFakeServer() {
	server := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "echo", Description: "Echo the text."},
		func(_ context.Context, _ *sdk.CallToolRequest, in textArgs) (*sdk.CallToolResult, any, error) {
			return textResult("echo: " + in.Text), nil, nil
		})
	sdk.AddTool(server, &sdk.Tool{Name: "secret", Description: "Not for the agent."},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return textResult("secret"), nil, nil
		})
	sdk.AddTool(server, &sdk.Tool{Name: "sleep", Description: "Never answers in time."},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			select {
			case <-time.After(30 * time.Second):
			case <-ctx.Done():
			}
			return textResult("late"), nil, nil
		})
	sdk.AddTool(server, &sdk.Tool{Name: "big", Description: "Returns a lot of text."},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return textResult(strings.Repeat("é", 5000)), nil, nil
		})
	sdk.AddTool(server, &sdk.Tool{Name: "fail", Description: "Reports a tool error."},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			res := textResult("it broke")
			res.IsError = true
			return res, nil, nil
		})
	sdk.AddTool(server, &sdk.Tool{Name: "env", Description: "Reports the child environment."},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			var names []string
			for _, kv := range os.Environ() {
				name, _, _ := strings.Cut(kv, "=")
				if strings.HasPrefix(name, "FAKE_MCP_") {
					names = append(names, name)
				}
			}
			return textResult(strings.Join(names, ",")), nil, nil
		})
	if err := server.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}

func fakeConfig(t *testing.T, allow ...string) ServerConfig {
	t.Helper()
	t.Setenv(fakeServerEnv, "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Name: "fake", Command: exe, EnvAllowlist: []string{fakeServerEnv}, ToolsAllowlist: allow}
}

func connect(t *testing.T, cfg ServerConfig) *Client {
	t.Helper()
	c, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClient_DiscoversOnlyAllowlistedTools(t *testing.T) {
	c := connect(t, fakeConfig(t, "echo", "missing"))
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("Tools() = %+v, want only echo", tools)
	}
	if tools[0].Description != "Echo the text." {
		t.Errorf("description = %q", tools[0].Description)
	}
	props, _ := tools[0].InputSchema["properties"].(map[string]any)
	if _, ok := props["text"]; !ok {
		t.Errorf("input schema lacks the text property: %+v", tools[0].InputSchema)
	}
}

func TestClient_NoAllowlistExposesNothing(t *testing.T) {
	c := connect(t, fakeConfig(t))
	if len(c.Tools()) != 0 {
		t.Errorf("Tools() = %+v, want none", c.Tools())
	}
}

func TestClient_CallReturnsText(t *testing.T) {
	c := connect(t, fakeConfig(t, "echo"))
	out, err := c.Call(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil || out != "echo: hi" {
		t.Errorf("Call = %q, %v", out, err)
	}
}

func TestClient_RefusesToolOutsideAllowlist(t *testing.T) {
	c := connect(t, fakeConfig(t, "echo"))
	if out, err := c.Call(context.Background(), "secret", nil); err == nil {
		t.Errorf("Call(secret) = %q, want an error although the server offers it", out)
	}
}

func TestClient_CallTimeout(t *testing.T) {
	cfg := fakeConfig(t, "sleep")
	cfg.CallTimeout = 200 * time.Millisecond
	c := connect(t, cfg)
	start := time.Now()
	_, err := c.Call(context.Background(), "sleep", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("call took %s, the timeout did not apply", time.Since(start))
	}
}

func TestClient_OutputIsCappedOnRuneBoundary(t *testing.T) {
	cfg := fakeConfig(t, "big")
	cfg.MaxOutputBytes = 101 // odd: lands inside a two-byte rune
	c := connect(t, cfg)
	out, err := c.Call(context.Background(), "big", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, marker, _ := strings.Cut(out, "\n[...")
	if len(body) != 100 || !strings.Contains(marker, "truncated at 101 bytes") {
		t.Errorf("len(body) = %d, marker %q", len(body), marker)
	}
	if !strings.HasPrefix(body, "éé") || strings.ContainsRune(body, '�') {
		t.Errorf("body is not valid text: %q", body)
	}
}

func TestClient_ToolErrorBecomesError(t *testing.T) {
	c := connect(t, fakeConfig(t, "fail"))
	if _, err := c.Call(context.Background(), "fail", nil); err == nil || err.Error() != "it broke" {
		t.Errorf("err = %v, want the tool's own message", err)
	}
}

func TestClient_ChildGetsOnlyAllowlistedEnvironment(t *testing.T) {
	t.Setenv("FAKE_MCP_PASSED", "1")
	t.Setenv("FAKE_MCP_WITHHELD", "1")
	cfg := fakeConfig(t, "env")
	cfg.EnvAllowlist = append(cfg.EnvAllowlist, "FAKE_MCP_PASSED")
	c := connect(t, cfg)
	out, err := c.Call(context.Background(), "env", nil)
	if err != nil || out != "FAKE_MCP_PASSED" {
		t.Errorf("child saw %q (err %v), want only FAKE_MCP_PASSED", out, err)
	}
}

func TestConnect_FailsWhenCommandCannotStart(t *testing.T) {
	_, err := Connect(context.Background(), ServerConfig{Name: "x", Command: "/nonexistent/mcp-server"})
	if err == nil {
		t.Error("Connect succeeded for a missing command")
	}
}

func TestEnv(t *testing.T) {
	lookup := func(name string) (string, bool) {
		v, ok := map[string]string{"PATH": "/bin", "A": "1", "B": "2"}[name]
		return v, ok
	}
	got := strings.Join(Env([]string{"A", "A", "MISSING"}, lookup), " ")
	if got != "PATH=/bin A=1" {
		t.Errorf("Env = %q", got)
	}
}

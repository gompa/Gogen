package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gogen/internal/config"
)

// TestMCPServerHelperProcess is the re-exec'd helper that fakes an MCP
// stdio server for TestServer: it answers the initialize handshake and
// tools/list over stdin/stdout JSON-RPC lines, then exits.
func TestMCPServerHelperProcess(t *testing.T) {
	if os.Getenv("GOGEN_MCP_TEST_HELPER") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.Method == "notifications/initialized" {
			continue // notification: no reply
		}
		var resp map[string]any
		switch req.Method {
		case "initialize":
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
				},
			}
		case "tools/list":
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "tool-a", "description": "Tool A", "inputSchema": map[string]any{"type": "object"}},
						{"name": "tool-b", "description": "Tool B", "inputSchema": map[string]any{"type": "object"}},
					},
				},
			}
		default:
			continue
		}
		b, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		fmt.Fprintln(os.Stdout, string(b))
	}
	os.Exit(0)
}

func helperCommand() config.MCPServerConfig {
	return config.MCPServerConfig{
		Name:    "fake",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPServerHelperProcess"},
	}
}

// TestServerProbeSuccess spawns the fake server and asserts the probe
// returns the exposed tools with their descriptions.
func TestServerProbeSuccess(t *testing.T) {
	t.Setenv("GOGEN_MCP_TEST_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := TestServer(ctx, helperCommand())
	if err != nil {
		t.Fatalf("TestServer: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "tool-a" || tools[0].Description != "Tool A" || tools[1].Name != "tool-b" {
		t.Fatalf("tools = %+v, want tool-a/tool-b", tools)
	}
}

// TestServerProbeSpawnFailure verifies a missing binary surfaces the spawn
// phase (not a generic error).
func TestServerProbeSpawnFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := TestServer(ctx, config.MCPServerConfig{Name: "nope", Command: "gogen-mcp-no-such-binary-xyz"})
	if err == nil || !strings.Contains(err.Error(), "spawn failed") {
		t.Fatalf("err = %v, want spawn failure", err)
	}
}

// TestServerProbeHandshakeFailure verifies a process that does not speak
// MCP surfaces the initialize phase (bounded by the caller's context).
func TestServerProbeHandshakeFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	_, err := TestServer(ctx, config.MCPServerConfig{Name: "echo", Command: "echo"})
	if err == nil || !strings.Contains(err.Error(), "initialize failed") {
		t.Fatalf("err = %v, want initialize failure", err)
	}
}

// TestServerProbeRequiresCommand pins the command validation.
func TestServerProbeRequiresCommand(t *testing.T) {
	if _, err := TestServer(context.Background(), config.MCPServerConfig{Name: "x"}); err == nil {
		t.Fatal("expected error for missing command")
	}
}

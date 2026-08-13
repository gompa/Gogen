package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gogen/internal/config"
	"gogen/internal/llm"
)

// TestMCPTestViaWS drives test_mcp through a real WebSocket with an
// injected probe stub: the raw form passes the typed command/args/env
// through, the by-name form resolves the STORED server config from the
// runtime overlay (env values never leave the server), and failures land in
// the reply.
func TestMCPTestViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	s.ws.SetRuntimeConfig(func() config.Config {
		r := s.ws.GetRuntimeConfig()
		r.MCPServers = []config.MCPServerConfig{
			{Name: "fetch", Command: "npx", Args: []string{"-y", "server-fetch"}, Env: map[string]string{"TOKEN": "secret"}},
		}
		return r
	}())
	var got config.MCPServerConfig
	s.mcpTestFn = func(_ context.Context, server config.MCPServerConfig) ([]llm.Tool, error) {
		got = server
		if server.Command == "broken" {
			return nil, fmt.Errorf("spawn failed: boom")
		}
		return []llm.Tool{
			{Name: "get-url", Description: "Fetch a URL"},
			{Name: "fetch", Description: "Fetch tool"},
		}, nil
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	// The handshake config push advertises the configured MCP server list
	// WITHOUT env values.
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && len(m.MCPServers) > 0 })
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "fetch" || cfg.MCPServers[0].Command != "npx" || !cfg.MCPServers[0].EnvSet {
		t.Fatalf("mcpServers push = %+v, want fetch row", cfg.MCPServers)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })

	send := func(req MCPTestRequest) {
		t.Helper()
		if err := conn.WriteJSON(WSMessage{Type: "test_mcp", MCPTest: &req}); err != nil {
			t.Fatalf("send test_mcp: %v", err)
		}
	}

	// Raw form: the typed command/args/env reach the probe.
	send(MCPTestRequest{Command: "node", Args: []string{"server.js"}, Env: map[string]string{"K": "V"}})
	res := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "mcp_test" })
	if res.MCPTestResult == nil || !res.MCPTestResult.OK {
		t.Fatalf("mcp_test reply = %+v, want ok", res.MCPTestResult)
	}
	if len(res.MCPTestResult.Tools) != 2 || res.MCPTestResult.Tools[0].Name != "get-url" {
		t.Fatalf("tools = %+v, want [get-url, fetch]", res.MCPTestResult.Tools)
	}
	if got.Command != "node" || len(got.Args) != 1 || got.Args[0] != "server.js" || got.Env["K"] != "V" {
		t.Fatalf("probe received %+v, want the typed server", got)
	}

	// By-name form: the stored config (with its env) is resolved server-side.
	send(MCPTestRequest{Name: "fetch"})
	res = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "mcp_test" })
	if res.MCPTestResult == nil || !res.MCPTestResult.OK {
		t.Fatalf("by-name mcp_test reply = %+v, want ok", res.MCPTestResult)
	}
	if got.Name != "fetch" || got.Command != "npx" || got.Args[0] != "-y" || got.Env["TOKEN"] != "secret" {
		t.Fatalf("by-name probe received %+v, want the stored fetch server", got)
	}

	// Probe failure lands in the reply.
	send(MCPTestRequest{Command: "broken"})
	res = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "mcp_test" })
	if res.MCPTestResult == nil || res.MCPTestResult.OK || !strings.Contains(res.MCPTestResult.Error, "spawn failed") {
		t.Fatalf("broken mcp_test reply = %+v, want spawn error", res.MCPTestResult)
	}

	// Unknown registered name → provider-style error notice.
	send(MCPTestRequest{Name: "nope"})
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "mcp" || resp.Success {
		t.Fatalf("unknown-name notice = %+v, want mcp error", resp)
	}

	// Missing command → error notice.
	send(MCPTestRequest{Command: "  "})
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "mcp" || resp.Success {
		t.Fatalf("missing-command notice = %+v, want mcp error", resp)
	}
}

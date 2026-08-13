package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gogen/internal/config"
)

// mcpTestTimeout bounds a test_mcp probe (spawn + handshake + tools/list).
const mcpTestTimeout = 20 * time.Second

// wsHandleTestMCP runs the MCP connectivity + tools test against a
// THROWAWAY stdio process (test_mcp) — never registered, never wired to the
// shared registry. The reply is an mcp_test message carrying
// ok/latency/tools/error.
func wsHandleTestMCP(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	s.handleTestMCP(ws, msg)
}

func (s *Server) handleTestMCP(ws *wsConn, msg WSMessage) {
	req := msg.MCPTest
	if req == nil {
		writeNoticeError(ws, "mcp", "Error: missing MCP test request")
		return
	}
	server := config.MCPServerConfig{Name: req.Name, Command: req.Command, Args: req.Args, Env: req.Env}
	// Testing a registered server by name resolves its STORED command/args/
	// env from the runtime overlay (the client only sees envSet, never the
	// env values).
	if req.Name != "" && req.Command == "" {
		r := s.ws.GetRuntimeConfig()
		found := false
		for _, m := range r.MCPServers {
			if m.Name == req.Name {
				server = m
				found = true
				break
			}
		}
		if !found {
			writeNoticeError(ws, "mcp", fmt.Sprintf("Error: MCP server %q is not configured", req.Name))
			return
		}
	}
	if strings.TrimSpace(server.Command) == "" {
		writeNoticeError(ws, "mcp", "Error: MCP server command is required")
		return
	}
	go func() {
		start := time.Now()
		reply := func(res MCPTestResult) {
			_ = ws.writeJSON(WSMessage{Type: "mcp_test", MCPTestResult: &res})
		}
		ctx, cancel := context.WithTimeout(context.Background(), mcpTestTimeout)
		defer cancel()
		tools, err := s.mcpTestFn(ctx, server)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			reply(MCPTestResult{Error: err.Error(), LatencyMs: latency})
			return
		}
		out := make([]MCPTestTool, 0, len(tools))
		for _, t := range tools {
			out = append(out, MCPTestTool{Name: t.Name, Description: t.Description})
		}
		reply(MCPTestResult{OK: true, LatencyMs: latency, Tools: out})
	}()
}

// mcpEntries projects the configured MCP server list for the client: name,
// command, args, and envSet — env values (which can hold secrets) are never
// pushed.
func (s *Server) mcpEntries() []MCPEntry {
	r := s.ws.GetRuntimeConfig()
	if len(r.MCPServers) == 0 {
		return nil
	}
	out := make([]MCPEntry, 0, len(r.MCPServers))
	for _, m := range r.MCPServers {
		out = append(out, MCPEntry{
			Name:    m.Name,
			Command: m.Command,
			Args:    append([]string(nil), m.Args...),
			EnvSet:  len(m.Env) > 0,
		})
	}
	return out
}

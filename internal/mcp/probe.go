package mcp

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/config"
	"gogen/internal/llm"
)

// TestServer probes one MCP stdio server: it spawns the process, performs
// the initialize handshake, lists the exposed tools, and closes it. Nothing
// is registered — the web settings modal uses it to test a server before or
// without saving it (mirrors the provider test). Bounded by ctx; failures
// carry the failing phase so the UI can distinguish a missing binary from a
// broken handshake.
func TestServer(ctx context.Context, server config.MCPServerConfig) ([]llm.Tool, error) {
	if strings.TrimSpace(server.Command) == "" {
		return nil, fmt.Errorf("mcp server requires a command")
	}
	c, err := startClient(server)
	if err != nil {
		return nil, fmt.Errorf("spawn failed: %w", err)
	}
	defer c.Close()
	if err := c.initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize failed: %w", err)
	}
	tools, err := c.listTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("tools/list failed: %w", err)
	}
	return tools, nil
}

package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/mcp"
)

// mcpHandle owns the asynchronous MCP manager initialization. mgr is written
// by the init goroutine and must not be read until done is closed; closeMCP
// enforces that (bounded by a 3s timeout), preserving the original main.go
// shutdown handshake. Manager Close is bounded by a 2s timeout.
type mcpHandle struct {
	mgr  *mcp.Manager
	done chan struct{}
}

// startMCP launches MCP server initialization in the background and returns
// a handle whose mgr is valid once done is closed (nil when MCP is disabled
// or no valid servers are configured).
func startMCP(a *agent.Agent, cfg *config.Config) *mcpHandle {
	h := &mcpHandle{done: make(chan struct{})}
	go func() {
		defer close(h.done)
		servers := mcp.ValidServers(cfg.MCPServers)
		if !cfg.MCPEnabled() {
			if len(cfg.MCPServers) > 0 {
				fmt.Fprintf(os.Stderr, "MCP servers configured but mcp is off; set mcp: on or GOGEN_MCP=on to enable\n")
			}
			return
		}
		if len(servers) == 0 {
			// mcp: on with no usable servers — do not start a manager.
			if len(cfg.MCPServers) > 0 {
				fmt.Fprintf(os.Stderr, "MCP enabled but no valid mcp_servers entries (need name + command)\n")
			}
			return
		}
		var mcpErr error
		h.mgr, mcpErr = mcp.NewManager(servers)
		if mcpErr != nil {
			fmt.Fprintf(os.Stderr, "MCP init error: %v\n", mcpErr)
		} else if reg := h.mgr.Registry(); reg != nil {
			a.SetMCPRegistry(reg)
			fmt.Fprintf(os.Stderr, "MCP tools: %d\n", len(reg.ToolNames()))
		}
	}()
	return h
}

// closeMCP waits for MCP init to finish (bounded), then closes the manager
// (bounded). Safe to call when init never completed or no manager exists.
func closeMCP(h *mcpHandle) {
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		log.Printf("mcp shutdown: timed out waiting for init")
	}
	if h.mgr == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = h.mgr.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("mcp shutdown: timed out closing manager")
	}
}

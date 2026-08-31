package main

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/mcp"
)

// Bounded waits for the MCP shutdown handshake. Package-level vars so tests
// can shrink them; production values preserve the original main.go behavior.
var (
	mcpInitWait  = 3 * time.Second
	mcpCloseWait = 2 * time.Second
)

// newMCPManager is the manager constructor used by startMCP (test hook).
var newMCPManager = mcp.NewManager

// mcpHandle owns the asynchronous MCP manager initialization. mgr is guarded
// by mu: the init goroutine may still be writing it when closeMCP gives up
// waiting (init can legitimately outlive the bounded wait, since per-server
// init timeouts in internal/mcp exceed it), so every read and write of mgr
// goes through the lock. The timeout does NOT establish happens-before; the
// mutex does. Manager Close is bounded by mcpCloseWait.
type mcpHandle struct {
	mu   sync.Mutex // guards mgr
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
		m, mcpErr := newMCPManager(servers)
		if mcpErr != nil {
			fmt.Fprintf(os.Stderr, "MCP init error: %v\n", mcpErr)
			return
		}
		h.mu.Lock()
		h.mgr = m
		h.mu.Unlock()
		if reg := m.Registry(); reg != nil {
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
	case <-time.After(mcpInitWait):
		log.Printf("mcp shutdown: timed out waiting for init")
	}
	h.mu.Lock()
	mgr := h.mgr
	h.mu.Unlock()
	if mgr == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = mgr.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(mcpCloseWait):
		log.Printf("mcp shutdown: timed out closing manager")
	}
}

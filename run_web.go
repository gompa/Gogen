package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/server"
)

// runWeb runs the web server until the context is cancelled or Start fails.
// It owns the server's session-sweep defer (ShutdownSessions); the
// agent-level defers (a.Close, a.FlushPending, MCP shutdown) stay in main so
// the shutdown order is unchanged. MCP init itself is started by startMCP in
// main before the mode branches.
func runWeb(ctx context.Context, a *agent.Agent, cfg *config.Config, restoredModel string) error {
	// Determine the listen address first so we can check for loopback
	// and auto-generate a token before creating the server.
	addr := cfg.WebBind
	var isLoopback bool
	if addr == "" {
		addr = "127.0.0.1:8081"
		isLoopback = true
	} else {
		if !strings.Contains(addr, ":") {
			addr += ":8081"
		}
		isLoopback = server.IsLoopbackBind(addr)
	}

	// For non-loopback binds, auto-generate a token if none is provided.
	if !isLoopback && cfg.WebAuthToken == "" {
		token, err := generateToken()
		if err != nil {
			log.Fatalf("failed to generate auth token: %v", err)
		}
		cfg.WebAuthToken = token
	}

	s := server.NewServer(a, cfg)
	// Flush every registered session agent on shutdown. Both this
	// sweep and the outer defer use the
	// non-forcing FlushPending: the sweep must persist unsaved state,
	// but a forced write on a clean session re-stamps its Updated
	// timestamp with ~now in registry order, which reshuffled the
	// saved-session list on every restart and demoted the session that
	// was active at shutdown. The outer defer still covers the default
	// session idempotently and the TUI/CLI paths (flushAndQuit already
	// forces the TUI write; a dirty session is still written here).
	defer s.ShutdownSessions()

	// Build a user-friendly URL for the startup message.
	// Replace 0.0.0.0 with 127.0.0.1 so the link works when clicked.
	displayAddr := addr
	if strings.HasPrefix(displayAddr, "0.0.0.0:") {
		displayAddr = "127.0.0.1:" + displayAddr[len("0.0.0.0:"):]
	}
	if cfg.WebAuthToken != "" {
		fmt.Printf("Open http://%s?token=%s\n", displayAddr, cfg.WebAuthToken)
	} else {
		fmt.Printf("Open http://%s\n", displayAddr)
	}
	// Listen first so the UI can connect immediately. Provider model
	// validation and context-limit lookup continue in the background.
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, addr)
	}()
	go func() {
		a.ValidateRestoredModel(context.Background(), restoredModel)
		cfg.OpenAIModel = a.Provider.ModelName()
	}()
	if err := <-errCh; err != nil {
		log.Printf("web server error: %v", err)
	}
	return nil
}

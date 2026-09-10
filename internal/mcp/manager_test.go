package mcp

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"gogen/internal/llm"

	"gogen/internal/config"
)

// TestReadLoopRejectsOverlongLine pins the bounded line read: a server that
// emits an unterminated line longer than mcpMaxLineBytes must fail the
// connection's pending calls (instead of the reader allocating without
// bound). The cap is shrunk so the test stays fast.
func TestReadLoopRejectsOverlongLine(t *testing.T) {
	old := mcpMaxLineBytes
	mcpMaxLineBytes = 64
	defer func() { mcpMaxLineBytes = old }()

	ch := make(chan jsonRPCResponse, 1)
	c := &Client{
		stdout:     io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), 4096))),
		pending:    map[int64]chan jsonRPCResponse{1: ch},
		readerDone: make(chan struct{}),
	}
	go c.readLoop()

	select {
	case resp := <-ch:
		if resp.Error == nil {
			t.Fatalf("resp = %+v, want an error", resp)
		}
		if !strings.Contains(resp.Error.Message, "token too long") {
			t.Fatalf("error = %q, want an overlong-line error", resp.Error.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not fail pending on an overlong line")
	}
	select {
	case <-c.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not exit after an overlong line")
	}
}

// TestWithCallTimeoutHonorsCallerDeadline pins the fix for the fixed 30s cap
// on every tools/call: a caller that supplies its own (possibly longer)
// deadline must keep it, so a legitimately long-running MCP tool is not
// reported as failed after mcpCallTimeout. A caller with no deadline still
// gets the default bound.
func TestWithCallTimeoutHonorsCallerDeadline(t *testing.T) {
	t.Run("no caller deadline applies default", func(t *testing.T) {
		ctx, cancel := withCallTimeout(context.Background())
		defer cancel()
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected a default deadline")
		}
		if remaining := time.Until(dl); remaining <= 0 || remaining > mcpCallTimeout {
			t.Fatalf("default deadline %s out of range (want <= %s)", remaining, mcpCallTimeout)
		}
	})

	t.Run("longer caller deadline is honored", func(t *testing.T) {
		want := time.Now().Add(10 * time.Minute)
		caller, callerCancel := context.WithDeadline(context.Background(), want)
		defer callerCancel()

		ctx, cancel := withCallTimeout(caller)
		defer cancel()
		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected a deadline")
		}
		if !got.Equal(want) {
			t.Fatalf("deadline = %s, want the caller's %s (not capped at %s)", got, want, mcpCallTimeout)
		}
	})

	t.Run("shorter caller deadline is preserved", func(t *testing.T) {
		want := time.Now().Add(time.Second)
		caller, callerCancel := context.WithDeadline(context.Background(), want)
		defer callerCancel()

		ctx, cancel := withCallTimeout(caller)
		defer cancel()
		got, _ := ctx.Deadline()
		if !got.Equal(want) {
			t.Fatalf("deadline = %s, want %s", got, want)
		}
	})
}

func TestBytesTrimSpace(t *testing.T) {
	got := string(bytes.TrimSpace([]byte("  hello  \n")))
	if got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestExternalToolName(t *testing.T) {
	got := ExternalToolName("Fetch Server", "get-url")
	if got != "mcp_fetch_server_get_url" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitize(t *testing.T) {
	if sanitize("") != "x" {
		t.Fatal("empty should become x")
	}
}

func TestValidServersDropsIncomplete(t *testing.T) {
	in := []config.MCPServerConfig{
		{Name: "", Command: "npx"},
		{Name: "ok", Command: ""},
		{Name: "fetch", Command: "npx"},
	}
	got := ValidServers(in)
	if len(got) != 1 || got[0].Name != "fetch" {
		t.Fatalf("got %#v", got)
	}
	if ValidServers(nil) != nil {
		t.Fatal("nil in → nil out")
	}
}

func TestRegistryDefinitionsNil(t *testing.T) {
	var r *Registry
	if defs := r.Definitions(); defs != nil {
		t.Fatalf("expected nil, got %v", defs)
	}
}

func TestRegistryDefinitionsEmpty(t *testing.T) {
	r := &Registry{tools: make(map[string]toolEntry)}
	if defs := r.Definitions(); defs != nil {
		t.Fatalf("expected nil for empty registry, got %v", defs)
	}
}

func TestRegistryDefinitionsPopulated(t *testing.T) {
	r := &Registry{tools: map[string]toolEntry{
		"mcp_srv_fetch": {
			server: "srv",
			tool:   "fetch",
			schema: llm.Tool{Name: "fetch", Description: "Fetch a URL", Parameters: map[string]any{"type": "object"}},
		},
		"mcp_srv_search": {
			server: "srv",
			tool:   "search",
			schema: llm.Tool{Name: "search", Description: "Search the web", Parameters: map[string]any{"type": "object"}},
		},
	}}
	defs := r.Definitions()
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	// Must be sorted by name
	if defs[0].Name != "mcp_srv_fetch" || defs[1].Name != "mcp_srv_search" {
		t.Fatalf("expected sorted names, got %q and %q", defs[0].Name, defs[1].Name)
	}
}

func TestRegistryToolNamesNil(t *testing.T) {
	var r *Registry
	if names := r.ToolNames(); names != nil {
		t.Fatalf("expected nil, got %v", names)
	}
}

func TestRegistryToolNamesPopulated(t *testing.T) {
	r := &Registry{tools: map[string]toolEntry{
		"mcp_srv_fetch": {
			server: "srv",
			tool:   "fetch",
			schema: llm.Tool{Name: "fetch"},
		},
	}}
	names := r.ToolNames()
	if len(names) != 1 {
		t.Fatalf("expected 1 name, got %d", len(names))
	}
	if _, ok := names["mcp_srv_fetch"]; !ok {
		t.Fatalf("expected 'mcp_srv_fetch' in names")
	}
}

func TestRegistryCallToolNil(t *testing.T) {
	var r *Registry
	_, err := r.CallTool(context.Background(), "any", nil)
	if err == nil || err.Error() != "mcp registry not configured" {
		t.Fatalf("expected 'mcp registry not configured', got %v", err)
	}
}

func TestRegistryCallToolUnknown(t *testing.T) {
	r := &Registry{tools: make(map[string]toolEntry)}
	_, err := r.CallTool(context.Background(), "unknown_tool", nil)
	if err == nil || err.Error() != "unknown mcp tool: unknown_tool" {
		t.Fatalf("expected 'unknown mcp tool: unknown_tool', got %v", err)
	}
}

func TestNewManagerWithNoServers(t *testing.T) {
	m, err := NewManager(nil)
	if err != nil {
		t.Fatalf("NewManager(nil) should not error, got %v", err)
	}
	if m == nil {
		t.Fatal("NewManager(nil) returned nil")
	}
	if m.Registry() == nil {
		t.Fatal("Registry() should not be nil")
	}
	defs := m.Registry().Definitions()
	if defs != nil {
		t.Fatalf("expected no tools for nil servers, got %v", defs)
	}
}

// TestRegistryAddCollisionKeepsFirst pins the duplicate-name policy: two
// different (server, tool) pairs can sanitize to the same LLM-visible name
// (server "my-server" + tool "list_files" vs server "my" +
// tool "server_list_files"), and a bare map write would silently overwrite
// the earlier entry — the tool would vanish from the model's toolset with no
// trace. The FIRST registration must win; the loser is logged and skipped,
// and later non-colliding tools still register normally.
func TestRegistryAddCollisionKeepsFirst(t *testing.T) {
	if ExternalToolName("my-server", "list_files") != ExternalToolName("my", "server_list_files") {
		t.Fatal("test setup: expected the two (server, tool) pairs to collide")
	}
	r := &Registry{tools: make(map[string]toolEntry)}
	r.add("my-server", "list_files", llm.Tool{Name: "mcp_my_server_list_files", Description: "first"}, nil)
	r.add("my", "server_list_files", llm.Tool{Name: "mcp_my_server_list_files", Description: "second"}, nil)

	got, ok := r.tools["mcp_my_server_list_files"]
	if !ok {
		t.Fatal("colliding tool missing from the registry")
	}
	if got.server != "my-server" || got.tool != "list_files" {
		t.Fatalf("kept entry = server %q tool %q, want the FIRST registration (my-server/list_files)", got.server, got.tool)
	}
	if got.schema.Description != "first" {
		t.Fatalf("schema description = %q, want the first tool's schema", got.schema.Description)
	}

	// A non-colliding tool on the same registry still registers.
	r.add("other", "ping", llm.Tool{Name: "mcp_other_ping"}, nil)
	if entry, ok := r.tools["mcp_other_ping"]; !ok || entry.tool != "ping" {
		t.Fatalf("non-colliding tool missing or wrong: %+v", entry)
	}
}

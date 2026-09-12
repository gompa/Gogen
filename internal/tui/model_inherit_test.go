package tui

import (
	"testing"

	"gogen/internal/agent"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/server"
)

// TestOpenNewLiveSessionInheritsFocusedModel pins the TUI new-session model
// inheritance (web createNewSession parity): a live session spawned via the
// sidebar "n" / /open must adopt the FOCUSED session's current model — not
// the startup-era workspace default seed the per-session provider factory
// falls back to. Pre-fix every new live session started on whatever model
// the workspace was constructed with, so the user's last /models pick was
// lost.
func TestOpenNewLiveSessionInheritsFocusedModel(t *testing.T) {
	m := newSidebarFullModel(t)
	m.workspace = server.NewWorkspaceForHost(m.agent, nil)
	// Simulate the startup-era workspace default seed (captured once from
	// the root agent's model at NewWorkspaceForHost time).
	m.workspace.SetDefaultModel("m0")

	// The focused session has since switched to m1 (/models) and raised its
	// reasoning effort. Per-session state: the workspace default must not
	// have moved.
	if err := m.agent.Provider.SetModel("m1"); err != nil {
		t.Fatalf("focused SetModel: %v", err)
	}
	m.agent.SetThinkingLevel(agent.ThinkingHigh)

	// Production-shape factory: each new session gets a FRESH provider
	// seeded from the workspace default (workspace.go's OpenAIProvider
	// branch), collected here for isolation assertions.
	var created []llm.LLMProvider
	m.workspace.ProviderFactory = func() llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Models = []llm.ModelInfo{{ID: "m0", ContextLimit: 128000}, {ID: "m1", ContextLimit: 128000}}
		_ = p.SetModel(m.workspace.DefaultModel())
		created = append(created, p)
		return p
	}

	m.openNewLiveSession("newsess")
	if len(created) != 1 {
		t.Fatalf("factory created %d providers, want 1", len(created))
	}
	got := m.lives.Active().agent.CurrentModel()
	if got != "m1" {
		t.Fatalf("new session model = %q, want the focused model m1", got)
	}
	if _, lvl := m.lives.Active().agent.ModeAndThinkingLevel(); lvl != agent.ThinkingHigh {
		t.Fatalf("new session thinking level = %q, want the focused level high", lvl)
	}
	// Per-session isolation: the new session's provider is a fresh factory
	// instance, not the focused session's provider.
	if m.lives.Active().agent.Provider == m.lives.sessions[0].agent.Provider {
		t.Fatal("new session shares the focused session's provider")
	}
	// The workspace default stays the startup seed (set_model is per-session).
	if got := m.workspace.DefaultModel(); got != "m0" {
		t.Fatalf("workspace default = %q, want m0 (must stay untouched)", got)
	}
}

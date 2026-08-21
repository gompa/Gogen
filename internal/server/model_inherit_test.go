package server

import (
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// perSessionModelFactory returns a ProviderFactory that hands every session
// a FRESH mock provider seeded with the workspace default model — the same
// shape as the production factory (workspace.go's OpenAIProvider branch:
// per-session provider, seeded from DefaultModel). created collects the
// instances so tests can assert per-session isolation.
func perSessionModelFactory(s *Server, created *[]llm.LLMProvider) func() llm.LLMProvider {
	return func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.Models = []llm.ModelInfo{
			{ID: "m0", ContextLimit: 128000, Current: true},
			{ID: "m1", ContextLimit: 128000},
		}
		_ = p.SetModel(s.ws.DefaultModel())
		if created != nil {
			*created = append(*created, p)
		}
		return p
	}
}

// newModelServer builds a server whose default session runs mock model m0
// (the workspace default) and whose sessions get per-session mock providers.
func newModelServer(t *testing.T) (*Server, *llm.MockProvider, *[]llm.LLMProvider) {
	t.Helper()
	dir := t.TempDir()
	exec := agent.NewExecutor(dir)
	prov := llm.NewMockProvider()
	prov.Model = "m0"
	prov.Models = []llm.ModelInfo{
		{ID: "m0", ContextLimit: 128000, Current: true},
		{ID: "m1", ContextLimit: 128000},
	}
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(prov, exec, ctxMgr)
	store := session.NewStore(true)
	a.SessionStore = store
	s := NewServer(a, &config.Config{})
	var created []llm.LLMProvider
	s.ws.ProviderFactory = perSessionModelFactory(s, &created)
	return s, prov, &created
}

// TestSessionNewInheritsPaneModel pins the /new model inheritance: a new
// session created from a pane that switched models must adopt the pane's
// model — not the startup-era workspace default — while the default itself
// stays untouched (D1, set_model is per-session). The adopted model is
// confirmed in the background via the same ValidateRestoredModel contract
// as resume/fork: the new session's clients receive a second config once
// the model is verified against the session's own provider. This is the
// "default new session stays locked on the first session's model"
// regression.
func TestSessionNewInheritsPaneModel(t *testing.T) {
	s, _, created := newModelServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()

	// Drain the attach handshake and identify the default session A.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sidA := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sidA })

	// The pane switches to m1; the workspace default (m0) must not move.
	if err := conn.WriteJSON(WSMessage{Type: "set_model", Model: "m1", SessionID: sidA}); err != nil {
		t.Fatalf("send set_model: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sidA && m.Model == "m1" })
	if got := s.ws.DefaultModel(); got != "m0" {
		t.Fatalf("workspace default after set_model = %q, want m0 (set_model is per-session)", got)
	}

	// /new from the m1 pane: the new session must adopt m1, not the default.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	// Collect the post-/new frame sequence until it holds response,
	// clear_chat, history, and TWO configs for the new session (the initial
	// adopt-config plus the async validation push). Collecting the whole
	// sequence — rather than single-frame readUntil calls — is essential:
	// the two configs are identical and their order relative to history is
	// NOT guaranteed, so a readUntil(history) can discard a config that a
	// later assertion needs (the config is queued before history under load).
	// The async validation push depends on background goroutine scheduling,
	// so the deadline is generous for loaded -race runs.
	msgs := readMessagesUntil(t, conn, 15*time.Second, func(ms []WSMessage) bool {
		hasResponse, hasClear, hasHistory := false, false, false
		newConfigs := 0
		for _, m := range ms {
			switch m.Type {
			case "response":
				hasResponse = true
			case "clear_chat":
				hasClear = true
			case "history":
				hasHistory = true
			case "config":
				// Only the new session carries m1 here (the pane's own m1
				// config was already drained above); sidA is excluded to be
				// safe. Two such configs = initial adopt + async push.
				if m.SessionID != sidA && m.Model == "m1" {
					newConfigs++
				}
			}
		}
		return hasResponse && hasClear && hasHistory && newConfigs >= 2
	})
	var sidB string
	for _, m := range msgs {
		if m.Type == "config" && m.SessionID != sidA && m.Model == "m1" {
			sidB = m.SessionID
			break
		}
	}
	if sidB == "" {
		t.Fatal("no config for the new session (model m1)")
	}

	rt, ok := s.registry.get(sidB)
	if !ok {
		t.Fatal("new session not registered")
	}
	if got := rt.agent.CurrentModel(); got != "m1" {
		t.Fatalf("new session provider model = %q, want m1", got)
	}
	if got := s.ws.DefaultModel(); got != "m0" {
		t.Fatalf("workspace default after session_new = %q, want m0 (must stay untouched)", got)
	}
	// Per-session isolation: B's provider is a fresh factory instance, not
	// A's provider (or a shared one).
	if len(*created) != 1 {
		t.Fatalf("factory created %d providers for one session_new, want 1", len(*created))
	}
	rtA, _ := s.registry.get(sidA)
	if (*created)[0] == rtA.agent.Provider {
		t.Fatal("new session shares the pane's provider (per-session isolation broken)")
	}
	if rt.agent.OnModelChanged == nil {
		t.Fatal("new session has no OnModelChanged hook (async validation not wired)")
	}
}

// TestSessionNewNoAdoptionWhenPaneMatchesDefault pins the no-adoption
// guard: a /new from a pane that still runs the workspace default seeds the
// fresh provider from that default (verified by construction) — no
// AdoptModel, no unverified mark, and no async validation, so no
// OnModelChanged hook is installed.
func TestSessionNewNoAdoptionWhenPaneMatchesDefault(t *testing.T) {
	s, _, created := newModelServer(t)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()

	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sidA := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sidA })

	// The pane never switched models: pane model == workspace default m0.
	if err := conn.WriteJSON(WSMessage{Type: "session_new"}); err != nil {
		t.Fatalf("send session_new: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "clear_chat" })
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" })
	newCfg := configAfterClear(t, conn, sidA)
	sidB := newCfg.SessionID
	if newCfg.Model != "m0" {
		t.Fatalf("new session config Model = %q, want workspace default m0", newCfg.Model)
	}

	rt, ok := s.registry.get(sidB)
	if !ok {
		t.Fatal("new session not registered")
	}
	if rt.agent.OnModelChanged != nil {
		t.Fatal("no model inheritance expected (pane model == default); OnModelChanged must stay nil")
	}
	if got := s.ws.DefaultModel(); got != "m0" {
		t.Fatalf("workspace default after session_new = %q, want m0", got)
	}
	// The factory still handed B its own provider (isolation holds even
	// without adoption).
	if len(*created) != 1 {
		t.Fatalf("factory created %d providers for one session_new, want 1", len(*created))
	}
}

package tui

import (
	"path/filepath"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// newSwitchTestAgent builds a fully wired agent (real provider/executor/
// context manager over a dead endpoint) so switchToLive's idle-rebuild
// path — renderMessages + refreshContextStats — runs for real.
func newSwitchTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	dir := t.TempDir()
	p := llm.NewOpenAIProvider("", "", "http://127.0.0.1:9/v1", dir)
	exec := agent.NewExecutorWithGuard(dir, agent.NewCommandGuard("", nil))
	cm := contextmgr.NewManager(p, contextmgr.Settings{})
	return agent.NewAgent(p, exec, cm)
}

// The idle path must fully rebind and rebuild without panicking: this
// exercises renderMessages + refreshContextStats against a real agent,
// which the registry-only tests skipped.
func TestSwitchToLiveIdleRebuild(t *testing.T) {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle

	a1 := newSwitchTestAgent(t)
	a2 := newSwitchTestAgent(t)

	m := &Model{agent: a1, lives: newLiveSessions(a1), viewport: vp, sessionID: a1.SessionID}
	m.lives.Add(a2, "second")
	if m.agent != a1 {
		t.Fatal("startup agent not focused")
	}

	m.switchToLive(1)
	if m.agent != a2 || m.sessionID != a2.SessionID {
		t.Fatalf("rebind failed: agent=%v sessionID=%q", m.agent != nil, m.sessionID)
	}
	if !m.lives.ByID("s2").focused.Load() || m.lives.Active() != m.lives.ByID("s2") {
		t.Fatal("focus gates wrong after idle switch")
	}
	// An empty session legitimately renders nothing; the assertion is that
	// the full rebuild path (renderMessages + refreshContextStats) ran
	// against a real agent without panicking or desyncing registry state.
	if m.lives.active != 1 || len(m.lives.sessions) != 2 {
		t.Fatalf("registry desynced: active=%d n=%d", m.lives.active, len(m.lives.sessions))
	}

	m.switchToLive(0)
	if m.agent != a1 || m.lives.active != 0 {
		t.Fatal("switch back failed")
	}
	_ = filepath.Join // keep import stable if helpers change
}

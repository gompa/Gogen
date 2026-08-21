package agent

import (
	"testing"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// TestFeatureFlagsDefaultOff verifies the live feature flags default to off
// with the default nesting depth, exactly like the config layer.
func TestFeatureFlagsDefaultOff(t *testing.T) {
	prov := llm.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	if a.BoardEnabled() {
		t.Fatal("board should default off")
	}
	if a.SubagentsEnabled() {
		t.Fatal("subagents should default off")
	}
	if d := a.SubagentMaxDepth(); d != config.DefaultSubagentMaxDepth {
		t.Fatalf("default depth = %d, want %d", d, config.DefaultSubagentMaxDepth)
	}
	if n := a.SubagentMaxConcurrent(); n != config.DefaultSubagentMaxConcurrent {
		t.Fatalf("default concurrent limit = %d, want %d", n, config.DefaultSubagentMaxConcurrent)
	}
}

// TestFeatureFlagSetters verifies the setters publish atomically (run under
// -race to check concurrent readers).
func TestFeatureFlagSetters(t *testing.T) {
	prov := llm.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	a.SetBoardEnabled(true)
	a.SetSubagentsEnabled(true)
	a.SetSubagentMaxDepth(3)
	if !a.BoardEnabled() || !a.SubagentsEnabled() {
		t.Fatalf("flags not set: board=%v subagents=%v", a.BoardEnabled(), a.SubagentsEnabled())
	}
	if d := a.SubagentMaxDepth(); d != 3 {
		t.Fatalf("depth = %d, want 3", d)
	}
	a.SetSubagentMaxDepth(0)
	if d := a.SubagentMaxDepth(); d != config.DefaultSubagentMaxDepth {
		t.Fatalf("zero depth should fall back to default, got %d", d)
	}
	a.SetSubagentMaxConcurrent(2)
	if n := a.SubagentMaxConcurrent(); n != 2 {
		t.Fatalf("concurrent limit = %d, want 2", n)
	}
	a.SetSubagentMaxConcurrent(0)
	if n := a.SubagentMaxConcurrent(); n != config.DefaultSubagentMaxConcurrent {
		t.Fatalf("zero concurrent limit should fall back to default, got %d", n)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			_ = a.BoardEnabled()
			_ = a.SubagentsEnabled()
			_ = a.SubagentMaxDepth()
			_ = a.SubagentMaxConcurrent()
		}
	}()
	for i := 0; i < 100; i++ {
		a.SetBoardEnabled(i%2 == 0)
	}
	<-done
}

// TestSessionAgentFactorySeedsFlags verifies NewSessionAgent propagates the
// live feature flags onto the created agent.
func TestSessionAgentFactorySeedsFlags(t *testing.T) {
	prov := llm.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	opts := SessionAgentOptions{
		Provider:              prov,
		Executor:              exec,
		Config:                &config.Config{ContextLimit: 128000},
		WorkingDir:            exec.GetWorkingDir(),
		BoardEnabled:          true,
		SubagentsEnabled:      true,
		SubagentMaxDepth:      4,
		SubagentMaxConcurrent: 7,
	}
	a := NewSessionAgent(opts, nil, "test-id")
	if !a.BoardEnabled() || !a.SubagentsEnabled() {
		t.Fatalf("factory did not seed flags: board=%v subagents=%v", a.BoardEnabled(), a.SubagentsEnabled())
	}
	if d := a.SubagentMaxDepth(); d != 4 {
		t.Fatalf("factory depth = %d, want 4", d)
	}
	if n := a.SubagentMaxConcurrent(); n != 7 {
		t.Fatalf("factory concurrent limit = %d, want 7", n)
	}
	if a.SessionID != "test-id" {
		t.Fatalf("session id = %q, want test-id", a.SessionID)
	}
}

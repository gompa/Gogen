package agent

import (
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// noModelProvider is a MockProvider whose ModelName() is always empty, so
// requireModelSelected fails and StreamProcessInput's first-turn error path
// (append → truncate) is exercised.
type noModelProvider struct {
	llm.MockProvider
}

func (p *noModelProvider) ModelName() string { return "" }

// TestFailedFirstTurnClearsStaleLabel pins the stale-label bug: when the
// very first turn fails before reaching the model (requireModelSelected),
// the just-appended user message is truncated — the label derived from it
// must be cleared too, or the session keeps a title whose message no longer
// exists (the next turn would never re-derive it, since the label is only
// set when empty).
func TestFailedFirstTurnClearsStaleLabel(t *testing.T) {
	prov := &noModelProvider{*llm.NewMockProvider()}
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(prov, NewExecutor(t.TempDir()), ctxMgr)

	if _, err := a.StreamProcessInput(t.Context(), "fix the parser", nil); err == nil {
		t.Fatal("expected requireModelSelected error")
	}
	if got := a.SessionLabelSnapshot(); got != "" {
		t.Fatalf("label after failed first turn = %q, want empty (the only message was dropped)", got)
	}
	if got := a.MessageCount(); got != 0 {
		t.Fatalf("message count = %d, want 0 (the failed message must be truncated)", got)
	}
}

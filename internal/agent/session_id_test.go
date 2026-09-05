package agent

import (
	"testing"

	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// recordingProvider wraps a mock provider and records SetSessionID calls so
// the agent→provider session sync is observable.
type recordingProvider struct {
	llm.LLMProvider
	last  string
	calls int
}

func (r *recordingProvider) SetSessionID(id string) {
	r.last = id
	r.calls++
}

// TestAgentSetSessionIDSyncsProvider pins the choke point: every session
// transition through Agent.SetSessionID updates a.SessionID AND the
// provider's session view (llm.SessionIDSetter), so the two can never drift.
func TestAgentSetSessionIDSyncsProvider(t *testing.T) {
	rec := &recordingProvider{LLMProvider: llmtest.NewMockProvider()}
	a := NewAgent(rec, NewExecutor(t.TempDir()), nil)

	a.SetSessionID("sess-1")
	if a.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", a.SessionID)
	}
	if rec.last != "sess-1" || rec.calls != 1 {
		t.Fatalf("provider sync = (%q, %d), want (sess-1, 1)", rec.last, rec.calls)
	}

	a.SetSessionID("sess-2")
	if rec.last != "sess-2" || rec.calls != 2 {
		t.Fatalf("provider sync after switch = (%q, %d), want (sess-2, 2)", rec.last, rec.calls)
	}
}

// TestAgentSetSessionIDPlainProvider pins that providers without the
// llm.SessionIDSetter capability (mocks, stubs) are a silent no-op.
func TestAgentSetSessionIDPlainProvider(t *testing.T) {
	a := NewAgent(llmtest.NewMockProvider(), NewExecutor(t.TempDir()), nil)
	a.SetSessionID("sess-plain")
	if a.SessionID != "sess-plain" {
		t.Fatalf("SessionID = %q, want sess-plain", a.SessionID)
	}
}

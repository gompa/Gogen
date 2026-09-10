package agent

// Regression test for the silent save-on-exit drop: a failed persist used to
// consume the dirty flag and never restore it, so the session looked clean
// and every later flush — including the exit sweep (ShutdownSessions /
// flushAndQuit, both FlushPending) — skipped it, losing the unsaved content.
// doPersist now restores the dirty flag when the write fails, so the next
// flush retries.

import (
	"fmt"
	"sync"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// transientFailStore fails the first failCount Save/AppendMessages calls
// (simulating a transient disk/target error during a flush), then records the
// persisted conversation.
type transientFailStore struct {
	mu        sync.Mutex
	failCount int
	saved     []llm.Message
}

func (s *transientFailStore) Save(_ string, snap SessionSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failCount > 0 {
		s.failCount--
		return fmt.Errorf("transient save failure")
	}
	s.saved = append([]llm.Message(nil), snap.Messages...)
	return nil
}

func (s *transientFailStore) AppendMessages(_ string, snap SessionSnapshot, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failCount > 0 {
		s.failCount--
		return fmt.Errorf("transient delta failure")
	}
	s.saved = append([]llm.Message(nil), snap.Messages...)
	return nil
}

func (s *transientFailStore) LoadInWorkingDir(string, string) (SessionSnapshot, error) {
	return SessionSnapshot{}, nil
}
func (s *transientFailStore) List(string) ([]SessionInfo, error) { return nil, nil }
func (s *transientFailStore) LatestID(string) (string, error)    { return "", nil }
func (s *transientFailStore) Delete(string, string) error        { return nil }
func (s *transientFailStore) TouchSession(string, string) error  { return nil }

func (s *transientFailStore) lastSaved() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llm.Message(nil), s.saved...)
}

func TestFailedPersistIsRetriedOnExitFlush(t *testing.T) {
	store := &transientFailStore{failCount: 1} // fail the next write, then succeed
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(prov, exec, ctxMgr)
	a.SessionStore = store
	a.SessionID = "retry-sess"

	a.appendMessage(llm.Message{Role: "user", Content: "hello"})

	// The write fails (transient error): the session must stay dirty.
	a.FlushSession()
	if got := store.lastSaved(); got != nil {
		t.Fatalf("setup: failed write must persist nothing, got %v", msgsContents(got))
	}

	// The exit sweep retries and succeeds. Before the fix the failure had
	// consumed the dirty flag, so this FlushPending was a no-op.
	a.FlushPending()
	got := store.lastSaved()
	if len(got) != 1 || got[0].Content != "hello" {
		t.Fatalf("exit flush after a failed save persisted %v, want [hello] (dirty flag was consumed by the failure)", msgsContents(got))
	}
}

package agent

// Regression tests for the turn-start persist path:
//
//  1. A turn that cannot start (no model selected) must not write anything:
//     the old code flushed the user message BEFORE the model check (creating
//     the session file) and then flushed the rollback (overwriting it with
//     an empty snapshot) — the "session saved as an empty session" bug. The
//     failed turn must leave the store untouched, and a deliberate rename
//     must survive it.
//
//  2. A cancelled PARALLEL tool round must be persisted: the old code left
//     the round in memory without marking the session dirty, so the shutdown
//     sweep (FlushPending) and the TUI's /resume (both write only dirty
//     sessions) silently lost it.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// recordingPersistStore records every Save/AppendMessages call so tests can
// assert exactly which writes happened (or that none did).
type recordingPersistStore struct {
	mu     sync.Mutex
	saves  []SessionSnapshot
	deltas []SessionSnapshot
}

func (r *recordingPersistStore) Save(id string, snap SessionSnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves = append(r.saves, snap)
	return nil
}

func (r *recordingPersistStore) AppendMessages(id string, snap SessionSnapshot, _ int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deltas = append(r.deltas, snap)
	return nil
}

func (r *recordingPersistStore) LoadInWorkingDir(string, string) (SessionSnapshot, error) {
	return SessionSnapshot{}, fmt.Errorf("not found")
}
func (r *recordingPersistStore) List(string) ([]SessionInfo, error) { return nil, nil }
func (r *recordingPersistStore) LatestID(string) (string, error)    { return "", nil }
func (r *recordingPersistStore) Delete(string, string) error        { return nil }
func (r *recordingPersistStore) TouchSession(string, string) error  { return nil }

// noModelProvider is defined in stale_label_test.go (a MockProvider whose
// ModelName() is always empty), so requireModelSelected fails and the
// first-turn rollback path is exercised.

// toolRoundProvider has a model selected and answers the first stream
// request with one read_file tool call (parallel-eligible), so a cancel
// lands inside the parallel tool round.
type toolRoundProvider struct {
	used atomic.Bool
}

func (p *toolRoundProvider) GenerateResponse(context.Context, []llm.Message, map[string]struct{}, []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}
func (p *toolRoundProvider) GenerateResponseStream(context.Context, []llm.Message, map[string]struct{}, []llm.Tool, *llm.StreamHandlers) (*llm.StreamResult, error) {
	if p.used.Swap(true) {
		return &llm.StreamResult{}, nil
	}
	return &llm.StreamResult{
		Usage: &llm.Usage{},
		ToolCalls: []llm.ToolCall{{
			ID:   "call_1",
			Name: "read_file",
			Args: map[string]any{"path": "x"},
		}},
	}, nil
}
func (p *toolRoundProvider) ModelContextLimit(context.Context) (int, error) { return 128000, nil }
func (p *toolRoundProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (p *toolRoundProvider) SetModel(string) error { return nil }
func (p *toolRoundProvider) ModelName() string     { return "test-model" }
func (p *toolRoundProvider) SetThinkingLevel(string) {
}

func newPersistTestAgent(t *testing.T, prov llm.LLMProvider, store SessionPersister) *Agent {
	t.Helper()
	exec := NewExecutor(t.TempDir())
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(prov, exec, ctxMgr)
	a.SessionStore = store
	a.SessionID = "test-sess"
	return a
}

// TestFailedModelCheckWritesNothing pins the empty-session bug: a fresh
// session whose first message fails the model check must not persist
// anything — no session file, no index entry (the old flow flushed the
// message first, creating the file, then flushed the rollback, leaving a
// 0-message session that showed up in the saved list and became the restore
// target on the next startup).
func TestFailedModelCheckWritesNothing(t *testing.T) {
	store := &recordingPersistStore{}
	a := newPersistTestAgent(t, &noModelProvider{*llm.NewMockProvider()}, store)

	_, err := a.StreamProcessInput(context.Background(), "hello", nil)
	if err == nil {
		t.Fatal("expected no-model-selected error")
	}
	if got := a.MessageCount(); got != 0 {
		t.Fatalf("in-memory messages after failed turn = %d, want 0 (message must be rolled back)", got)
	}
	if got := a.SessionLabelSnapshot(); got != "" {
		t.Fatalf("label after failed turn = %q, want empty (derived from the dropped message)", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saves) != 0 || len(store.deltas) != 0 {
		t.Fatalf("failed turn persisted %d save(s) and %d delta(s); want none (an empty session must never be written)",
			len(store.saves), len(store.deltas))
	}
}

// TestFailedModelCheckKeepsRenamedLabel pins the rename-clobber bug: a
// deliberately renamed session that fails the model check must keep its
// custom title AND the rename marker — the old code re-derived the label
// from the remaining messages, which cleared the marker and replaced the
// title with the first-message text on the next save.
func TestFailedModelCheckKeepsRenamedLabel(t *testing.T) {
	store := &recordingPersistStore{}
	a := newPersistTestAgent(t, &noModelProvider{*llm.NewMockProvider()}, store)

	if _, err := a.RenameSession("My Custom Title"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	store.mu.Lock()
	savesBefore := len(store.saves)
	store.mu.Unlock()
	if savesBefore != 1 {
		t.Fatalf("rename persisted %d save(s), want 1 (renamed empty sessions are persisted deliberately)", savesBefore)
	}

	_, err := a.StreamProcessInput(context.Background(), "hello", nil)
	if err == nil {
		t.Fatal("expected no-model-selected error")
	}
	if got := a.SessionLabelSnapshot(); got != "My Custom Title" {
		t.Fatalf("label after failed turn = %q, want %q (a deliberate rename must survive)", got, "My Custom Title")
	}

	// The failed turn itself must not have written anything.
	store.mu.Lock()
	if len(store.saves) != savesBefore {
		store.mu.Unlock()
		t.Fatalf("failed turn wrote %d save(s); want none beyond the rename", len(store.saves)-savesBefore)
	}
	store.mu.Unlock()

	// The next save (a successful retry, or a quit flush) must persist the
	// rename with its marker intact.
	a.FlushSession()
	store.mu.Lock()
	defer store.mu.Unlock()
	last := store.saves[len(store.saves)-1]
	if last.Label != "My Custom Title" {
		t.Fatalf("persisted label = %q, want %q", last.Label, "My Custom Title")
	}
	if !last.LabelRenamed {
		t.Fatal("persisted LabelRenamed = false, want true (the rename marker must survive the failed turn)")
	}
}

// TestParallelToolCancelPersistsRound pins the lost-round bug: cancelling a
// turn inside a PARALLEL tool round must persist the round's assistant
// message and tool results. The old code returned without flushing and
// without marking the session dirty, so the shutdown/eviction sweep
// (FlushPending) and the TUI's /resume — both write only dirty sessions —
// silently dropped the last round.
func TestParallelToolCancelPersistsRound(t *testing.T) {
	store := &recordingPersistStore{}
	a := newPersistTestAgent(t, &toolRoundProvider{}, store)

	started := make(chan struct{})
	var once sync.Once
	a.SetToolHandlers(map[string]ToolHandler{
		"read_file": func(ctx context.Context, a *Agent, args map[string]any) (string, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return "", ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := a.StreamProcessInput(ctx, "hello", nil)
		errCh <- err
	}()

	<-started // the parallel tool call is in flight
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("turn error = %v, want context.Canceled", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deltas) == 0 {
		t.Fatalf("cancelled parallel round was not persisted (no delta save): the round would be lost on shutdown / resume")
	}
	last := store.deltas[len(store.deltas)-1]
	if len(last.Messages) != 2 {
		t.Fatalf("persisted round has %d message(s), want 2 (assistant + tool result): %v",
			len(last.Messages), msgsContents(last.Messages))
	}
	if last.Messages[0].Role != "assistant" || len(last.Messages[0].ToolCalls) != 1 {
		t.Fatalf("persisted[0] = %+v, want the assistant message with the tool call", last.Messages[0])
	}
	if last.Messages[1].Role != "tool" || last.Messages[1].ToolCallID != "call_1" {
		t.Fatalf("persisted[1] = %+v, want the tool result for call_1", last.Messages[1])
	}
}

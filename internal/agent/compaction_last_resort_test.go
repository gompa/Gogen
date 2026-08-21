package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// archiveRecordingStore is a SessionPersister + ArchiveAppender that records
// the archive sidecar entries (Phase 5) so tests can assert on them.
type archiveRecordingStore struct {
	mu       sync.Mutex
	archives []ArchiveEntry
}

func (r *archiveRecordingStore) Save(string, SessionSnapshot) error { return nil }
func (r *archiveRecordingStore) AppendMessages(string, SessionSnapshot, int) error {
	return nil
}
func (r *archiveRecordingStore) LoadInWorkingDir(string, string) (SessionSnapshot, error) {
	return SessionSnapshot{}, fmt.Errorf("not found")
}
func (r *archiveRecordingStore) List(string) ([]SessionInfo, error) { return nil, nil }
func (r *archiveRecordingStore) LatestID(string) (string, error)    { return "", nil }
func (r *archiveRecordingStore) Delete(string, string) error        { return nil }
func (r *archiveRecordingStore) TouchSession(string, string) error  { return nil }

func (r *archiveRecordingStore) AppendArchive(_, _ string, entry ArchiveEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.archives = append(r.archives, entry)
	return nil
}

func (r *archiveRecordingStore) entries() []ArchiveEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ArchiveEntry(nil), r.archives...)
}

// newLastResortTestAgent builds an agent with a placeholder context limit
// (each test derives the real limit from the measured wire overhead so the
// pre-flight math is exact) and an archive-recording session store.
func newLastResortTestAgent(t *testing.T) (*Agent, *contextmgr.Manager, *countingProvider, *archiveRecordingStore) {
	t.Helper()
	provider := &countingProvider{limit: 1000000, fail: false}
	mgr := contextmgr.NewManager(provider, contextmgr.Settings{
		ContextLimit:              1000000,
		CompactThreshold:          0.85,
		CompactKeepRecentMessages: 2,
		CompactReserveTokens:      4000,
	})
	a := NewAgent(provider, &Executor{WorkingDir: "."}, mgr)
	store := &archiveRecordingStore{}
	a.SessionStore = store
	a.SessionID = "last-resort-test"
	return a, mgr, provider, store
}

// setLimitOverBy derives the context limit so the outgoing estimate lands
// over tokens ABOVE the window: the request would be refused.
func setLimitOverBy(t *testing.T, a *Agent, mgr *contextmgr.Manager, over int) {
	t.Helper()
	a.statsMu.RLock()
	msgTok := 0
	for _, c := range a.tokenCounts {
		msgTok += c
	}
	a.statsMu.RUnlock()
	mgr.SetContextLimit(a.wireOverheadTokens() + msgTok - over)
}

// TestLastResortFreshSessionOversizedMessage pins the core fix and the
// idempotency guard: a fresh session whose single user message is bigger
// than the window (the message is the head, so there is no middle to
// summarize — forced compaction is a no-op). The last-resort condensation
// replaces the message in place with a clearly-marked condensed version,
// announces it in-band, archives the original to the sidecar, and the
// request fits afterwards. A second pass with the window still too small
// must NOT re-condense the already-condensed message.
func TestLastResortFreshSessionOversizedMessage(t *testing.T) {
	a, mgr, provider, store := newLastResortTestAgent(t)
	original := textOfTokens(t, 2200)
	a.appendMessage(llm.Message{Role: "user", Content: original})
	_ = a.ContextStats(context.Background())
	setLimitOverBy(t, a, mgr, 50)

	var notes []string
	var compacting int32
	h := &llm.StreamHandlers{
		OnCompacting: func() { atomic.AddInt32(&compacting, 1) },
		OnCondensed:  func(note string) { notes = append(notes, note) },
	}

	view, err := a.prepareMessages(context.Background(), h)
	if err != nil {
		t.Fatalf("prepareMessages: %v", err)
	}
	// The message was condensed in place (same index, same role)...
	if len(a.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (in-place condensation)", len(a.Messages))
	}
	m := a.Messages[0]
	if m.Role != "user" || !contextmgr.IsCondensedMessage(m.Content) {
		t.Fatalf("message = role %q content %q, want the marked condensed user message", m.Role, m.Content)
	}
	// ...and announced in-band with the size vs the window and the archive.
	if len(notes) != 1 {
		t.Fatalf("OnCondensed fired %d times, want 1", len(notes))
	}
	if !strings.Contains(notes[0], fmt.Sprintf("%d-token", mgr.ContextLimit())) {
		t.Fatalf("note %q does not name the window", notes[0])
	}
	if !strings.Contains(notes[0], "archived") {
		t.Fatalf("note %q does not say the original was archived", notes[0])
	}
	if atomic.LoadInt32(&compacting) == 0 {
		t.Fatal("OnCompacting never fired: the UI has no progress indicator")
	}
	// ...and the original was archived to the sidecar (Phase 5).
	entries := store.entries()
	if len(entries) != 1 {
		t.Fatalf("archive entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Kind != "condensed_message" || e.Role != "user" || e.Index != 0 || e.Content != original {
		t.Fatalf("archive entry = %+v, want the original user message", e)
	}
	// ...and the request now fits the window.
	if est := a.outgoingViewEstimate(view); est >= mgr.ContextLimit() {
		t.Fatalf("estimate after condensation = %d, want < limit %d", est, mgr.ContextLimit())
	}

	// Idempotency guard: the condensed message is not a candidate for a
	// second condensation. Shrink the window below the (now condensed)
	// request and run the pre-flight again: no new condensation, no new
	// summarization call, no new archive entry.
	condensed := a.Messages[0].Content
	calls1 := atomic.LoadInt64(&provider.calls)
	entries1 := len(store.entries())
	a.statsMu.RLock()
	msgTok := a.tokenCounts[0]
	a.statsMu.RUnlock()
	mgr.SetContextLimit(a.wireOverheadTokens() + msgTok - 50)
	if _, err := a.prepareMessages(context.Background(), nil); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if a.Messages[0].Content != condensed {
		t.Fatal("the condensed message was modified again")
	}
	if got := atomic.LoadInt64(&provider.calls); got != calls1 {
		t.Fatalf("second pass made %d -> %d summarization calls, want none (idempotency guard)", calls1, got)
	}
	if got := len(store.entries()); got != entries1 {
		t.Fatalf("archive entries = %d, want %d (no second condensation)", got, entries1)
	}
}

// TestLastResortErrorModeDiagnostic pins the config escape hatch on both
// entry points: with compact_last_resort=error the still-over request
// returns the clear diagnostic instead of the raw provider refusal — via
// the pre-flight (prepareMessages) and via the Phase 3 hand-off
// (handleOverflowError) — and the history is left untouched (no
// condensation, no archive, no summarization call).
func TestLastResortErrorModeDiagnostic(t *testing.T) {
	a, mgr, provider, store := newLastResortTestAgent(t)
	s := mgr.SettingsSnapshot()
	s.CompactLastResort = "error"
	mgr.UpdateSettings(s)

	original := textOfTokens(t, 2200)
	a.appendMessage(llm.Message{Role: "user", Content: original})
	_ = a.ContextStats(context.Background())
	setLimitOverBy(t, a, mgr, 50)

	view, err := a.prepareMessages(context.Background(), nil)
	if err == nil {
		t.Fatal("prepareMessages: want the error-mode diagnostic")
	}
	if !strings.Contains(err.Error(), "shorten it or start a fresh session") {
		t.Fatalf("diagnostic %q is not actionable", err.Error())
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d-token window", mgr.ContextLimit())) {
		t.Fatalf("diagnostic %q does not name the window", err.Error())
	}
	if view != nil {
		t.Fatalf("prepareMessages: view = %d message(s), want nil on the diagnostic path", len(view))
	}
	// History unchanged: no condensation, no archive, no summarization.
	if len(a.Messages) != 1 || a.Messages[0].Content != original {
		t.Fatalf("history changed in error mode: %d message(s), content %q", len(a.Messages), a.Messages[0].Content)
	}
	if len(store.entries()) != 0 {
		t.Fatalf("archive entries = %d, want 0 in error mode", len(store.entries()))
	}
	if got := atomic.LoadInt64(&provider.calls); got != 0 {
		t.Fatalf("summarization calls = %d, want 0 in error mode", got)
	}

	// Phase 3 hand-off (handleOverflowError): the terminal error is the same
	// actionable diagnostic (not the raw provider refusal), history untouched.
	retry, terminal := a.handleOverflowError(context.Background(), nil, llm.ErrContextWindowExceeded)
	if retry {
		t.Fatal("retry = true in error mode, want the diagnostic")
	}
	if terminal == nil {
		t.Fatal("terminal = nil, want the actionable diagnostic")
	}
	if !strings.Contains(terminal.Error(), "shorten it or start a fresh session") {
		t.Fatalf("terminal %q is not actionable", terminal.Error())
	}
	if strings.Contains(terminal.Error(), "maximum context length") {
		t.Fatalf("terminal %q is the raw provider refusal, want the diagnostic", terminal.Error())
	}
	if len(a.Messages) != 1 || a.Messages[0].Content != original {
		t.Fatalf("history changed after the Phase 3 diagnostic: %d message(s), content %q", len(a.Messages), a.Messages[0].Content)
	}
	if len(store.entries()) != 0 {
		t.Fatalf("archive entries = %d, want 0 after the Phase 3 diagnostic", len(store.entries()))
	}
}

// TestLastResortNormalMessagePathNeverEntered pins the regression guard:
// with a normal-sized message comfortably under the window, the last-resort
// path is never entered — no condensation, no notification, no archive, no
// summarization call.
func TestLastResortNormalMessagePathNeverEntered(t *testing.T) {
	a, mgr, provider, store := newLastResortTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	_ = a.ContextStats(context.Background())
	a.statsMu.RLock()
	msgTok := a.tokenCounts[0]
	a.statsMu.RUnlock()
	mgr.SetContextLimit(a.wireOverheadTokens() + msgTok + 5000)

	var notes []string
	view, err := a.prepareMessages(context.Background(), &llm.StreamHandlers{
		OnCondensed: func(note string) { notes = append(notes, note) },
	})
	if err != nil {
		t.Fatalf("prepareMessages: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("OnCondensed fired %d times, want 0 (path never entered)", len(notes))
	}
	if got := atomic.LoadInt64(&provider.calls); got != 0 {
		t.Fatalf("summarization calls = %d, want 0", got)
	}
	if len(store.entries()) != 0 {
		t.Fatalf("archive entries = %d, want 0", len(store.entries()))
	}
	if a.Messages[0].Content != "hello" {
		t.Fatalf("message = %q, want unchanged", a.Messages[0].Content)
	}
	if est := a.outgoingViewEstimate(view); est >= mgr.ContextLimit() {
		t.Fatalf("estimate %d over limit %d in a no-op test", est, mgr.ContextLimit())
	}
}

// TestLastResortFloorOverNotEntered pins the "strictly last-resort" guard
// from the other side: the request is over the window, but NO single
// message is the cause (every message is small; condensing the largest one
// would not fit the request). The path stays out of the way — the view is
// handed off as-is (the provider refusal, wrapped by Phase 3, is the
// diagnostic).
func TestLastResortFloorOverNotEntered(t *testing.T) {
	a, mgr, _, store := newLastResortTestAgent(t)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 800)})
	a.appendMessage(llm.Message{Role: "user", Content: textOfTokens(t, 800)})
	_ = a.ContextStats(context.Background())
	// Pin the two large messages: forced compaction has no middle (the tail
	// covers everything after the head), so it is a strict-shrink abort —
	// the still-over state reaches the last-resort check.
	pm := NewPinManager()
	a.PinManager = pm
	pm.ReplacePins(map[int]struct{}{1: {}, 2: {}})
	setLimitOverBy(t, a, mgr, 50)

	var notes []string
	view, err := a.prepareMessages(context.Background(), &llm.StreamHandlers{
		OnCondensed: func(note string) { notes = append(notes, note) },
	})
	if err != nil {
		t.Fatalf("prepareMessages: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("OnCondensed fired %d times, want 0 (the floor is over, not one message)", len(notes))
	}
	if len(a.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (unchanged)", len(a.Messages))
	}
	if len(store.entries()) != 0 {
		t.Fatalf("archive entries = %d, want 0", len(store.entries()))
	}
	if est := a.outgoingViewEstimate(view); est < mgr.ContextLimit() {
		t.Fatalf("estimate %d under limit %d: the test did not set up a still-over request", est, mgr.ContextLimit())
	}
}

// TestLastResortOverflowRecoveryCondenses pins the Phase 3 hand-off: a
// provider context-window refusal on a fresh session (forced compaction
// cannot shrink — there is no middle) recovers via the last-resort
// condensation and the turn retries with the shrunken history.
func TestLastResortOverflowRecoveryCondenses(t *testing.T) {
	a, mgr, _, store := newLastResortTestAgent(t)
	original := textOfTokens(t, 2200)
	a.appendMessage(llm.Message{Role: "user", Content: original})
	_ = a.ContextStats(context.Background())
	setLimitOverBy(t, a, mgr, 50)

	var notes []string
	retry, terminal := a.handleOverflowError(context.Background(),
		&llm.StreamHandlers{OnCondensed: func(note string) { notes = append(notes, note) }},
		llm.ErrContextWindowExceeded)
	if terminal != nil {
		t.Fatalf("terminal = %v, want a retry after condensation", terminal)
	}
	if !retry {
		t.Fatal("retry = false, want the turn to continue with the condensed history")
	}
	if len(a.Messages) != 1 || !contextmgr.IsCondensedMessage(a.Messages[0].Content) {
		t.Fatalf("history = %d message(s) %q, want the condensed message", len(a.Messages), a.Messages[0].Content)
	}
	if len(notes) != 1 {
		t.Fatalf("OnCondensed fired %d times, want 1", len(notes))
	}
	entries := store.entries()
	if len(entries) != 1 || entries[0].Content != original {
		t.Fatalf("archive entries = %+v, want the original archived", entries)
	}
}

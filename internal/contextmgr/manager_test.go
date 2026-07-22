package contextmgr

import (
	"context"
	"strings"
	"testing"
	"time"

	"gogen/internal/llm"
)

type stubProvider struct {
	summary string
}

func (s *stubProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: s.summary}, nil
}

func (s *stubProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (s *stubProvider) ModelContextLimit(_ context.Context) (int, error) {
	return 128000, nil
}

func (s *stubProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{{ID: "test-model", ContextLimit: 128000, Current: true}}, nil
}

func (s *stubProvider) SetModel(id string) error {
	return nil
}

func (s *stubProvider) ModelName() string {
	return "test-model"
}

type blockingLimitProvider struct {
	stubProvider
	entered chan struct{}
	release chan struct{}
}

func (p *blockingLimitProvider) ModelContextLimit(_ context.Context) (int, error) {
	close(p.entered)
	<-p.release
	return 64000, nil
}

func TestEnsureContextLimitDoesNotHoldLockDuringProviderIO(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	m := NewManager(&blockingLimitProvider{
		entered: entered,
		release: release,
	}, Settings{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.EnsureContextLimit(context.Background())
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider ModelContextLimit was not entered")
	}

	// Snapshot only needs RLock; it must not stall behind provider I/O.
	snapDone := make(chan struct{})
	go func() {
		defer close(snapDone)
		_ = m.Snapshot(nil, nil)
	}()
	select {
	case <-snapDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Snapshot blocked while EnsureContextLimit waited on provider")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureContextLimit did not finish")
	}
	if got := m.ContextLimit(); got != 64000 {
		t.Fatalf("ContextLimit=%d, want 64000", got)
	}
}

func TestTruncateToolResult(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{MaxToolResultBytes: 10})
	got := m.TruncateToolResult("0123456789012345")
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if len(got) < 10 {
		t.Fatalf("expected truncated prefix, got %q", got)
	}
}

func TestShouldCompactRequiresEnoughMessages(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:       1000,
		CompactThreshold:   0.75,
		KeepRecentMessages: 2,
		MaxToolResultBytes: 8192,
	})
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	if m.ShouldCompact(msgs) {
		t.Fatal("should not compact with too few messages")
	}
}

func TestShouldCompactWhenOverBudget(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:         200,
		CompactThreshold:     0.75,
		KeepRecentMessages:   2,
		MaxToolResultBytes:   8192,
		CompactReserveTokens: 20,
	})
	big := strings.Repeat("token ", 2000)
	msgs := []llm.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: big},
		{Role: "user", Content: "more"},
		{Role: "assistant", Content: "recent"},
	}
	if !m.ShouldCompact(msgs) {
		t.Fatal("expected compaction threshold to be exceeded")
	}
}

// Regression: a prior ShouldCompact call must not freeze the decision when
// the message list later grows past the compaction budget.
func TestShouldCompactSeesGrowthAcrossCalls(t *testing.T) {
	InvalidateTokenCache()
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:         8000,
		CompactThreshold:     0.5,
		KeepRecentMessages:   2,
		MaxToolResultBytes:   8192,
		CompactReserveTokens: 0,
	})
	msgs := []llm.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, llm.Message{Role: "user", Content: "u"})
		msgs = append(msgs, llm.Message{Role: "assistant", Content: "a"})
	}
	if m.ShouldCompact(msgs) {
		t.Fatal("expected under budget initially")
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: strings.Repeat("word ", 20000)})
	if !m.ShouldCompact(msgs) {
		t.Fatal("expected ShouldCompact to see growth after appending a large message")
	}
}

func TestCompactPreservesHeadAndTail(t *testing.T) {
	provider := &stubProvider{summary: "did auth work"}
	m := NewManager(provider, Settings{KeepRecentMessages: 2})
	msgs := []llm.Message{
		{Role: "user", Content: "fix auth"},
		{Role: "assistant", Content: "reading"},
		{Role: "tool", Content: "file contents", ToolCallID: "c1"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "add tests"},
	}
	out, err := m.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(out))
	}
	if out[0].Content != "fix auth" {
		t.Fatalf("head mismatch: %q", out[0].Content)
	}
	if !strings.Contains(out[1].Content, summaryPrefix) {
		t.Fatalf("expected summary message, got %q", out[1].Content)
	}
	if out[3].Content != "add tests" {
		t.Fatalf("tail mismatch: %q", out[3].Content)
	}
}

func TestEnsureToolResultsCappedSticky(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{MaxToolResultBytes: 5})
	big := strings.Repeat("x", 4000)
	msgs := []llm.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", Content: big, ToolCallID: "c1"},
	}
	if !m.EnsureToolResultsCapped(msgs) {
		t.Fatal("expected first pass to rewrite oversized tool result")
	}
	capped := msgs[1].Content
	if capped == big || !strings.Contains(capped, "truncated") {
		t.Fatalf("expected capped tool result, got %q", capped)
	}
	if m.EnsureToolResultsCapped(msgs) {
		t.Fatal("second pass should be a no-op for stable prompt prefixes")
	}
	if msgs[1].Content != capped {
		t.Fatal("capped content changed on second pass")
	}
	if msgs[1].Content != capped {
		t.Fatal("sticky capped content was lost")
	}
}

func TestSnapshot(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:         10000,
		CompactThreshold:     0.75,
		CompactReserveTokens: 100,
	})
	canonical := []llm.Message{
		{Role: "user", Content: strings.Repeat("a", 400)},
		{Role: "assistant", Content: "ok"},
	}
	llmView := append([]llm.Message{{Role: "system", Content: "sys"}}, canonical...)
	snap := m.Snapshot(canonical, llmView)
	if snap.MessageCount != 2 {
		t.Fatalf("got %d messages", snap.MessageCount)
	}
	if snap.Limit != 10000 {
		t.Fatalf("got limit %d", snap.Limit)
	}
	if snap.Used <= snap.Stored {
		t.Fatalf("expected llm view >= canonical stored tokens, used=%d stored=%d", snap.Used, snap.Stored)
	}
	if snap.CompactAt != 7400 {
		t.Fatalf("got compactAt %d", snap.CompactAt)
	}
}

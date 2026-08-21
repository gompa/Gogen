package contextmgr

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"gogen/internal/llm"
)

// countingStubProvider is a stubProvider that counts GenerateResponse calls
// so tests can assert whether the condensation hit the provider or fell
// back to the deterministic truncation.
type countingStubProvider struct {
	stubProvider
	calls int64
}

func (p *countingStubProvider) GenerateResponse(ctx context.Context, msgs []llm.Message, allowed map[string]struct{}, tools []llm.Tool) (llm.Response, error) {
	atomic.AddInt64(&p.calls, 1)
	return p.stubProvider.GenerateResponse(ctx, msgs, allowed, tools)
}

// TestCondenseMessageProviderPath pins the preferred path: a message that
// fits the summary input budget is summarized via the provider.
func TestCondenseMessageProviderPath(t *testing.T) {
	prov := &countingStubProvider{stubProvider: stubProvider{summary: "the recap"}}
	m := NewManager(prov, Settings{ContextLimit: 128000})
	out, err := m.CondenseMessage(context.Background(), llm.Message{Role: "user", Content: "please do the thing"})
	if err != nil {
		t.Fatalf("CondenseMessage: %v", err)
	}
	if out != "the recap" {
		t.Fatalf("summary = %q, want the provider's recap", out)
	}
	if got := atomic.LoadInt64(&prov.calls); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

// TestCondenseMessageTruncatedFallback pins the no-provider-call fallback:
// a message whose rendered content exceeds the summary input budget is
// truncated deterministically (the depth-cap behavior) instead of being
// sent to the provider — a message that big cannot be sent anyway.
func TestCondenseMessageTruncatedFallback(t *testing.T) {
	prov := &countingStubProvider{stubProvider: stubProvider{summary: "the recap"}}
	// limit 10000: maxIn = 10000/2 - 4000 -> clamped to 2000. A
	// ~3000-token message exceeds the budget.
	m := NewManager(prov, Settings{ContextLimit: 10000, CompactReserveTokens: 4000})
	big := strings.Repeat("x", 8*3000)
	out, err := m.CondenseMessage(context.Background(), llm.Message{Role: "user", Content: big})
	if err != nil {
		t.Fatalf("CondenseMessage: %v", err)
	}
	if !strings.Contains(out, "truncated for summarization") {
		t.Fatalf("output %q is not the deterministic truncation", out)
	}
	if got := atomic.LoadInt64(&prov.calls); got != 0 {
		t.Fatalf("provider calls = %d, want 0 (truncation fallback)", got)
	}
}

// TestCondenseMessageNoProvider pins the error path: without a provider
// there is nothing to condense with.
func TestCondenseMessageNoProvider(t *testing.T) {
	m := NewManager(nil, Settings{ContextLimit: 128000})
	if _, err := m.CondenseMessage(context.Background(), llm.Message{Role: "user", Content: "hello"}); err == nil {
		t.Fatal("CondenseMessage without a provider: want an error")
	}
}

// TestNormalizeCompactLastResort pins the setting clamp: "error" passes
// through (case-insensitive), everything else — including empty — is
// "condense" (the default).
func TestNormalizeCompactLastResort(t *testing.T) {
	for in, want := range map[string]string{
		"":         "condense",
		"condense": "condense",
		"error":    "error",
		"ERROR":    "error",
		" bogus":   "condense",
	} {
		m := NewManager(nil, Settings{ContextLimit: 128000, CompactLastResort: in})
		if got := m.CompactLastResort(); got != want {
			t.Fatalf("CompactLastResort(%q) = %q, want %q", in, got, want)
		}
	}
	// UpdateSettings normalizes too (the web settings push path).
	m := NewManager(nil, Settings{ContextLimit: 128000})
	s := m.SettingsSnapshot()
	s.CompactLastResort = "error"
	m.UpdateSettings(s)
	if got := m.CompactLastResort(); got != "error" {
		t.Fatalf("after UpdateSettings: %q, want error", got)
	}
}

// TestIsCondensedMessage pins the marker: only the framed condensed prefix
// identifies a condensed message, and a condensed message is NOT a
// compaction summary (it keeps its role and anchors the head).
func TestIsCondensedMessage(t *testing.T) {
	if !IsCondensedMessage(CondensedMessagePrefix + "recap") {
		t.Fatal("framed condensed message not recognized")
	}
	if IsCondensedMessage("plain user content") {
		t.Fatal("plain content recognized as condensed")
	}
	framed := CondensedMessagePrefix + "recap"
	if IsCompactionSummary(framed) {
		t.Fatal("a condensed message must not be a compaction summary")
	}
}

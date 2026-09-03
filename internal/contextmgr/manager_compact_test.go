package contextmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gogen/internal/llm"
)

// recordingProvider captures the summarization request so tests can verify
// its shape (view prefix + head + middle + instruction).
type recordingProvider struct {
	stubProvider
	requests [][]llm.Message
}

func (p *recordingProvider) GenerateResponse(_ context.Context, msgs []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	p.requests = append(p.requests, msgs)
	return llm.Response{Content: "middle summary"}, nil
}

func TestCompactSummaryRequestUsesConversationPrefix(t *testing.T) {
	provider := &recordingProvider{}
	m := NewManager(provider, Settings{CompactKeepRecentMessages: 2, ContextLimit: 100000, CompactThreshold: 0.01})
	m.minMiddleTokens = 0 // tiny messages: skip the minimum-middle guard
	viewPrefix := []llm.Message{{Role: "system", Content: "You are a coding agent."}}
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "middle user"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "later"},
		{Role: "assistant", Content: "a3"},
		{Role: "user", Content: "tail"},
	}
	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{ViewPrefix: viewPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected one summarization request, got %d", len(provider.requests))
	}
	req := provider.requests[0]
	if len(req) < len(viewPrefix)+2 {
		t.Fatalf("summarization request too short: %d messages", len(req))
	}
	if req[0].Content != "You are a coding agent." {
		t.Fatalf("expected view prefix first, got %q", req[0].Content)
	}
	if req[len(viewPrefix)].Content != "first" {
		t.Fatalf("expected first user message after prefix, got %q", req[len(viewPrefix)].Content)
	}
	last := req[len(req)-1]
	// The trailing instruction must be a user message: a trailing system
	// message is rejected by Anthropic and by strict Jinja chat templates
	// (Qwen3: "System message must be at the beginning").
	if last.Role != "user" || !strings.Contains(last.Content, "Summarize everything after the first user message") {
		t.Fatalf("expected user-role summary instruction last, got role=%q content=%q", last.Role, last.Content)
	}
	if !strings.Contains(last.Content, "not a conversation turn") || !strings.Contains(last.Content, "Do not continue the conversation") {
		t.Fatalf("instruction must be framed as a task, not a chat turn: %q", last.Content)
	}
	if strings.Contains(last.Content, "most recent") || strings.Contains(last.Content, "not shown") {
		t.Fatalf("instruction must not mention cut messages: %q", last.Content)
	}
	if strings.Contains(last.Content, "lead-in") || strings.Contains(last.Content, "continues after this point") {
		t.Fatalf("instruction must not cue conversation continuation: %q", last.Content)
	}
	if out[0].Content != "first" {
		t.Fatalf("head not preserved: %+v", out[0])
	}
	if out[len(out)-1].Content != "tail" {
		t.Fatalf("tail not preserved: %+v", out[len(out)-1])
	}
	foundSummary := false
	for _, msg := range out {
		if msg.Role == "assistant" && strings.Contains(msg.Content, "middle summary") {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Fatalf("expected summary message in compacted history: %+v", out)
	}
}

func TestAdjustCompactTailStartIncludesToolCallAssistant(t *testing.T) {
	messages := []llm.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", Content: "file body", ToolCallID: "c1"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "next"},
	}
	got := adjustCompactTailStart(messages, 2)
	if got != 1 {
		t.Fatalf("expected tail to include assistant tool call at index 1, got %d", got)
	}
}

func TestRenderMessagesForSummaryIncludesToolName(t *testing.T) {
	messages := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "search_code"}}},
		{Role: "tool", Content: "main.go:1:needle", ToolCallID: "c1"},
	}
	text := renderMessagesForSummary(messages, 8192)
	if !strings.Contains(text, "TOOL RESULT (search_code (c1)):") {
		t.Fatalf("expected tool name in summary, got %q", text)
	}
}

func TestCompactKeepsToolCallPairInTail(t *testing.T) {
	provider := &stubProvider{summary: "summary"}
	m := NewManager(provider, Settings{CompactKeepRecentMessages: 3})
	m.minMiddleTokens = 0 // tiny messages: skip the minimum-middle guard
	msgs := []llm.Message{
		{Role: "user", Content: "fix auth"},
		{Role: "assistant", Content: strings.Repeat("reading the codebase ", 10)}, // middle must outgrow the framed summary (smaller-summary guard)
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", Content: "file contents", ToolCallID: "c1"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "add tests"},
	}
	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out[2].Role != "assistant" || len(out[2].ToolCalls) == 0 {
		t.Fatalf("expected assistant tool call preserved in tail, got %+v", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "c1" {
		t.Fatalf("expected tool result preserved in tail, got %+v", out[3])
	}
}

// TestCompactPreservesMetadataFields verifies compaction preserves the
// persisted fields cloneMessage must not drop: attached images (vision
// input), CreatedAt timestamps, and the provider-reported Model on assistant
// messages. Pre-fix these were silently stripped from the preserved head and
// tail, so vision context vanished after the first compaction and assistant
// model chips disappeared from preserved bubbles.
func TestCompactPreservesMetadataFields(t *testing.T) {
	provider := &stubProvider{summary: "summary"}
	m := NewManager(provider, Settings{CompactKeepRecentMessages: 2})
	m.minMiddleTokens = 0 // tiny messages: skip the minimum-middle guard
	created := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	msgs := []llm.Message{
		{
			Role:      "user",
			Content:   "first with image",
			Images:    []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA", Detail: "high"}},
			CreatedAt: created,
		},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: strings.Repeat("middle content ", 10)}, // middle must outgrow the framed summary (smaller-summary guard)
		{Role: "assistant", Content: "a2", Model: "glm-4.6", CreatedAt: created.Add(time.Hour)},
		{Role: "user", Content: "tail"},
	}
	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Head: the first user message keeps its image and timestamp.
	if len(out[0].Images) != 1 || out[0].Images[0].DataURL != "data:image/png;base64,AAAA" {
		t.Fatalf("head image lost: %+v", out[0].Images)
	}
	if out[0].Images[0].Detail != "high" {
		t.Fatalf("head image detail lost: %+v", out[0].Images[0])
	}
	if !out[0].CreatedAt.Equal(created) {
		t.Fatalf("head CreatedAt lost: %v", out[0].CreatedAt)
	}
	// Tail: the preserved assistant message keeps its model + timestamp.
	found := false
	for _, msg := range out {
		if msg.Role == "assistant" && msg.Content == "a2" {
			found = true
			if msg.Model != "glm-4.6" {
				t.Fatalf("tail assistant Model lost: %q", msg.Model)
			}
			if !msg.CreatedAt.Equal(created.Add(time.Hour)) {
				t.Fatalf("tail assistant CreatedAt lost: %v", msg.CreatedAt)
			}
		}
	}
	if !found {
		t.Fatal("tail assistant message missing from compacted history")
	}
}

// compact_keep_recent_messages 0 is literal: compaction keeps only the first user
// message and summarizes everything else (no tail).
func TestCompactKeepZeroKeepsOnlyHead(t *testing.T) {
	provider := &stubProvider{summary: "summary"}
	m := NewManager(provider, Settings{CompactKeepRecentMessages: 0})
	m.minMiddleTokens = 0 // tiny messages: skip the minimum-middle guard
	msgs := []llm.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "b"},
		{Role: "assistant", Content: "c"},
	}
	out, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected [head, summary], got %d messages: %+v", len(out), out)
	}
	if out[0].Role != "user" || out[0].Content != "task" {
		t.Fatalf("expected first user message preserved, got %+v", out[0])
	}
	if !strings.Contains(out[1].Content, "summary") {
		t.Fatalf("expected summarized middle, got %+v", out[1])
	}
}

// keep=0 is literal, not "disable compaction": the len <= keep+1 guard only
// protects a head-only conversation. Once there is anything beyond the head,
// auto-compaction proceeds normally (and would summarize everything but the
// head, see TestCompactKeepZeroKeepsOnlyHead).
func TestShouldCompactWithKeepZero(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{CompactKeepRecentMessages: 0, CompactThreshold: 0.5, ContextLimit: 8000})
	msgs := []llm.Message{
		{Role: "user", Content: strings.Repeat("word ", 20000)},
	}
	if m.ShouldCompact(msgs) {
		t.Fatal("expected no compaction for a head-only conversation (len <= keep+1)")
	}
	msgs = append(msgs, llm.Message{Role: "assistant", Content: strings.Repeat("word ", 20000)})
	if !m.ShouldCompact(msgs) {
		t.Fatal("expected compaction once there is something beyond the head")
	}
}

func TestSnapshotMarksCompactDisabled(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{CompactThreshold: 0, ContextLimit: 8000})
	snap := m.Snapshot(nil, nil)
	if !snap.CompactDisabled {
		t.Fatal("expected CompactDisabled for threshold 0")
	}
	if snap.NearCompact {
		t.Fatal("expected NearCompact false when auto-compaction is disabled")
	}
}

func TestSnapshotAutoCompactEnabledByDefault(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{CompactThreshold: 0.5, ContextLimit: 8000})
	snap := m.Snapshot(nil, nil)
	if snap.CompactDisabled {
		t.Fatal("expected auto-compaction enabled at threshold 0.5")
	}
}

// TestCompactRefusesTinyMiddle verifies the minimum-middle guard: compacting a
// history whose summarized middle is trivially small is refused (the model
// would echo the one or two messages instead of recapping them), and the same
// history compacts fine once the guard is lifted.
func TestCompactRefusesTinyMiddle(t *testing.T) {
	provider := &recordingProvider{}
	m := NewManager(provider, Settings{CompactKeepRecentMessages: 2, ContextLimit: 100000, CompactThreshold: 0.01})
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "middle user"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "tail"},
	}
	if _, _, err := m.Compact(context.Background(), msgs, CompactOptions{}); err == nil {
		t.Fatal("expected compaction of a tiny middle to be refused")
	}

	// Same history with the guard lifted compacts normally. The
	// smaller-summary guard is disabled too: this tiny middle cannot
	// outgrow its framed summary (covered by TestCompactSmallerSummaryGuard).
	m.minMiddleTokens = 0
	m.requireSummaryShrink = false
	if _, _, err := m.Compact(context.Background(), msgs, CompactOptions{}); err != nil {
		t.Fatalf("expected compaction to succeed with guard lifted: %v", err)
	}

	// A middle above the default threshold is not refused.
	big := NewManager(&recordingProvider{}, Settings{CompactKeepRecentMessages: 2, ContextLimit: 100000, CompactThreshold: 0.01})
	bigMsgs := append([]llm.Message{
		{Role: "user", Content: "first"},
		{Role: "user", Content: strings.Repeat("substantial middle content ", 120)}, // ~1.5k tokens
		{Role: "assistant", Content: strings.Repeat("and assistant replies ", 120)},
	}, msgs[3:]...)
	if _, _, err := big.Compact(context.Background(), bigMsgs, CompactOptions{}); err != nil {
		t.Fatalf("expected compaction of a substantial middle to succeed: %v", err)
	}
}

// fallbackEchoProvider fails the primary continuation-summary request
// (multiple messages: head + middle + instruction) so the flattened-text
// fallback runs, and answers the fallback's single-message call with a large
// "echo" summary — standing in for a depth-capped truncated echo of the
// middle, which is exactly what the smaller-summary guard must refuse.
type fallbackEchoProvider struct {
	stubProvider
	calls int
}

func (p *fallbackEchoProvider) GenerateResponse(_ context.Context, msgs []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	p.calls++
	if len(msgs) > 1 {
		return llm.Response{}, fmt.Errorf("primary summary path forced failure")
	}
	return llm.Response{Content: strings.Repeat("echoed middle content ", 30)}, nil
}

// TestCompactSmallerSummaryGuard verifies the smaller-summary guard:
// compaction is accepted only when the framed summary (SummaryPrefix +
// summary) is strictly smaller than the summarized middle. An equal-sized
// summary is refused (the emergency fit loop needs strict shrinkage to
// terminate), a larger one is refused, and the test hook disables the guard.
func TestCompactSmallerSummaryGuard(t *testing.T) {
	const smallSummary = "ok"
	var largeSummary = strings.Repeat("long summary content ", 30)

	tests := []struct {
		name         string
		middle       string
		summary      string
		disableGuard bool
		wantErr      bool
	}{
		{"smaller summary accepted", strings.Repeat("word ", 100), smallSummary, false, false},
		// Equal: the middle's content is byte-identical to the framed
		// summary's content, so the framed message has exactly the middle's
		// token count.
		{"equal summary refused", SummaryPrefix + smallSummary, smallSummary, false, true},
		{"larger summary refused", "tiny", largeSummary, false, true},
		{"guard disabled accepts non-shrinking summary", "tiny", largeSummary, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(&stubProvider{summary: tc.summary}, Settings{
				CompactKeepRecentMessages: 1,
				ContextLimit:              100000,
				CompactThreshold:          0.5,
			})
			m.minMiddleTokens = 0
			m.requireSummaryShrink = !tc.disableGuard
			msgs := []llm.Message{
				{Role: "user", Content: "first"},
				{Role: "assistant", Content: tc.middle},
				{Role: "user", Content: "tail"},
			}
			out, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("expected compaction to succeed: %v", err)
				}
				found := false
				for _, msg := range out {
					if msg.Role == "assistant" && strings.Contains(msg.Content, tc.summary) {
						found = true
					}
				}
				if !found {
					t.Fatalf("expected summary message in compacted history: %+v", out)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected refusal, got %d messages", len(out))
			}
			if !errors.Is(err, ErrSummaryNotSmaller) {
				t.Fatalf("err = %v, want ErrSummaryNotSmaller", err)
			}
			if out != nil {
				t.Fatalf("refusal must not return compacted history, got %d messages", len(out))
			}
			if msgs[1].Content != tc.middle {
				t.Fatalf("refusal must not mutate the input history: %q", msgs[1].Content)
			}
		})
	}
}

// TestCompactSmallerSummaryGuardCoversFallbackPath verifies the guard applies
// to the flattened-text fallback too: when the primary request fails and the
// fallback returns a summary as big as the middle (a depth-capped truncated
// echo), the compaction is refused.
func TestCompactSmallerSummaryGuardCoversFallbackPath(t *testing.T) {
	provider := &fallbackEchoProvider{}
	m := NewManager(provider, Settings{
		CompactKeepRecentMessages: 1,
		ContextLimit:              100000,
		CompactThreshold:          0.5,
	})
	m.minMiddleTokens = 0
	msgs := []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: strings.Repeat("word ", 10)}, // ~20 tokens
		{Role: "user", Content: "tail"},
	}
	_, _, err := m.Compact(context.Background(), msgs, CompactOptions{})
	if err == nil {
		t.Fatal("expected the non-shrinking fallback summary to be refused")
	}
	if !errors.Is(err, ErrSummaryNotSmaller) {
		t.Fatalf("err = %v, want ErrSummaryNotSmaller", err)
	}
	if provider.calls < 2 {
		t.Fatalf("expected the flattened-text fallback to run, provider calls = %d", provider.calls)
	}
}

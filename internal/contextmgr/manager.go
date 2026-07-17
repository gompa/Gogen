package contextmgr

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/llm"
)

const summaryPrefix = "[Session summary — earlier conversation condensed]\n"
const maxSummarizeDepth = 8

// Settings controls context window management.
type Settings struct {
	ContextLimit         int
	CompactThreshold     float64
	KeepRecentMessages   int
	MaxToolResultBytes   int
	CompactReserveTokens int
}

// DefaultSettings returns defaults; ContextLimit 0 means resolve from the provider at runtime.
func DefaultSettings() Settings {
	return Settings{
		ContextLimit:         0,
		CompactThreshold:     0.75,
		KeepRecentMessages:   12,
		MaxToolResultBytes:   8192,
		CompactReserveTokens: 4000,
	}
}

// Manager builds LLM views and compacts canonical conversation history.
type Manager struct {
	Settings           Settings
	Provider           llm.LLMProvider
	limitResolved      bool
	manualContextLimit int
}

func NewManager(provider llm.LLMProvider, settings Settings) *Manager {
	if settings.CompactThreshold <= 0 || settings.CompactThreshold > 1 {
		settings.CompactThreshold = DefaultSettings().CompactThreshold
	}
	if settings.KeepRecentMessages <= 0 {
		settings.KeepRecentMessages = DefaultSettings().KeepRecentMessages
	}
	if settings.MaxToolResultBytes <= 0 {
		settings.MaxToolResultBytes = DefaultSettings().MaxToolResultBytes
	}
	if settings.CompactReserveTokens <= 0 {
		settings.CompactReserveTokens = DefaultSettings().CompactReserveTokens
	}
	manual := 0
	if settings.ContextLimit > 0 {
		manual = settings.ContextLimit
	}
	return &Manager{
		Settings:           settings,
		Provider:           provider,
		manualContextLimit: manual,
	}
}

// RefreshAfterModelChange updates the context limit for the newly selected model.
func (m *Manager) RefreshAfterModelChange(ctx context.Context) {
	if m.manualContextLimit > 0 {
		m.Settings.ContextLimit = m.manualContextLimit
		m.limitResolved = true
		return
	}
	m.Settings.ContextLimit = 0
	m.limitResolved = false
	m.EnsureContextLimit(ctx)
}

// EnsureContextLimit resolves ContextLimit from the provider when not set explicitly.
// A positive Settings.ContextLimit from GOGEN_CONTEXT_LIMIT is a manual override and
// skips provider lookup; RefreshAfterModelChange preserves that override.
func (m *Manager) EnsureContextLimit(ctx context.Context) {
	if m.Settings.ContextLimit > 0 && m.limitResolved {
		return
	}
	if m.Settings.ContextLimit > 0 {
		m.limitResolved = true
		return
	}
	if limit, err := m.Provider.ModelContextLimit(ctx); err == nil && limit > 0 {
		m.Settings.ContextLimit = limit
		m.limitResolved = true
		return
	}
	m.Settings.ContextLimit = 128000
	m.limitResolved = true
}

const toolResultTruncationMarker = "\n… truncated ("

// TruncateToolResult caps tool output stored in canonical history / LLM views.
func (m *Manager) TruncateToolResult(content string) string {
	max := m.Settings.MaxToolResultBytes
	if max <= 0 || len(content) <= max {
		return content
	}
	if strings.Contains(content, toolResultTruncationMarker) {
		return content
	}
	return content[:max] + fmt.Sprintf("\n… truncated (%d chars total)", len(content))
}

// EstimateTokens approximates token count for a message list.
func (m *Manager) EstimateTokens(messages []llm.Message) int {
	total := 0
	for _, msg := range messages {
		total += estimateMessageTokens(msg)
	}
	return total
}

func estimateMessageTokens(msg llm.Message) int {
	tokens := (len(msg.Content) + 3) / 4
	tokens += 4 // role/overhead
	for _, tc := range msg.ToolCalls {
		tokens += (len(tc.Name)+len(tc.ID)+12)/4 + 4
		for k, v := range tc.Args {
			tokens += (len(k)+len(fmt.Sprint(v))+4)/4 + 2
		}
	}
	if msg.ToolCallID != "" {
		tokens += (len(msg.ToolCallID) + 4) / 4
	}
	return tokens
}

// ContextSnapshot summarizes context window usage for display.
type ContextSnapshot struct {
	Limit         int
	Used          int
	Stored        int
	CompactAt     int
	MessageCount  int
	ToolTruncated bool
	NearCompact   bool
	Percent       float64
}

// Snapshot estimates token usage for canonical history and the LLM view.
func (m *Manager) Snapshot(canonical, llmView []llm.Message) ContextSnapshot {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = 128000
	}
	used := m.EstimateTokens(llmView)
	stored := m.EstimateTokens(canonical)
	compactAt := m.compactBudget()
	snap := ContextSnapshot{
		Limit:         limit,
		Used:          used,
		Stored:        stored,
		CompactAt:     compactAt,
		MessageCount:  len(canonical),
		ToolTruncated: hasTruncatedToolResults(canonical),
		NearCompact:   used >= compactAt,
	}
	if limit > 0 {
		snap.Percent = float64(used) / float64(limit)
	}
	return snap
}

func hasTruncatedToolResults(messages []llm.Message) bool {
	for _, msg := range messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, toolResultTruncationMarker) {
			return true
		}
	}
	return false
}

func (m *Manager) compactBudget() int {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = 128000
	}
	budget := int(float64(limit) * m.Settings.CompactThreshold)
	budget -= m.Settings.CompactReserveTokens
	if budget < 1000 {
		budget = 1000
	}
	return budget
}

// ShouldCompact reports whether messages exceed the compaction threshold.
func (m *Manager) ShouldCompact(messages []llm.Message) bool {
	if len(messages) <= m.Settings.KeepRecentMessages+1 {
		return false
	}
	return m.EstimateTokens(messages) >= m.compactBudget()
}

// EnsureToolResultsCapped mutates messages so every tool body fits MaxToolResultBytes.
// Safe to call every turn; only rewrites oversized bodies (one-time sticky rewrite).
func (m *Manager) EnsureToolResultsCapped(messages []llm.Message) bool {
	max := m.Settings.MaxToolResultBytes
	if max <= 0 {
		return false
	}
	changed := false
	for i := range messages {
		if messages[i].Role != "tool" || messages[i].Content == "" {
			continue
		}
		if strings.Contains(messages[i].Content, toolResultTruncationMarker) {
			continue
		}
		if len(messages[i].Content) <= max {
			continue
		}
		messages[i].Content = m.TruncateToolResult(messages[i].Content)
		changed = true
	}
	return changed
}

// ViewForLLM returns the message list sent to the model.
// Tool results are capped in canonical history (append + EnsureToolResultsCapped),
// not rewritten per round, so prompt-cache prefixes stay stable.
func (m *Manager) ViewForLLM(messages []llm.Message) []llm.Message {
	return messages
}

// ViewWithTruncation returns a copy of messages with tool results truncated.
// Prefer EnsureToolResultsCapped for sticky in-place capping of canonical history.
func (m *Manager) ViewWithTruncation(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	for i, msg := range messages {
		out[i] = cloneMessage(msg)
		if msg.Role == "tool" && msg.Content != "" {
			out[i].Content = m.TruncateToolResult(msg.Content)
		}
	}
	return out
}

// Compact replaces the middle of canonical history with an LLM-generated summary.
// It preserves the first user message and the most recent KeepRecentMessages entries.
func (m *Manager) Compact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
	out, _, err := m.CompactPinned(ctx, messages, nil)
	return out, err
}

// CompactPinned is like Compact but keeps pinned message indices in the preserved
// tail and returns remapped pin indices for the compacted history.
func (m *Manager) CompactPinned(ctx context.Context, messages []llm.Message, pinned map[int]struct{}) ([]llm.Message, map[int]struct{}, error) {
	if len(messages) <= m.Settings.KeepRecentMessages+1 {
		return messages, copyIntSet(pinned), nil
	}

	headIdx := firstUserIndex(messages)
	tailStart := adjustCompactTailStart(messages, len(messages)-m.Settings.KeepRecentMessages)
	tailStart = extendTailForPins(messages, headIdx, tailStart, pinned)
	if tailStart <= headIdx+1 {
		return messages, copyIntSet(pinned), nil
	}

	oldTailStart := tailStart
	head := []llm.Message{cloneMessage(messages[headIdx])}
	middle := messages[headIdx+1 : tailStart]
	tail := cloneMessages(messages[tailStart:])

	summary, err := m.summarizeMiddle(ctx, middle)
	if err != nil {
		return nil, nil, err
	}

	compact := make([]llm.Message, 0, headIdx+1+1+len(tail))
	if headIdx > 0 {
		compact = append(compact, cloneMessages(messages[:headIdx])...)
	}
	compact = append(compact, head...)
	compact = append(compact, llm.Message{
		Role:    "assistant",
		Content: summaryPrefix + summary,
	})
	compact = append(compact, tail...)

	newPinned := remapPinsAfterCompact(pinned, headIdx, oldTailStart, len(compact)-len(tail))
	return compact, newPinned, nil
}

// extendTailForPins pulls the tail start earlier so every pinned index is preserved.
func extendTailForPins(messages []llm.Message, headIdx, tailStart int, pinned map[int]struct{}) int {
	if len(pinned) == 0 {
		return tailStart
	}
	for idx := range pinned {
		if idx <= headIdx || idx >= len(messages) {
			continue
		}
		if idx < tailStart {
			tailStart = idx
		}
	}
	return adjustCompactTailStart(messages, tailStart)
}

func remapPinsAfterCompact(pinned map[int]struct{}, headIdx, oldTailStart, newTailStart int) map[int]struct{} {
	if len(pinned) == 0 {
		return nil
	}
	out := make(map[int]struct{}, len(pinned))
	for idx := range pinned {
		if idx < 0 {
			continue
		}
		if idx <= headIdx {
			out[idx] = struct{}{}
			continue
		}
		if idx >= oldTailStart {
			out[newTailStart+(idx-oldTailStart)] = struct{}{}
		}
		// Pins that fell in the summarized middle are dropped (should not happen
		// when extendTailForPins ran first).
	}
	return out
}

func copyIntSet(in map[int]struct{}) map[int]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func firstUserIndex(messages []llm.Message) int {
	for i, msg := range messages {
		if msg.Role == "user" && !strings.HasPrefix(msg.Content, summaryPrefix) {
			return i
		}
	}
	return 0
}

func (m *Manager) summarizeMiddle(ctx context.Context, middle []llm.Message) (string, error) {
	if len(middle) == 0 {
		return "", nil
	}
	return m.summarizeMessagesDepth(ctx, middle, 0)
}

func (m *Manager) summarizeMessagesDepth(ctx context.Context, messages []llm.Message, depth int) (string, error) {
	if depth >= maxSummarizeDepth {
		text := renderMessagesForSummary(messages, m.Settings.MaxToolResultBytes)
		return truncateForSummary(text, m.maxSummaryInputTokens()), nil
	}
	return m.summarizeMessages(ctx, messages, depth)
}

func (m *Manager) summarizeMessages(ctx context.Context, messages []llm.Message, depth int) (string, error) {
	text := renderMessagesForSummary(messages, m.Settings.MaxToolResultBytes)
	if m.EstimateTokens([]llm.Message{{Content: text}}) <= m.maxSummaryInputTokens() {
		return m.summarizeText(ctx, text)
	}
	if len(messages) == 1 {
		return m.summarizeText(ctx, truncateForSummary(text, m.maxSummaryInputTokens()))
	}

	mid := len(messages) / 2
	left, err := m.summarizeMessages(ctx, messages[:mid], depth+1)
	if err != nil {
		return "", err
	}
	right, err := m.summarizeMessages(ctx, messages[mid:], depth+1)
	if err != nil {
		return "", err
	}
	merged := "Earlier segment summary:\n" + left + "\n\nLater segment summary:\n" + right
	if m.EstimateTokens([]llm.Message{{Content: merged}}) <= m.maxSummaryInputTokens() {
		return m.summarizeText(ctx, merged)
	}
	return merged, nil
}

func (m *Manager) maxSummaryInputTokens() int {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = 128000
	}
	budget := limit/2 - m.Settings.CompactReserveTokens
	if budget < 2000 {
		budget = 2000
	}
	return budget
}

func renderMessagesForSummary(messages []llm.Message, maxToolBytes int) string {
	toolNames := toolNamesFromMessages(messages)
	var b strings.Builder
	for _, msg := range messages {
		writeMessageForSummary(&b, msg, maxToolBytes, toolNames)
	}
	return b.String()
}

func toolNamesFromMessages(messages []llm.Message) map[string]string {
	names := make(map[string]string)
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.ID != "" && tc.Name != "" {
				names[tc.ID] = tc.Name
			}
		}
	}
	return names
}

func adjustCompactTailStart(messages []llm.Message, start int) int {
	if start <= 0 || start >= len(messages) {
		return start
	}
	for start > 0 && messages[start].Role == "tool" {
		start--
	}
	return start
}

func truncateForSummary(text string, maxTokens int) string {
	maxChars := maxTokens * 4
	if len(text) <= maxChars {
		return text
	}
	return text[:maxChars] + fmt.Sprintf("\n… truncated for summarization (%d chars total)", len(text))
}

func (m *Manager) summarizeText(ctx context.Context, segment string) (string, error) {
	if strings.TrimSpace(segment) == "" {
		return "", nil
	}

	prompt := `Summarize the conversation segment below for continuation. Preserve:
- The user's original goal and any changes to it
- Files touched and why they matter
- Key findings from tool results (errors, line numbers, search hits)
- Technical decisions made
- Errors encountered and how they were fixed
- Pending work and the current state

Be concise but keep facts the agent needs to continue without re-reading everything.
Do not invent information.

Conversation segment:
` + segment

	resp, err := m.Provider.GenerateResponse(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, nil, nil)
	if err != nil {
		return "", fmt.Errorf("context summarization failed: %w", err)
	}
	if resp.Content == "" {
		return "", fmt.Errorf("context summarization returned empty summary")
	}
	return resp.Content, nil
}

func writeMessageForSummary(b *strings.Builder, msg llm.Message, maxToolBytes int, toolNames map[string]string) {
	switch msg.Role {
	case "user":
		fmt.Fprintf(b, "USER: %s\n", msg.Content)
	case "assistant":
		if msg.Content != "" {
			fmt.Fprintf(b, "ASSISTANT: %s\n", msg.Content)
		}
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(b, "TOOL CALL: %s(%v)\n", tc.Name, tc.Args)
		}
	case "tool":
		content := msg.Content
		if maxToolBytes > 0 && len(content) > maxToolBytes {
			content = content[:maxToolBytes] + fmt.Sprintf(" …(%d chars total)", len(msg.Content))
		}
		label := msg.ToolCallID
		if name := toolNames[msg.ToolCallID]; name != "" {
			label = name + " (" + msg.ToolCallID + ")"
		}
		fmt.Fprintf(b, "TOOL RESULT (%s): %s\n", label, content)
	}
}

func cloneMessage(msg llm.Message) llm.Message {
	out := llm.Message{
		Role:       msg.Role,
		Content:    msg.Content,
		ToolCallID: msg.ToolCallID,
	}
	if len(msg.ToolCalls) > 0 {
		out.ToolCalls = make([]llm.ToolCall, len(msg.ToolCalls))
		copy(out.ToolCalls, msg.ToolCalls)
		for i := range out.ToolCalls {
			if msg.ToolCalls[i].Args != nil {
				out.ToolCalls[i].Args = make(map[string]interface{}, len(msg.ToolCalls[i].Args))
				for k, v := range msg.ToolCalls[i].Args {
					out.ToolCalls[i].Args[k] = cloneArgValue(v)
				}
			}
		}
	}
	return out
}

func cloneArgValue(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, val := range x {
			out[k] = cloneArgValue(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, val := range x {
			out[i] = cloneArgValue(val)
		}
		return out
	default:
		return v
	}
}

func cloneMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	for i, msg := range messages {
		out[i] = cloneMessage(msg)
	}
	return out
}

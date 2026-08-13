package contextmgr

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"gogen/internal/config"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
)

// SummaryPrefix marks messages produced by compaction. Summaries are stored
// as assistant-role messages today; the prefix also identifies legacy
// user-role summaries, which must never be treated as real user messages
// (firstUserIndex, PinManager).
const SummaryPrefix = "[Session summary — earlier conversation condensed]\n"
const maxSummarizeDepth = 8

// defaultMinMiddleTokens is the smallest middle (estimated tokens) worth
// summarizing. Compacting a smaller middle makes the model echo the one or
// two messages it is asked to summarize — a conversation reply, not a recap —
// so the compaction is refused instead. Manager.minMiddleTokens holds the
// effective value (tunable to 0 in tests that exercise tiny histories).
const defaultMinMiddleTokens = 500

// WarnThreshold is the fraction of the context limit at which the
// near-compact warning (web banner, statusbar color) appears. It is
// deliberately decoupled from the auto-compaction trigger (CompactThreshold)
// so the UI warns before compaction fires, giving the user lead time to
// compact manually. The TUI statusbar and the web banner both read this
// constant so the thresholds cannot drift apart.
const WarnThreshold = 0.75

// summaryInstruction is the user message appended to the summarization
// request. The request carries the wire prefix (system/enrichment messages)
// plus everything up to the compaction tail start, so the provider can serve
// the bulk of it from its prompt cache (the prefix is byte-identical to the
// last real turn). The instruction itself is a SYSTEM-role message trailing
// the conversation: a trailing user message reads as the next chat turn and
// makes the model continue the conversation instead of summarizing (it
// replied to the head question and echoed the opening messages). A trailing
// system message is a task directive, and the byte-identical prefix still
// keeps the conversation cached. The model is asked to summarize only the
// middle — the first user message (the head) and the tail are preserved
// verbatim by construction, so they are excluded here and the cut messages
// are never mentioned.
const summaryInstruction = `This is a summarization task, not a conversation turn. Do not reply to any question in the transcript above. Do not continue the conversation. Do not quote or repeat any message verbatim.

Summarize everything after the first user message in the transcript above. The first user message and any leading context before it are preserved verbatim and excluded from this summary.

Preserve:
- The user's original goal and any changes to it
- Files touched and why they matter
- Key findings from tool results (errors, line numbers, search hits)
- Technical decisions made
- Errors encountered and how they were fixed
- Pending work and the current state

Be concise but keep the facts the agent needs to continue without re-reading the summarized messages. Write a concise factual recap in the third person covering only what is in the transcript. Do not invent information. The recap will be inserted into the conversation in place of the summarized messages.

Output only the recap text — no preamble, no markdown headings, no dialogue.`

// Settings controls context window management.
type Settings struct {
	ContextLimit              int
	CompactThreshold          float64
	CompactKeepRecentMessages int
	MaxToolResultBytes        int
	CompactReserveTokens      int
}

// DefaultSettings returns defaults; ContextLimit 0 means resolve from the provider at runtime.
func DefaultSettings() Settings {
	return Settings{
		ContextLimit:              0,
		CompactThreshold:          config.DefaultCompactThreshold,
		CompactKeepRecentMessages: config.DefaultCompactKeepRecentMessages,
		MaxToolResultBytes:        config.DefaultMaxToolResultBytes,
		CompactReserveTokens:      config.DefaultCompactReserveTokens,
	}
}

// Manager builds LLM views and compacts canonical conversation history.
type Manager struct {
	Settings           Settings
	Provider           llm.LLMProvider
	minMiddleTokens    int
	mu                 sync.RWMutex
	limitResolved      bool
	manualContextLimit int
}

func NewManager(provider llm.LLMProvider, settings Settings) *Manager {
	settings = normalizeSettings(settings)
	manual := 0
	if settings.ContextLimit > 0 {
		manual = settings.ContextLimit
	}
	return &Manager{
		Settings:           settings,
		Provider:           provider,
		minMiddleTokens:    defaultMinMiddleTokens,
		manualContextLimit: manual,
	}
}

// normalizeSettings clamps invalid values to defaults. 0 is meaningful for
// these fields (see Settings docs): compact_threshold 0 disables
// auto-compaction, compact_keep_recent_messages 0 keeps no recent messages on
// compaction, max_tool_result_bytes 0 removes the truncation cap,
// compact_reserve_tokens 0 reserves no tokens. Only negative values are
// invalid and fall back to defaults.
func normalizeSettings(settings Settings) Settings {
	def := DefaultSettings()
	if settings.CompactThreshold < 0 || settings.CompactThreshold > 1 {
		settings.CompactThreshold = def.CompactThreshold
	}
	if settings.CompactKeepRecentMessages < 0 {
		settings.CompactKeepRecentMessages = def.CompactKeepRecentMessages
	}
	if settings.MaxToolResultBytes < 0 {
		settings.MaxToolResultBytes = def.MaxToolResultBytes
	}
	if settings.CompactReserveTokens < 0 {
		settings.CompactReserveTokens = def.CompactReserveTokens
	}
	return settings
}

// UpdateSettings replaces the context-management settings at runtime (the
// web settings modal). Zero values are meaningful and pass through unchanged
// (same semantics as NewManager); a ContextLimit of 0 returns to provider
// resolution. All reads are internally synchronized (m.mu), so a running
// turn sees the new settings on its next snapshot/compact check.
//
// The ContextLimit's manual/resolved distinction is preserved when the
// incoming value is UNCHANGED (the per-field settings push sends each
// session's current values back for fields it does not touch): a
// provider-resolved limit (set via SetContextLimit on restore) must not
// become a manual pin — that would stop RefreshAfterModelChange from
// re-resolving for a new model — and a manual pin must not silently become
// auto. Only an explicitly changed value re-derives the state: > 0 pins a
// manual limit, 0 returns to provider resolution.
func (m *Manager) UpdateSettings(settings Settings) {
	settings = normalizeSettings(settings)
	m.mu.Lock()
	defer m.mu.Unlock()
	limitChanged := settings.ContextLimit != m.Settings.ContextLimit
	m.Settings = settings
	if !limitChanged {
		return
	}
	m.manualContextLimit = 0
	// A manual limit is resolved by construction; 0 returns to provider
	// resolution (EnsureContextLimit re-probes on the next turn).
	m.limitResolved = settings.ContextLimit > 0
	if settings.ContextLimit > 0 {
		m.manualContextLimit = settings.ContextLimit
	}
}

// SettingsSnapshot returns a copy of the current settings (web settings
// modal display). Internally synchronized.
func (m *Manager) SettingsSnapshot() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings
}

// RefreshAfterModelChange updates the context limit for the newly selected model.
// Provider I/O runs without holding m.mu so Snapshot/ContextStats are not stalled
// behind a slow /v1/models call.
func (m *Manager) RefreshAfterModelChange(ctx context.Context) {
	m.mu.Lock()
	if m.manualContextLimit > 0 {
		m.Settings.ContextLimit = m.manualContextLimit
		m.limitResolved = true
		m.mu.Unlock()
		return
	}
	m.Settings.ContextLimit = 0
	m.limitResolved = false
	m.mu.Unlock()
	m.resolveContextLimit(ctx)
}

// EnsureContextLimit resolves ContextLimit from the provider when not set explicitly.
// A positive Settings.ContextLimit from GOGEN_CONTEXT_LIMIT is a manual override and
// skips provider lookup; RefreshAfterModelChange preserves that override.
// Provider I/O runs without holding m.mu.
func (m *Manager) EnsureContextLimit(ctx context.Context) {
	m.mu.Lock()
	if m.Settings.ContextLimit > 0 && m.limitResolved {
		m.mu.Unlock()
		return
	}
	if m.Settings.ContextLimit > 0 {
		m.limitResolved = true
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.resolveContextLimit(ctx)
}

// resolveContextLimit fetches the provider limit without holding m.mu, then
// stores the result. Concurrent resolvers are safe: the first successful store
// wins unless a manual override lands mid-flight.
func (m *Manager) resolveContextLimit(ctx context.Context) {
	limit := config.DefaultContextLimit
	if m.Provider != nil {
		if n, err := m.Provider.ModelContextLimit(ctx); err == nil && n > 0 {
			limit = n
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.manualContextLimit > 0 {
		m.Settings.ContextLimit = m.manualContextLimit
		m.limitResolved = true
		return
	}
	if m.limitResolved && m.Settings.ContextLimit > 0 {
		return
	}
	m.Settings.ContextLimit = limit
	m.limitResolved = true
}

// ContextLimit returns the resolved context window size.
func (m *Manager) ContextLimit() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.ContextLimit
}

// CompactBudget returns the token budget at which auto-compaction triggers
// (CompactThreshold fraction of the context limit, minus the reserve). The
// caller can compare a cached token total against this to avoid re-tokenizing
// the whole conversation.
func (m *Manager) CompactBudget() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.compactBudgetLocked()
}

// CompactKeepRecentMessages returns how many recent messages are preserved
// verbatim during compaction.
func (m *Manager) CompactKeepRecentMessages() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.CompactKeepRecentMessages
}

// AutoCompactEnabled reports whether automatic compaction is enabled
// (compact_threshold > 0). A threshold of 0 disables auto-compaction; manual
// compaction via Compact/CompactHistory is unaffected.
func (m *Manager) AutoCompactEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings.CompactThreshold > 0
}

// SetContextLimit sets the context window size directly, bypassing provider
// resolution. Used when restoring a session snapshot so the limit is available
// synchronously before the async provider refresh completes.
func (m *Manager) SetContextLimit(limit int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Settings.ContextLimit = limit
	m.limitResolved = true
}

const toolResultTruncationMarker = "\n… truncated ("

// TruncateToolResult caps tool output stored in canonical history / LLM views.
func (m *Manager) TruncateToolResult(content string) string {
	m.mu.RLock()
	max := m.Settings.MaxToolResultBytes
	m.mu.RUnlock()
	return truncateToolResult(content, max)
}

// TruncateRuneSafe cuts s to at most max bytes without splitting a UTF-8
// rune: it backs off over continuation bytes until it lands on a rune
// boundary. s is assumed valid UTF-8 (tool results that pass through JSON
// decoding always are); for invalid input the result is never worse than a
// raw byte cut. Shared by the context window capper and the web server's
// per-frame tool-result cap so both produce valid UTF-8 output.
func TruncateRuneSafe(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// truncateToolResult caps content to max bytes and appends a truncation
// marker. The marker's length is reserved up front so the capped result fits
// within max bytes (the previous behavior returned max + marker, exceeding
// the budget). When max is smaller than the marker itself, the marker is
// omitted rather than exceeding the cap; the result is then exactly max
// bytes and will not be re-truncated on a later pass.
func truncateToolResult(content string, max int) string {
	if max <= 0 || len(content) <= max {
		return content
	}
	if strings.Contains(content, toolResultTruncationMarker) {
		return content
	}
	marker := fmt.Sprintf("\n… truncated (%d chars total)", len(content))
	if len(marker) >= max {
		return TruncateRuneSafe(content, max)
	}
	return TruncateRuneSafe(content, max-len(marker)) + marker
}

// ContextSnapshot summarizes context window usage for display.
type ContextSnapshot struct {
	Limit           int
	Used            int
	Stored          int
	CompactAt       int
	MessageCount    int
	ToolTruncated   bool
	CompactDisabled bool // auto-compaction disabled (compact_threshold = 0)
	NearCompact     bool
	// WarnNearCompact is true when usage reached the warning point
	// (min(75% of the limit, CompactAt)) and auto-compaction is enabled.
	// Unlike NearCompact it tracks the warning threshold, not the trigger.
	WarnNearCompact bool
	Percent         float64
}

// Snapshot estimates token usage for canonical history and the LLM view.
func (m *Manager) Snapshot(canonical, llmView []llm.Message) ContextSnapshot {
	stored := m.EstimateTokens(canonical)
	return m.snapshot(canonical, llmView, stored)
}

// SnapshotWithCounts is like Snapshot but uses pre-computed token counts for
// the canonical messages, avoiding re-tokenization. The counts slice must
// correspond 1:1 to canonical messages (not including system prompt). This
// is significantly faster for large restored sessions where the token counts
// were persisted alongside the messages.
func (m *Manager) SnapshotWithCounts(canonical, llmView []llm.Message, canonicalCounts []int) ContextSnapshot {
	stored := 0
	for _, c := range canonicalCounts {
		stored += c
	}
	return m.snapshot(canonical, llmView, stored)
}

// snapshot is the shared implementation for Snapshot and SnapshotWithCounts.
// stored is the pre-computed token count for canonical messages.
func (m *Manager) snapshot(canonical, llmView []llm.Message, stored int) ContextSnapshot {
	m.mu.RLock()
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = config.DefaultContextLimit
	}
	compactAt := m.compactBudgetLocked()
	m.mu.RUnlock()

	// Estimate tokens for system prompt / enrichment messages (the prefix
	// in llmView that is not part of canonical). This is typically 1 message
	// and very cheap to tokenize compared to the full history.
	sysTokens := 0
	if n := len(llmView) - len(canonical); n > 0 {
		sysTokens = m.EstimateTokens(llmView[:n])
	}
	used := stored + sysTokens

	warnAt := 0
	if compactAt > 0 {
		warnAt = int(float64(limit) * WarnThreshold)
		if warnAt > compactAt {
			warnAt = compactAt // small windows: warn exactly when compaction would trigger
		}
	}
	return m.buildSnapshot(limit, compactAt, warnAt, stored, used, len(canonical), hasTruncatedToolResults(canonical))
}

// buildSnapshot creates a ContextSnapshot from computed values.
func (m *Manager) buildSnapshot(limit, compactAt, warnAt, stored, used, msgCount int, truncated bool) ContextSnapshot {
	snap := ContextSnapshot{
		Limit:           limit,
		Used:            used,
		Stored:          stored,
		CompactAt:       compactAt,
		MessageCount:    msgCount,
		ToolTruncated:   truncated,
		CompactDisabled: compactAt <= 0,
		NearCompact:     compactAt > 0 && used >= compactAt,
		WarnNearCompact: compactAt > 0 && used >= warnAt,
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

func (m *Manager) compactBudgetLocked() int {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = config.DefaultContextLimit
	}
	if m.Settings.CompactThreshold <= 0 {
		return 0 // auto-compaction disabled
	}
	budget := int(float64(limit) * m.Settings.CompactThreshold)
	budget -= m.Settings.CompactReserveTokens
	if budget < 1000 {
		budget = 1000
	}
	return budget
}

// ShouldCompact reports whether messages exceed the compaction threshold.
// EstimateTokens computes fresh each call — safe to call every turn.
func (m *Manager) ShouldCompact(messages []llm.Message) bool {
	m.mu.RLock()
	keep := m.Settings.CompactKeepRecentMessages
	budget := m.compactBudgetLocked()
	m.mu.RUnlock()
	if budget <= 0 {
		return false // auto-compaction disabled
	}
	if len(messages) <= keep+1 {
		return false
	}
	return m.EstimateTokens(messages) >= budget
}

// EnsureToolResultsCapped mutates messages so every tool body fits MaxToolResultBytes.
// Safe to call every turn; only rewrites oversized bodies (one-time sticky rewrite).
func (m *Manager) EnsureToolResultsCapped(messages []llm.Message) bool {
	m.mu.RLock()
	max := m.Settings.MaxToolResultBytes
	m.mu.RUnlock()
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
		messages[i].Content = truncateToolResult(messages[i].Content, max)
		changed = true
	}
	return changed
}

// Compact replaces the middle of canonical history with an LLM-generated summary.
// It preserves the first user message and the most recent CompactKeepRecentMessages entries.
func (m *Manager) Compact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
	out, _, err := m.CompactPinned(ctx, nil, messages, nil, nil)
	return out, err
}

// CompactPinned is like Compact but keeps pinned message indices in the
// preserved tail and returns remapped pin indices for the compacted history.
// viewPrefix carries the system/enrichment messages that precede canonical
// history on the wire (nil when none). It is prepended to the summarization
// request so the conversation prefix stays byte-identical to the last turn
// and the provider's prompt cache covers the bulk of the request.
//
// counts, when non-nil, carries per-message token counts for a prefix of
// messages (the agent's cached counts). When it covers the region being
// compacted, the summarization request is sized from those counts instead of
// re-tokenizing the middle with the tokenizer — the most expensive part of a
// compaction on large histories.
func (m *Manager) CompactPinned(ctx context.Context, viewPrefix, messages []llm.Message, counts []int, pinned map[int]struct{}) ([]llm.Message, map[int]struct{}, error) {
	m.mu.RLock()
	keep := m.Settings.CompactKeepRecentMessages
	m.mu.RUnlock()
	if len(messages) <= keep+1 {
		return messages, copyIntSet(pinned), nil
	}

	headIdx, ok := firstUserIndex(messages)
	if !ok {
		// No user message to preserve as the head; compacting would drop
		// messages without anything to anchor the summary.
		return messages, copyIntSet(pinned), nil
	}
	tailStart := adjustCompactTailStart(messages, len(messages)-keep)
	tailStart = extendTailForPins(messages, headIdx, tailStart, pinned)
	if tailStart <= headIdx+1 {
		return messages, copyIntSet(pinned), nil
	}

	oldTailStart := tailStart
	head := []llm.Message{cloneMessage(messages[headIdx])}
	middle := messages[headIdx+1 : tailStart]
	tail := cloneMessages(messages[tailStart:])

	// Cached per-message counts (a prefix of messages) let the middle be
	// sized without re-tokenizing it. knownTokens covers everything from
	// messages[0] up to the tail start — the entire messages-derived portion
	// of the summarization request (pre-head, first user message, middle) —
	// and falls back to -1 (unknown) when the cache does not reach tailStart.
	knownTokens := -1
	if counts != nil && len(counts) >= tailStart {
		knownTokens = 0
		for i := 0; i < tailStart; i++ {
			knownTokens += counts[i]
		}
	}

	// Refuse to summarize a trivially small middle: asking a model to recap
	// one or two short messages produces an echo of those messages (a
	// conversation reply) rather than a summary — worse than not compacting.
	middleTokens := -1
	if counts != nil && len(counts) >= tailStart {
		middleTokens = 0
		for i := headIdx + 1; i < tailStart; i++ {
			middleTokens += counts[i]
		}
	}
	if middleTokens < 0 {
		middleTokens = m.EstimateTokens(middle)
	}
	if middleTokens < m.minMiddleTokens {
		return nil, nil, fmt.Errorf("not enough history to compact (%d messages in the middle)", len(middle))
	}

	// The summarization request carries the wire prefix (viewPrefix), the
	// messages before the middle (pre-head + the preserved first user
	// message), and the middle to summarize; the instruction tells the model
	// to summarize only the middle — head and tail are preserved verbatim by
	// construction below.
	summary, err := m.summarizeMiddle(ctx, viewPrefix, messages[:headIdx+1], middle, knownTokens)
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
		Content: SummaryPrefix + summary,
	})
	compact = append(compact, tail...)

	newPinned := remapPinsAfterCompact(pinned, headIdx, oldTailStart, len(compact)-len(tail))
	// The token cache is keyed by content fingerprint (not pointer), so
	// the newly allocated copies of preserved messages still hit the same
	// cache entries. Old entries for summarised-away middle messages are
	// harmless (bounded by session size and valid for their key).
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

// IsCompactionSummary reports whether content is a compaction summary
// message. Summaries are stored as assistant-role messages today; the check
// also covers legacy user-role summaries, which must not be treated as real
// user messages (firstUserIndex, PinManager).
func IsCompactionSummary(content string) bool {
	return strings.HasPrefix(content, SummaryPrefix)
}

// firstUserIndex returns the index of the first real user message (skipping
// compaction summaries — stored as assistant-role messages prefixed with
// SummaryPrefix today, user-role in legacy sessions) and whether one exists.
// ok is false when the conversation has no user message at all — callers
// must not compact in that case.
func firstUserIndex(messages []llm.Message) (int, bool) {
	for i, msg := range messages {
		if msg.Role == "user" && !IsCompactionSummary(msg.Content) {
			return i, true
		}
	}
	return 0, false
}

// summarizeMiddle produces a summary of the middle of the conversation.
// viewPrefix is the wire context that precedes canonical history
// (system/enrichment messages) and prefix is messages[:headIdx+1] (pre-head
// plus the preserved first user message); the preferred path sends
// viewPrefix + prefix + middle + summaryInstruction as one request so the
// provider prompt cache covers the conversation prefix. When the request
// would not fit in the context window, it falls back to the legacy
// flattened-text recursive summarization.
//
// knownTokens, when non-negative, is the cached token count of prefix+middle
// (the messages-derived portion of the request); only the wire prefix and
// the instruction are then tokenized fresh, avoiding a full re-tokenization
// of the middle on every compaction.
func (m *Manager) summarizeMiddle(ctx context.Context, viewPrefix, prefix, middle []llm.Message, knownTokens int) (string, error) {
	if len(middle) == 0 {
		return "", nil
	}
	m.mu.RLock()
	budget := m.summaryRequestBudgetLocked()
	m.mu.RUnlock()

	instruction := llm.Message{Role: "system", Content: summaryInstruction}
	req := make([]llm.Message, 0, len(viewPrefix)+len(prefix)+len(middle)+1)
	// Summarization requests must not carry user images: the summary model
	// may not support vision, and image bytes are irrelevant to a recap.
	req = append(req, stripImages(viewPrefix)...)
	req = append(req, stripImages(prefix)...)
	req = append(req, stripImages(middle)...)
	// Trailing SYSTEM-role instruction: a task directive, not a chat turn
	// (a trailing user message made models continue the conversation instead
	// of summarizing). The conversation prefix stays byte-identical, so the
	// provider prompt cache still covers the bulk of the request.
	req = append(req, instruction)
	// Count the request exactly once and reuse the count for the budget
	// check and both debuglog entries. (The map literal passed to
	// debuglog.Write is evaluated eagerly even when debug logging is off, so
	// a second EstimateTokens call here would duplicate the full
	// tokenization of the conversation prefix on every compaction.)
	reqTokens := knownTokens
	if reqTokens < 0 {
		reqTokens = m.EstimateTokens(req)
	} else {
		// knownTokens covers prefix+middle (cached counts exclude image
		// estimates, matching the stripped request); only the wire prefix
		// and the one-message instruction need fresh counts.
		reqTokens += m.EstimateTokens(stripImages(viewPrefix))
		reqTokens += m.EstimateTokens([]llm.Message{instruction})
	}
	if reqTokens <= budget {
		debuglog.Write("contextmgr/summarize", "continuation-summary request", "", map[string]interface{}{
			"path":           "primary",
			"middleMessages": len(middle),
			"requestTokens":  reqTokens,
			"budget":         budget,
		})
		return m.summarizeRequest(ctx, req)
	}
	debuglog.Write("contextmgr/summarize", "summary request exceeds window; using flattened-text fallback", "", map[string]interface{}{
		"path":           "fallback",
		"middleMessages": len(middle),
		"requestTokens":  reqTokens,
		"budget":         budget,
	})
	return m.summarizeMessagesDepth(ctx, middle, 0)
}

// stripImages returns a copy of msgs with every user message's images
// removed, leaving all other fields (and the original slice) untouched.
// Only the summarization request is affected: stripping images from the
// request prefix breaks the byte-identical prompt-cache prefix only for
// sessions that attach images; the main conversation requests keep their
// images and their cache behavior.
func stripImages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		m.Images = nil
		out[i] = m
	}
	return out
}

// summaryRequestBudgetLocked is the token budget for the continuation-summary
// request: the full context window minus the post-compaction reserve. Unlike
// maxSummaryInputTokensLocked (which budgets the flattened middle text for the
// legacy path), this leaves room for the summary output while keeping the
// request under the window. Callers must hold m.mu (R or W).
func (m *Manager) summaryRequestBudgetLocked() int {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = config.DefaultContextLimit
	}
	budget := limit - m.Settings.CompactReserveTokens
	if budget < 2000 {
		budget = 2000
	}
	return budget
}

func (m *Manager) summarizeMessagesDepth(ctx context.Context, messages []llm.Message, depth int) (string, error) {
	if depth >= maxSummarizeDepth {
		m.mu.RLock()
		maxTool := m.Settings.MaxToolResultBytes
		maxIn := m.maxSummaryInputTokensLocked()
		m.mu.RUnlock()
		text := renderMessagesForSummary(messages, maxTool)
		return truncateForSummary(text, maxIn), nil
	}
	return m.summarizeMessages(ctx, messages, depth)
}

func (m *Manager) summarizeMessages(ctx context.Context, messages []llm.Message, depth int) (string, error) {
	m.mu.RLock()
	maxTool := m.Settings.MaxToolResultBytes
	maxIn := m.maxSummaryInputTokensLocked()
	m.mu.RUnlock()
	text := renderMessagesForSummary(messages, maxTool)
	if m.EstimateTokens([]llm.Message{{Content: text}}) <= maxIn {
		return m.summarizeText(ctx, text)
	}
	if len(messages) == 1 {
		return m.summarizeText(ctx, truncateForSummary(text, maxIn))
	}

	mid := len(messages) / 2
	left, err := m.summarizeMessagesDepth(ctx, messages[:mid], depth+1)
	if err != nil {
		return "", err
	}
	right, err := m.summarizeMessagesDepth(ctx, messages[mid:], depth+1)
	if err != nil {
		return "", err
	}
	merged := "Earlier segment summary:\n" + left + "\n\nLater segment summary:\n" + right
	if m.EstimateTokens([]llm.Message{{Content: merged}}) <= maxIn {
		return m.summarizeText(ctx, merged)
	}
	return merged, nil
}

func (m *Manager) maxSummaryInputTokensLocked() int {
	limit := m.Settings.ContextLimit
	if limit <= 0 {
		limit = config.DefaultContextLimit
	}
	budget := limit/2 - m.Settings.CompactReserveTokens
	if budget < 2000 {
		budget = 2000
	}
	return budget
}

// summarizeRequest calls the provider with the full continuation-summary
// request (wire prefix + middle + summaryInstruction) and extracts the
// summary text from the response.
func (m *Manager) summarizeRequest(ctx context.Context, request []llm.Message) (string, error) {
	resp, err := m.Provider.GenerateResponse(ctx, request, nil, nil)
	if err != nil {
		return "", fmt.Errorf("context summarization failed: %w", err)
	}
	summary := resp.Content
	if summary == "" {
		summary = resp.Refusal
	}
	if summary == "" {
		// Some local models only emit reasoning for short completions.
		summary = resp.Reasoning
	}
	if summary == "" {
		return "", fmt.Errorf("context summarization returned empty summary")
	}
	return summary, nil
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

func (m *Manager) summarizeText(ctx context.Context, segment string) (string, error) {
	if strings.TrimSpace(segment) == "" {
		return "", nil
	}

	prompt := `This is a summarization task, not a conversation turn. Do not reply to the segment below, do not continue the conversation, and do not quote it verbatim.

Summarize the conversation segment below. Preserve:
- The user's original goal and any changes to it
- Files touched and why they matter
- Key findings from tool results (errors, line numbers, search hits)
- Technical decisions made
- Errors encountered and how they were fixed
- Pending work and the current state

Be concise but keep facts the agent needs to continue without re-reading everything.
Do not invent information. Output only the summary text.

Conversation segment:
` + segment

	resp, err := m.Provider.GenerateResponse(ctx, []llm.Message{
		{Role: "system", Content: prompt},
	}, nil, nil)
	if err != nil {
		return "", fmt.Errorf("context summarization failed: %w", err)
	}
	summary := resp.Content
	if summary == "" {
		summary = resp.Refusal
	}
	if summary == "" {
		// Some local models only emit reasoning for short completions.
		summary = resp.Reasoning
	}
	if summary == "" {
		return "", fmt.Errorf("context summarization returned empty summary")
	}
	return summary, nil
}

func writeMessageForSummary(b *strings.Builder, msg llm.Message, maxToolBytes int, toolNames map[string]string) {
	switch msg.Role {
	case "user":
		fmt.Fprintf(b, "USER: %s\n", msg.Content)
	case "assistant":
		if msg.Content != "" {
			fmt.Fprintf(b, "ASSISTANT: %s\n", msg.Content)
		} else if msg.Refusal != "" {
			fmt.Fprintf(b, "ASSISTANT REFUSAL: %s\n", msg.Refusal)
		}
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(b, "TOOL CALL: %s(%v)\n", tc.Name, tc.Args)
		}
	case "tool":
		content := msg.Content
		if maxToolBytes > 0 && len(content) > maxToolBytes {
			content = TruncateRuneSafe(content, maxToolBytes) + fmt.Sprintf(" …(%d chars total)", len(msg.Content))
		}
		label := msg.ToolCallID
		if name := toolNames[msg.ToolCallID]; name != "" {
			label = name + " (" + msg.ToolCallID + ")"
		}
		fmt.Fprintf(b, "TOOL RESULT (%s): %s\n", label, content)
	}
}

// cloneMessage deep-copies the persisted parts of a message so compaction can
// preserve them verbatim. Images (vision input), CreatedAt, and Model are
// value fields that must survive: stripping them from the preserved head/tail
// would silently drop image context after the first compaction and wipe
// assistant model attribution (web UI chips) from preserved bubbles.
func cloneMessage(msg llm.Message) llm.Message {
	out := llm.Message{
		Role:       msg.Role,
		Content:    msg.Content,
		Reasoning:  msg.Reasoning,
		Refusal:    msg.Refusal,
		ToolCallID: msg.ToolCallID,
		CreatedAt:  msg.CreatedAt,
		Model:      msg.Model,
	}
	if len(msg.Images) > 0 {
		out.Images = make([]llm.ImageInput, len(msg.Images))
		copy(out.Images, msg.Images)
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

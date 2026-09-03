package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"sync"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// baselineState is a consistent snapshot of the per-message token cache and
// the provider prompt_tokens baseline, taken under a single statsMu read
// lock so both compaction checks agree on the same conversation state.
type baselineState struct {
	msgs             []llm.Message
	counts           []int
	complete         bool
	baselineFresh    bool
	baselineTokens   int
	baselineMsgCount int
}

// snapshotBaselineState captures the per-message token cache and the
// provider prompt_tokens baseline under statsMu. The baseline is fresh when
// the provider's prompt_tokens were recorded for a list no larger than the
// current one (the list only grew since that request). A shrunken list
// (rollback) leaves the baseline stale — the API count would over-report, so
// the local estimate is used instead.
func (a *Agent) snapshotBaselineState() baselineState {
	a.statsMu.RLock()
	counts := a.tokenCounts
	msgs := a.Messages
	baselineTokens, baselineMsgCount := a.apiBaselinePromptTokens, a.apiBaselineMsgCount
	a.statsMu.RUnlock()
	return baselineState{
		msgs:             msgs,
		counts:           counts,
		complete:         counts != nil && len(counts) == len(msgs),
		baselineFresh:    baselineTokens > 0 && baselineMsgCount > 0 && baselineMsgCount <= len(msgs),
		baselineTokens:   baselineTokens,
		baselineMsgCount: baselineMsgCount,
	}
}

// shouldCompactUsingCounts reports whether auto-compaction should trigger,
// summing the cached per-message token counts when they are complete to avoid
// re-tokenizing the whole conversation on every turn. Falls back to
// Manager.ShouldCompactWithOverhead (a full EstimateTokens pass) when the
// cache is empty or incomplete (e.g. right after a compaction or session
// restore).
//
// Wire overhead accounting: the per-message counts cover the canonical
// messages only — the system prompt and tool definitions (10-30k tokens)
// ride on every request but are not in a.Messages. When the provider
// prompt_tokens baseline is fresh, it already includes that overhead, so it
// must NOT be added again (double-count trap). When the baseline is absent
// (post-compaction, post-restore, first turn), the wire overhead is added so
// the trigger fires on time instead of tens of thousands of tokens late.
func (a *Agent) shouldCompactUsingCounts() bool {
	if a.Context == nil {
		return false
	}
	snap := a.snapshotBaselineState()
	msgs := snap.msgs
	if !snap.complete {
		overhead := 0
		if !snap.baselineFresh {
			overhead = a.wireOverheadTokens()
		}
		return a.Context.ShouldCompactWithOverhead(msgs, overhead)
	}
	if !a.Context.AutoCompactEnabled() {
		return false
	}
	if len(msgs) <= a.Context.CompactKeepRecentMessages()+1 {
		return false
	}
	return a.compactionTokenTotal() >= a.Context.CompactBudget()
}

// compactionTokenTotal returns the estimated total token count of the
// conversation (system prompt + tool definitions + canonical messages),
// using the same accounting as shouldCompactUsingCounts: the provider's
// exact prompt_tokens baseline when fresh, otherwise the cached per-message
// counts plus the wire overhead. Returns -1 when the per-message count
// cache is incomplete (e.g. right after a session restore) so callers can
// fall back to the full-estimate path.
func (a *Agent) compactionTokenTotal() int {
	snap := a.snapshotBaselineState()
	if !snap.complete {
		return -1
	}
	total := 0
	for _, c := range snap.counts {
		total += c
	}
	// Use the provider's exact prompt_tokens for messages in the last request
	// (cl100k misjudges other tokenizers); estimate only the suffix appended
	// since. The provider count already includes the system prompt and tool
	// definitions, so the wire overhead is NOT added again (double-count
	// trap). Cleared by clearTurnUsage after a compaction.
	if snap.baselineFresh {
		local := 0
		for _, c := range snap.counts[snap.baselineMsgCount:] {
			local += c
		}
		total = snap.baselineTokens + local
	} else {
		// Baseline absent (post-compaction, post-restore, first turn): the
		// local counts cover the canonical messages only, so add the wire
		// overhead (system prompt + tool definitions) they omit.
		total += a.wireOverheadTokens()
	}
	return total
}

// wireOverheadTokens returns the estimated wire token cost of everything a
// provider request carries besides the canonical messages: the system prompt
// (or only its enrichment suffix when the history already carries a system
// message, whose base content is in the per-message counts) plus all tool
// definitions. The result is cached and recomputed only when the content
// fingerprint of (system prompt, tool definitions) changes, so the
// per-round prepareMessages call does not re-tokenize the tool set.
func (a *Agent) wireOverheadTokens() int {
	var sysContent string
	if prefix := a.systemPromptPrefix(); prefix != nil {
		sysContent = prefix[0].Content
	} else {
		// History carries a system message: only the enrichment suffix is
		// wire overhead (the base content is counted in the messages).
		sysContent = buildSystemSuffix(a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode)
	}
	tools := a.llmTools()
	fp := overheadFingerprint(sysContent, tools)
	a.overheadMu.Lock()
	if fp != a.overheadFingerprint {
		tokens := contextmgr.EstimateToolTokens(tools)
		if sysContent != "" && a.Context != nil {
			tokens += a.Context.EstimateTokens([]llm.Message{{Role: "system", Content: sysContent}})
		}
		a.overheadFingerprint = fp
		a.overheadTokens = tokens
	}
	t := a.overheadTokens
	a.overheadMu.Unlock()
	return t
}

// overheadFingerprint builds a content fingerprint of (system prompt, tool
// definitions) for the wire-overhead cache: any change to the system prompt
// text or any tool definition produces a different fingerprint, invalidating
// the cached token estimate. Length-prefixed segments make the hash
// collision-resistant against concatenation ambiguity.
func overheadFingerprint(sysContent string, tools []llm.Tool) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d\x00", len(sysContent))
	h.Write([]byte(sysContent))
	for _, t := range tools {
		s := contextmgr.ToolDefinitionString(t)
		fmt.Fprintf(h, "%d\x00", len(s))
		h.Write([]byte(s))
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// compactAttemptDue reports whether the auto-compaction failure backoff has expired.
func (a *Agent) compactAttemptDue() bool {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return time.Now().After(a.compactBackoffUntil)
}

// emergencySummaryAllowance is the token budget assumed for the summary
// output when sizing the emergency compaction middle: the middle must be
// large enough to save (total - hardLimit) PLUS this much, so the summary
// itself still lands the conversation under the window.
const emergencySummaryAllowance = 2000

// emergencyCompactDue reports whether the EMERGENCY compaction tier should
// fire: the conversation total has reached the hard window
// (ContextLimit - CompactReserveTokens, where the provider would refuse the
// request) and the progress guard allows a new attempt. Unlike the normal
// tier it ignores the failure backoff — a provider refusal is worse than a
// redundant summarization call — but it refuses to retry a compaction that
// already failed at a message count >= the current one
// (compactionGuards.emergency), so a permanently broken summarization path
// cannot hot-loop an expensive failure on every turn. It also requires at
// least 3 messages: with fewer, there is nothing between the starting
// prompt and the current one to summarize. total < 0 (incomplete count
// cache) never fires: the normal tier's full-estimate fallback covers that
// state.
func (a *Agent) emergencyCompactDue(total int) bool {
	if a.Context == nil || total < 0 {
		return false
	}
	hardLimit := a.Context.HardLimit()
	if hardLimit <= 0 || total < hardLimit {
		return false
	}
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	if len(a.Messages) < 3 {
		return false
	}
	return a.compactionGuards.emergency < len(a.Messages)
}

// noteProgressFailure records a failed compaction-tier attempt at the
// current message count: the tier's progress guard suppresses further
// attempts until the conversation grows past that count (a retry at the
// same count would hit the same failure).
func (a *Agent) noteProgressFailure(guard *int) {
	a.statsMu.Lock()
	if n := len(a.Messages); n > *guard {
		*guard = n
	}
	a.statsMu.Unlock()
}

// noteProgressSuccess resets one compaction tier's progress guard. The
// caller passes the specific tier: the reset semantics are per-tier
// (emergency is reset on any successful compaction, lastResort only on a
// successful condensation), so this is never a blanket reset.
func (a *Agent) noteProgressSuccess(guard *int) {
	a.statsMu.Lock()
	*guard = 0
	a.statsMu.Unlock()
}

// noteCompactFailure records a failed auto-compaction and backs off the next attempt exponentially.
func (a *Agent) noteCompactFailure(err error) {
	log.Printf("auto-compaction failed (backing off): %v", err)
	a.statsMu.Lock()
	if a.compactBackoffDelay == 0 {
		a.compactBackoffDelay = 30 * time.Second
	} else {
		a.compactBackoffDelay *= 2
		if a.compactBackoffDelay > 10*time.Minute {
			a.compactBackoffDelay = 10 * time.Minute
		}
	}
	a.compactBackoffUntil = time.Now().Add(a.compactBackoffDelay)
	a.statsMu.Unlock()
}

// noteCompactSuccess resets the auto-compaction failure backoff and the
// emergency-tier progress guard (NOT the last-resort guard: a successful
// compaction says nothing about a previously failed condensation).
func (a *Agent) noteCompactSuccess() {
	a.statsMu.Lock()
	a.compactBackoffUntil = time.Time{}
	a.compactBackoffDelay = 0
	a.compactionGuards.emergency = 0
	a.statsMu.Unlock()
}

// CompactHistory manually compacts conversation history at a task boundary.
func (a *Agent) CompactHistory(ctx context.Context) error {
	if a.Context == nil {
		return fmt.Errorf("context management is not configured")
	}
	if len(a.Messages) <= a.Context.CompactKeepRecentMessages()+1 {
		return fmt.Errorf("not enough history to compact (%d messages)", len(a.Messages))
	}
	a.statsMu.RLock()
	cachedCounts := append([]int(nil), a.tokenCounts...)
	a.statsMu.RUnlock()
	compacted, newPins, err := a.Context.Compact(ctx, a.Messages, contextmgr.CompactOptions{
		ViewPrefix: a.systemPromptPrefix(),
		Counts:     cachedCounts,
		Pinned:     pinnedSet(a.PinManager),
	})
	if err != nil {
		return err
	}
	// Publish the compacted history together with its freshly computed token
	// counts (cheap — compaction shrank the conversation) so the cached
	// shouldCompactUsingCounts path stays valid on the next turn.
	counts := make([]int, len(compacted))
	for i, m := range compacted {
		counts[i] = contextmgr.ComputeMessageTokens(m)
	}
	a.replaceMessagesWithCounts(compacted, counts)
	if a.PinManager != nil {
		a.PinManager.ReplacePins(newPins)
	}
	// lastTurnUsage is no longer representative after compaction.
	a.clearTurnUsage()
	a.resetSaveTracking()
	return nil
}

// compactionState groups the auto-compaction bookkeeping: the failure
// backoff, the per-tier progress guards, and the cached wire-overhead token
// estimate (overheadMu).
type compactionState struct {
	// Auto-compaction failure backoff (guarded by statsMu): doubles per
	// failure (30s → 10min cap), reset on success.
	compactBackoffUntil time.Time
	compactBackoffDelay time.Duration

	// compactionGuards records, per compaction tier, the message count at
	// which the last attempt failed (guarded by statsMu). An attempt is
	// suppressed while the message count has not grown past the recorded
	// count (the progress guard against a hot loop of identical failures).
	// The RESET semantics are per-tier and deliberately asymmetric:
	// emergency is reset on ANY successful compaction (noteCompactSuccess),
	// lastResort only on a successful last-resort condensation
	// (lastResortCondense). Never reset both at once.
	compactionGuards struct {
		emergency  int
		lastResort int
	}

	// overheadMu guards overheadFingerprint/overheadTokens: the cached wire
	// overhead (system prompt + tool definitions) in tokens. Recomputed only
	// when the content fingerprint of (system prompt, tool definitions)
	// changes, so the per-round prepareMessages call never re-tokenizes the
	// tool set. Deliberately not statsMu: tokenizing is CPU-heavy and the
	// cache is consulted only on the turn goroutine (shouldCompactUsingCounts),
	// not by lock-free readers.
	overheadMu          sync.Mutex
	overheadFingerprint string
	overheadTokens      int
}

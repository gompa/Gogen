package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// llmTools returns the tool definitions exposed to the model: the built-in
// tools, the feature-gated tools from the single gating policy
// (featureTools), plus any registered MCP server tools. Feature tools are
// appended conditionally (MCP-style) so they have zero registry trace when
// their feature is off.
//
// A registered MCP tool whose name collides with a builtin or feature tool
// SHADOWS it here (its definition is the one the model sees): executeTool
// prefers the registry on name collisions too (featureToolFor), so what the
// model sees and what actually executes always agree — and the model never
// sees duplicate definitions for one name (several APIs reject those).
// Two properties below are load-bearing for provider prompt caching: tool
// definitions lead the request prefix, so any change to this list invalidates
// the whole conversation rather than a tail slice.
//
//   - Nested MCP mode inlines nothing: the single `mcp` gateway tool (from
//     featureTools) is the entire MCP surface, so the list size and its bytes
//     do not depend on how many tools the servers publish or on whether they
//     have connected yet.
//   - A shadowing MCP tool REPLACES the builtin at its own index. Removing the
//     builtin and appending the MCP definition at the end (the previous
//     behavior) shifted every tool before it, so one name collision reordered
//     the whole prefix.
func (a *Agent) llmTools() []llm.Tool {
	var mcpDefs []llm.Tool
	if a.MCPRegistry != nil && !a.MCPNested() {
		mcpDefs = a.MCPRegistry.Definitions()
	}
	// Replacement map: MCP definition by the name it shadows. Consumed as the
	// list is built, so the final loop only appends genuinely new tools.
	replacements := make(map[string]llm.Tool, len(mcpDefs))
	for _, t := range mcpDefs {
		replacements[t.Name] = t
	}
	take := func(name string) (llm.Tool, bool) {
		t, ok := replacements[name]
		if ok {
			delete(replacements, name)
		}
		return t, ok
	}

	features := a.featureTools()
	tools := make([]llm.Tool, 0, len(BuiltinTools())+len(features)+len(mcpDefs))
	for _, t := range BuiltinTools() {
		if replacement, shadowed := take(t.Name); shadowed {
			tools = append(tools, replacement) // same index, MCP schema
			continue
		}
		tools = append(tools, t)
	}
	for _, ft := range features {
		if replacement, shadowed := take(ft.Name); shadowed {
			tools = append(tools, replacement)
			continue
		}
		tools = append(tools, ft.Definition)
	}
	// Non-shadowing server tools keep the registry's (sorted) order and are
	// appended after the builtins, so adding a server never reorders what
	// precedes them.
	for _, t := range mcpDefs {
		if _, pending := replacements[t.Name]; pending {
			tools = append(tools, t)
		}
	}
	return tools
}

// prepareMessages builds the LLM view for the next round, compacting history
// at conversation boundaries when auto-compaction triggers. h carries the
// stream handlers for the round; OnCompacting fires before the summarization
// call so the UI can show compaction progress. It returns an error when the
// pre-flight check cannot make the request fit and the last-resort mode is
// "error" (compact_last_resort=error, Phase 0e): the actionable diagnostic
// is returned instead of letting the provider refuse the request.
func (a *Agent) prepareMessages(ctx context.Context, h *llm.StreamHandlers) ([]llm.Message, error) {
	var view []llm.Message
	if a.Context == nil {
		view = a.Messages
	} else {
		a.Context.EnsureContextLimit(ctx)
		// Compact at a user-message boundary. Compacting while a tool
		// call is still pending can drop that call. A tool result means
		// the call already finished. Smart compact (off by default) also
		// compacts then, once the conversation is over the compact budget,
		// so a long tool loop does not start another think against a full
		// window. Off keeps the user-message boundary only.
		lastRole := ""
		if len(a.Messages) > 0 {
			lastRole = a.Messages[len(a.Messages)-1].Role
		}
		if lastRole == "user" || (lastRole == "tool" && a.smartCompactEnabled()) {
			a.compactIfDue(ctx, h)
		}
		// Cap oversized tool bodies in place on the live message array,
		// spilling the full output exactly like a result arriving now (the
		// forced-compaction stage-1 pass keeps the model-free plain cap). The
		// cached counts are dropped when a body is rewritten. Runs BEFORE the
		// count backfill below: capping drops the cached counts, so backfilling
		// first would be discarded and the pre-flight would re-tokenize the
		// whole view anyway.
		a.capToolResultsForTurn()
		// Backfill a cold per-message count cache (a restored session, or the
		// cache capToolResultsForTurn just dropped) once, after the
		// boundary decision but before the pre-flight measurement, so the
		// pre-flight does not re-tokenize the whole view on every round
		// while the cache stays incomplete. The boundary decision above
		// deliberately runs first: the emergency tier is documented to skip
		// an incomplete cache (total -1) and fall back to the normal tier.
		a.ensureTokenCounts()
		view = a.Messages
	}
	// Stabilize tool args on a.Messages (not view, which may be a copy) so
	// ArgsStabilized is persisted and we skip already-stable messages.
	a.stabilizeToolArgs()

	view = buildSystemView(view, a.systemPromptBase(a.WorkingDir), a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode, a.mcpToolHint())

	// Pre-flight: verify the actual outgoing request fits the context
	// window, compacting in place when it would be refused. The boundary
	// check above only sees growth at conversation boundaries; tool
	// results appended mid-loop can push the request past the window
	// between boundaries, and nothing else verifies the outgoing view.
	view, err := a.preflightForcedCompact(ctx, h, view)
	if err != nil {
		return nil, err
	}

	a.recordViewForDrift(view)
	return view, nil
}

// preflightRecheckMargin is how far below the context limit the cached
// total may be before the pre-flight check skips the full view
// tokenization. The cached total (compactionTokenTotal) and the full view
// estimate use the same accounting (per-message counts + wire overhead, or
// the provider's exact prompt_tokens baseline), so a total this far under
// the limit cannot reach it; only within the margin is the view
// tokenized for real. The margin trades a rare redundant tokenization
// against a missed refusal — the provider remains the source of truth.
const preflightRecheckMargin = 2000

// outgoingViewEstimate estimates the wire token cost of the outgoing
// request: the actual view slice (system prompt + canonical history) plus
// the tool definitions. A cheap pre-check (compactionTokenTotal) skips the
// full view tokenization when the conversation is comfortably under the
// limit — tokenizing a large view on every round is too slow; only within
// preflightRecheckMargin of the limit (or on an incomplete count cache) is
// the view tokenized for real. The estimates are approximate (cl100k vs
// the provider's tokenizer): the provider is the source of truth, and
// Phase 3 recovers from a refusal that slips through.
func (a *Agent) outgoingViewEstimate(view []llm.Message) int {
	if a.Context == nil {
		return 0
	}
	if total := a.compactionTokenTotal(); total >= 0 && total < a.Context.ContextLimit()-preflightRecheckMargin {
		return total
	}
	return a.Context.EstimateTokens(view) + contextmgr.EstimateToolTokens(a.llmTools())
}

// rebuildView builds the LLM view from canonical history (system prompt and
// enrichment folded in), the same call prepareMessages makes.
func (a *Agent) rebuildView() []llm.Message {
	return buildSystemView(a.Messages, a.systemPromptBase(a.WorkingDir), a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode, a.mcpToolHint())
}

// preflightForcedCompact verifies that the outgoing view fits the context
// window and, when the request would be refused, runs an emergency forced
// compaction in place. It is the last local check before the request
// leaves: it fires only at/above the context limit (the hard window, where
// the alternative is a refused request), never at the compaction budget —
// the boundary tiers own everything below it. The estimates are
// approximate, so the provider remains the source of truth: Phase 3
// recovers from a refusal that slips through.
//
// When the request is still over the window after the forced compaction
// (or the attempt was suppressed or aborted), it hands off to last-resort
// condensation. In deep dive that pass can also summarize the request,
// when no other message's replacement fits. In the usual mode it condenses
// the largest message, and only when that replacement fits. In error mode
// (compact_last_resort=error) the hand-off returns the actionable
// diagnostic as the error instead of letting the provider refuse the
// request; in condense mode a failed condensation leaves the view as-is
// (the provider refusal remains the diagnostic, wrapped by Phase 3).
//
// The forced compaction is two-stage:
//  1. Cap oversized tool results (EnsureToolResultsCapped) — a model-free
//     win; re-measure and stop when capping alone brings the request
//     under the window (no summarization call).
//  2. Summarize with keep=0 (no preserved tail) and the minMiddleTokens
//     guard bypassed: mid-loop there may be only a few messages since the
//     last compaction. The smaller-summary guard still applies.
//
// A result whose summary text is unchanged is aborted. Replacing one
// message with a shorter summary keeps the count and still saves tokens.
// The smaller-summary guard is what keeps that from repeating.
//
// Two guards keep the check from hot-looping: a degenerate window (the
// limit does not exceed the wire overhead alone) can never fit any
// request, so the check stays out of the way — that state is Phase 0e's
// (last-resort condensation) or a misconfiguration, and the provider
// refusal is the diagnostic; and the emergency progress guard
// (compactionGuards.emergency) suppresses a summarization retry at an
// unchanged message count after a failed or aborted attempt (a retry
// would hit the same failure) — growth past that count re-arms it, as in
// the boundary emergency tier.
//
// Protocol safety mid-loop: the tail (the trailing tool group when the
// last message is a tool result, pulled back further by pins, empty
// otherwise) always starts at a non-tool message — adjustCompactTailStart
// walks back over trailing tool messages — so every tool result in the
// tail has its calling assistant message in the tail too, and the summary
// is a plain assistant message with no tool calls.
func (a *Agent) preflightForcedCompact(ctx context.Context, h *llm.StreamHandlers, view []llm.Message) ([]llm.Message, error) {
	if a.Context == nil {
		return view, nil
	}
	limit := a.Context.ContextLimit()
	if limit <= 0 || a.outgoingViewEstimate(view) < limit {
		return view, nil
	}
	// A degenerate window (the limit does not exceed the wire overhead
	// alone) can never fit any request — the system prompt and tool
	// definitions ride on every request — so no local strategy can help
	// and the check stays out of the way: the provider refusal is the
	// diagnostic.
	if limit <= a.wireOverheadTokens() {
		return view, nil
	}
	// The request would be refused. Stage 1: cap oversized tool results —
	// a model-free win. prepareMessages already caps every turn, so this
	// is normally a no-op; it makes the forced compaction self-contained.
	if a.capToolResultsForCompact() {
		view = a.rebuildView()
		if a.outgoingViewEstimate(view) < limit {
			return view, nil
		}
	}
	// Stage 2: forced summarization (keep=0, minMiddleTokens bypassed).
	// The emergency progress guard suppresses a retry at an unchanged
	// message count after a failed or aborted attempt (a retry would hit
	// the same failure); growth past that count re-arms it. A request
	// that condensation kept is not this skip: the layout still runs,
	// and the fallback below refuses to summarize that request.
	a.statsMu.RLock()
	guarded := a.compactionGuards.emergency >= len(a.Messages)
	a.statsMu.RUnlock()
	// A message the forced split will leave verbatim, measured before the
	// summary replaces the middle. A same-count summary can drop the
	// estimate under the window and skip last resort. The preserved
	// message is still condensed when it was larger than that middle.
	preservedLarger := a.preservedLargerThanMiddle()
	if !guarded {
		if a.runForcedSummarization(ctx, h, false) {
			view = a.rebuildView()
		}
	}
	if err := ctx.Err(); err != nil {
		// Cancelled during the forced pass. Last resort would record a
		// failure on the cancelled context and the next turn would stay
		// backed off.
		return view, err
	}
	if a.outgoingViewEstimate(view) < limit {
		if preservedLarger > 0 {
			outcome, condErr := a.condenseLargerPreserved(ctx, h, preservedLarger)
			if condErr != nil {
				return view, condErr
			}
			if outcome == condensePublished {
				view = a.rebuildView()
			}
		}
		return view, nil
	}
	// Still over after forced compaction (or a suppressed/aborted
	// attempt). Last resort reports why it did not publish, and the
	// request is summarized only when that reason says condensation
	// cannot fit. A view that is still over the window arms the
	// emergency guard on the way out.
	return a.settleOverWindow(ctx, h, view, limit)
}

// settleOverWindow runs last resort and, when condensation cannot fit,
// the single summary. A model summary that comes back too big leaves the
// request verbatim so a later condensation can be smaller. A cancel is
// returned without a guard.
func (a *Agent) settleOverWindow(ctx context.Context, h *llm.StreamHandlers, view []llm.Message, limit int) ([]llm.Message, error) {
	outcome, err := a.lastResortCondense(ctx, h, view)
	if err != nil {
		if ctx.Err() != nil {
			return view, ctx.Err()
		}
		a.finishStillOver(err)
		return view, err
	}
	if outcome == condensePublished {
		view = a.rebuildView()
		if a.outgoingViewEstimate(view) < limit {
			return view, nil
		}
	}
	// A summary that came back too big is not a reason to summarize the
	// request. The allowance still fits, so the ordinary layout returns
	// before it runs. Run it now: it keeps the request and summarizes
	// the regions around it. A later turn does not repeat it while the
	// emergency guard still records this count.
	if outcome == condenseAttemptMissed {
		changed, err := a.layoutAfterMissedCondensation(ctx, h)
		if err != nil {
			if ctx.Err() != nil {
				return view, ctx.Err()
			}
			return view, nil
		}
		if changed {
			view = a.rebuildView()
			if a.outgoingViewEstimate(view) < limit {
				return view, nil
			}
		}
		a.finishStillOver(fmt.Errorf("still over the context window after condensation"))
		return view, nil
	}
	// Nothing-fits and an already-guarded condensation are the reasons
	// that summarize the request, and only in deep dive. A remembered
	// request and a single summary that already failed at this message
	// count are not: repeating the summary doubles the backoff.
	if !a.summarizeRequestAllowed() {
		a.finishStillOver(fmt.Errorf("still over the context window after condensation"))
		return view, nil
	}
	if err := a.summarizeKeptRequestNow(ctx, h); err != nil {
		if ctx.Err() != nil {
			return view, ctx.Err()
		}
		log.Printf("forced compaction: single-summary fallback failed: %v", err)
		a.noteCompactFailure(err)
		a.noteProgressFailure(&a.compactionGuards.emergency)
		return view, nil
	}
	view = a.rebuildView()
	if a.outgoingViewEstimate(view) < limit {
		return view, nil
	}
	outcome, err = a.lastResortCondense(ctx, h, view)
	if err != nil {
		if ctx.Err() != nil {
			return view, ctx.Err()
		}
		a.finishStillOver(err)
		return view, err
	}
	if outcome == condensePublished {
		view = a.rebuildView()
	}
	if a.outgoingViewEstimate(view) >= limit {
		a.finishStillOver(fmt.Errorf("still over the context window after summarizing the request"))
	}
	return view, nil
}

// summarizeRequestAllowed reports whether the verbatim request should be
// summarized. A different message whose published replacement fits is
// condensed instead, unless that condensation is already guarded at this
// message count. A request a condensation kept is summarized once the
// view is over the window and nothing else fits. Under the window this
// is false, and the emergency tier returns through keptRequestUnderWindow.
func (a *Agent) summarizeRequestAllowed() bool {
	if a.singleSummaryFailed() {
		return false
	}
	if !a.keptRequestStillOver() {
		return false
	}
	return !a.leaveForCondensation()
}

// leaveForCondensation reports whether deep dive should condense a message
// other than the request instead of summarizing the request. The view is
// over the window, last resort is condense, that message's replacement
// would bring the view under, and condensation is not already guarded.
// Error mode still summarizes: the diagnostic is what remains after that
// summary. Under the window this is false, because last resort does not
// run there.
func (a *Agent) leaveForCondensation() bool {
	if a == nil || a.Context == nil || !a.Context.DeepDive() {
		return false
	}
	if a.Context.CompactLastResort() != "condense" {
		return false
	}
	limit := a.Context.ContextLimit()
	if limit <= 0 {
		return false
	}
	total := a.outgoingViewEstimate(a.rebuildView())
	if total < limit {
		return false
	}
	return condensableTailFits(a, total, limit) && !a.lastResortGuarded()
}

// lastResortGuarded reports whether condensation already failed at this
// message count.
func (a *Agent) lastResortGuarded() bool {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return a.compactionGuards.lastResort >= len(a.Messages)
}

// finishStillOver arms the emergency guard when the rebuilt view is still
// at or over the context window. A publish clears that guard, so a give-up
// after a summary that missed the window has to put it back, or the next
// round pays for another summarizer call. A backoff that is still running
// is left as it is, so one give-up does not stack two waits. A deadline
// that has already passed is not a running backoff: this give-up records
// it again with the doubled delay, the same comparison compactAttemptDue
// uses. A view under the window is left alone.
func (a *Agent) finishStillOver(err error) {
	if a == nil || a.Context == nil {
		return
	}
	limit := a.Context.ContextLimit()
	if limit <= 0 || a.outgoingViewEstimate(a.rebuildView()) < limit {
		return
	}
	a.statsMu.RLock()
	guard := a.compactionGuards.emergency
	backoffActive := !time.Now().After(a.compactBackoffUntil)
	n := len(a.Messages)
	a.statsMu.RUnlock()
	if !backoffActive {
		a.noteCompactFailure(err)
	}
	if guard < n {
		a.noteProgressFailure(&a.compactionGuards.emergency)
	}
}

// capToolResultsForCompact runs stage 1 of the forced compaction: cap
// oversized tool bodies in place (a model-free win). It returns true when
// a body was rewritten; the cached per-message counts are dropped in that
// case (they are rebuilt on the next ContextStats).
//
// It deliberately uses the plain, no-I/O cap (EnsureToolResultsCapped): the
// forced-compaction path may run in a tight retry loop, and the bodies it
// rewrites are frequently summarized out on the next pass anyway. The
// normal-turn pass that preserves the data is capToolResultsForTurn.
func (a *Agent) capToolResultsForCompact() bool {
	a.statsMu.Lock()
	capped := a.Context.EnsureToolResultsCapped(a.Messages)
	if capped {
		// Tool bodies were rewritten in place, so the cached counts are
		// stale; drop the cache (it is rebuilt on the next ContextStats).
		a.tokenCounts = nil
	}
	a.statsMu.Unlock()
	return capped
}

// capToolResultsForTurn runs the per-turn cap pass with the spill path:
// oversized tool bodies already in history are handled exactly like a result
// arriving now (capToolResult) — the full output is spilled to the session's
// spill dir and the inline body becomes a head+locator+tail preview — with
// the plain cap as the fallback when spilling is unavailable. This is the
// normal-turn counterpart of capToolResultsForCompact, which keeps the
// model-free, no-I/O cap for the forced-compaction stage-1 path.
//
// Spilling writes files, so the pass must NOT run under statsMu (a hot lock
// for ContextStats / history snapshots). It snapshots the candidates under
// the read lock, spills unlocked, then publishes under the write lock guarded
// by countsEpoch: a concurrent reshape (which bumps the epoch) discards the
// pending rewrite instead of applying previews at stale indices, and a
// discarded pass removes the spill files it wrote (they were never published).
// Appends do not shift existing indices, so they do not invalidate the
// candidates.
//
// Returns true when a body was rewritten; the cached per-message counts are
// dropped in that case (they are rebuilt on the next ContextStats).
func (a *Agent) capToolResultsForTurn() bool {
	if a.Context == nil {
		return false
	}
	max := a.Context.SettingsSnapshot().MaxToolResultBytes
	if max <= 0 {
		return false
	}

	type candidate struct {
		idx   int
		orig  string
		label string
	}
	var cands []candidate
	a.statsMu.RLock()
	// The tool-name lookup is built lazily: a turn with no oversized body
	// (the common case) pays only the length scan below. The label only names
	// the spill file, so a missing call falls back to a generic name.
	var names map[string]string
	for i := range a.Messages {
		m := &a.Messages[i]
		if m.Role != "tool" || m.Content == "" || len(m.Content) <= max {
			continue
		}
		// The precise idempotency predicate the arriving-result path uses
		// (alreadyPartial): a spill preview (an intact locator) or a plain cap
		// (the marker terminating the body) is left alone so repeated passes
		// stay no-ops and the prompt prefix stays stable — while a body that
		// merely CONTAINS the marker text (a file quoting it, a log line) is
		// still spilled, exactly like a result arriving now. The coarse
		// HasTruncationMarker the compaction path uses would skip such a body
		// and leave it oversized.
		if alreadyPartial(m.Content) {
			continue
		}
		if names == nil {
			names = make(map[string]string)
			for j := range a.Messages {
				for k := range a.Messages[j].ToolCalls {
					tc := &a.Messages[j].ToolCalls[k]
					if tc.ID != "" && tc.Name != "" {
						names[tc.ID] = tc.Name
					}
				}
			}
		}
		label := names[m.ToolCallID]
		if label == "" {
			label = "tool"
		}
		cands = append(cands, candidate{idx: i, orig: m.Content, label: label})
	}
	epoch := a.countsEpoch
	a.statsMu.RUnlock()

	if len(cands) == 0 {
		return false
	}

	// Spill outside the lock — this is the disk I/O the compaction path
	// deliberately avoids.
	previews := make([]string, len(cands))
	for i := range cands {
		previews[i] = a.capToolResult(cands[i].label, cands[i].orig)
	}

	a.statsMu.Lock()
	applied := a.countsEpoch == epoch
	changed := false
	if applied {
		for i := range cands {
			c := &cands[i]
			p := previews[i]
			if c.idx >= len(a.Messages) || a.Messages[c.idx].Content != c.orig || p == c.orig {
				continue
			}
			a.Messages[c.idx].Content = p
			changed = true
		}
		if changed {
			// Tool bodies were rewritten in place, so the cached counts are
			// stale; drop the cache (it is rebuilt on the next ContextStats).
			a.tokenCounts = nil
		}
	}
	a.statsMu.Unlock()
	if !applied {
		// A reshape bumped the epoch while the spill I/O ran unlocked: the
		// previews are stale and were never published, so remove the files
		// this pass wrote instead of leaking them until session deletion.
		// Outside statsMu — this is disk I/O.
		removeStaleSpills(previews)
	}
	return changed
}

// runForcedSummarization runs stage 2 of the forced compaction: the forced
// summarization (keep=0, minMiddleTokens bypassed). A result that did not
// change the history is aborted. A shorter count, or a summary that
// replaced one message, is published with the post-success bookkeeping
// (fresh counts, pin remap, clear turn usage, reset save tracking) — the
// same bookkeeping the auto-compaction path applies. It fires h.OnCompacting
// before the summarization call so the UI can show compaction progress.
// singleSummary skips the deep-dive layout, so the current request is
// summarized in this pass. Callers pass false. When that layout keeps the
// request verbatim and the rebuilt view is still over the window, one
// more pass summarizes the request and fires OnCompacting again, unless
// another message's condensation would fit. A failure of that pass is
// recorded like this path's own failures, including the single-summary
// guard the next refusal reads. It returns true when the history changed
// and was republished.
func (a *Agent) runForcedSummarization(ctx context.Context, h *llm.StreamHandlers, singleSummary bool) bool {
	// A tool round does not reach compactToFit when smart compact is off,
	// so this is the first summarizer. Publishing here can drop the view
	// under the window while it stays over the hard limit, and the next
	// emergency last keep would summarize the request. Condense instead.
	if a.leaveForCondensation() {
		return false
	}
	if h != nil && h.OnCompacting != nil {
		h.OnCompacting()
	}
	before := len(a.Messages)
	beforeBlob := summaryBlob(a.Messages)
	shrunk, err := a.compactNoTail(ctx, contextmgr.CompactOptions{Forced: true, SingleSummary: singleSummary})
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		// A deep-dive refusal means both regions came back no smaller.
		// The request may still be verbatim. Default mode records the
		// refusal, as it did before deep dive: a backoff and the
		// emergency guard. A later turn does not repeat it.
		if singleSummary || !a.Context.DeepDive() || !errors.Is(err, contextmgr.ErrSummaryNotSmaller) {
			a.noteCompactFailure(err)
			a.noteProgressFailure(&a.compactionGuards.emergency)
			if singleSummary {
				a.noteSingleSummaryFailure()
			}
			return false
		}
	}
	if !shrunk && !singleSummary {
		if err := a.summarizeKeptRequest(ctx, h); err != nil {
			if ctx.Err() != nil {
				return false
			}
			log.Printf("forced compaction: single-summary fallback failed: %v", err)
			a.noteCompactFailure(err)
			a.noteProgressFailure(&a.compactionGuards.emergency)
			return false
		}
		if len(a.Messages) != before || summaryBlob(a.Messages) != beforeBlob {
			return true
		}
		// The same pass reaches last resort and can condense the other
		// message. That is not a failed compaction. When condensation
		// is already guarded, or the estimate is under the window, or
		// this is default mode, fall through and arm the guard.
		if a.leaveForCondensation() {
			return false
		}
	}
	if !shrunk {
		// The history did not change. Publishing it would drop the
		// provider's token count and the next attempt would repeat it.
		log.Printf("forced compaction aborted: no shrink (%d -> %d messages)", before, len(a.Messages))
		a.noteProgressFailure(&a.compactionGuards.emergency)
		if singleSummary {
			a.noteSingleSummaryFailure()
		}
		return false
	}
	if !singleSummary {
		if err := a.summarizeKeptRequest(ctx, h); err != nil {
			if ctx.Err() != nil {
				return shrunk
			}
			log.Printf("forced compaction: single-summary fallback failed: %v", err)
			a.noteCompactFailure(err)
			a.noteProgressFailure(&a.compactionGuards.emergency)
		}
	}
	return true
}

// summarizeKeptRequest is the pre-flight fit fallback. When a later
// request is still verbatim and the rebuilt view is still over the
// window, one single-summary pass summarizes that request with the rest.
// The request may be verbatim because the layout kept it, or because the
// regions around it were empty. A history whose only request is the head
// is already the single summary and is left alone. It fires
// h.OnCompacting again, because this pass is a second summarizer call. A
// summarizer failure is returned for the caller to record.
func (a *Agent) summarizeKeptRequest(ctx context.Context, h *llm.StreamHandlers) error {
	// The same decision as the emergency tier and overflow. A message
	// whose condensation would fit is not summarized away, unless that
	// condensation is already guarded.
	if !a.summarizeRequestAllowed() {
		return nil
	}
	return a.summarizeKeptRequestNow(ctx, h)
}

// keptRequestStillOver reports whether a later request is still verbatim
// and the rebuilt view is still over the window. The request may be
// verbatim because the layout kept it, or because the regions around it
// were empty. A history whose only request is the head is already the
// single summary.
func (a *Agent) keptRequestStillOver() bool {
	if a.Context == nil || !a.Context.DeepDive() {
		return false
	}
	limit := a.Context.ContextLimit()
	if limit <= 0 || a.outgoingViewEstimate(a.rebuildView()) < limit {
		return false
	}
	return layoutKeptRequest(a.Messages) || requestAfterHead(a.Messages)
}

// summarizeKeptRequestNow summarizes the verbatim request. It does not
// ask whether a tail message would condense: the caller has already
// decided that condensation will not fit.
func (a *Agent) summarizeKeptRequestNow(ctx context.Context, h *llm.StreamHandlers) error {
	if h != nil && h.OnCompacting != nil {
		h.OnCompacting()
	}
	changed, err := a.compactSingleSummary(ctx, true)
	if ctx.Err() == nil && (err != nil || !changed) {
		// The same summary is not tried again at this message count.
		// Repeating it doubles the backoff and cannot succeed when the
		// summary was not smaller or changed nothing. A new message
		// re-arms it. The emergency guard is not this signal: that
		// guard is also set when condensation should still run.
		a.noteSingleSummaryFailure()
	}
	return err
}

// singleSummaryFailed reports whether the single summary already failed
// at this message count.
func (a *Agent) singleSummaryFailed() bool {
	if a == nil {
		return false
	}
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return a.compactionGuards.singleSummary > 0 && a.compactionGuards.singleSummary >= len(a.Messages)
}

// condensableChoice is the message last resort replaces. Deep dive prefers
// a message other than the latest request when that replacement fits, so
// the request can stay verbatim. Context mode off keeps the rule from
// before this feature: the largest condensable message, and only when its
// own replacement fits. A smaller message is not a fallback in that mode.
func (a *Agent) condensableChoice(total int, msgs []llm.Message, counts []int, limit int) (int, bool) {
	if a.Context != nil && a.Context.DeepDive() {
		return condensableIndex(a, total, msgs, counts, limit)
	}
	idx, c := largestCondensableFrom(msgs, counts, 0)
	if idx < 0 || condensationLeft(total, c, condensedReplacementTokens(a, msgs[idx])) >= limit {
		return -1, false
	}
	return idx, true
}

// condensableTailFits reports whether last resort would condense a message
// other than the latest request, and that condensation would bring the
// view under the window. total is the outgoing view estimate the caller
// already measured. The latest request is left to the single summary.
func condensableTailFits(a *Agent, total, limit int) bool {
	msgs, counts := a.messageCounts()
	idx, ok := condensableIndex(a, total, msgs, counts, limit)
	return ok && idx != latestRequestIndex(msgs)
}

// condensedReplacementTokens is the size last resort will publish for msg.
// A message the summarizer can accept uses the allowance. A message over
// that budget uses the truncation CondenseMessage publishes.
func condensedReplacementTokens(a *Agent, msg llm.Message) int {
	if a.Context != nil {
		if tokens, truncated := a.Context.CondensedReplacementTokens(msg); truncated {
			return tokens
		}
	}
	return lastResortCondenseAllowance
}

// condensationLeft is the outgoing view estimate after a message's
// replacement: the view total minus the message's token count plus its
// published replacement size.
func condensationLeft(total, c, replacement int) int {
	return total - c + replacement
}

// condensableIndex is the message deep dive's last resort condenses. A
// message other than the latest request is preferred when replacing it with what
// condensation publishes brings the view under the window. Several such
// messages take the one that leaves the smallest view. A pin is skipped
// there. When none of those fits, the largest unprotected message is
// used if its own replacement fits, which is how a request that is the
// whole window is still condensed. When nothing outside the pin fits,
// the largest message inside the pin is used on the same terms.
// Compaction still keeps the pin. ok is false when no replacement fits.
func condensableIndex(a *Agent, total int, msgs []llm.Message, counts []int, limit int) (idx int, ok bool) {
	if len(counts) < len(msgs) {
		return -1, false
	}
	lastReq := latestRequestIndex(msgs)
	var pins map[int]struct{}
	if a != nil && a.PinManager != nil {
		pins = a.PinManager.PinnedSet()
	}
	// One replacement size per message: the first pass and the two
	// fallbacks can ask for the same message again, and rendering and
	// tokenizing it is the expensive part of this scan.
	replacement := make([]int, len(msgs))
	for i := range replacement {
		replacement[i] = -1
	}
	replacementTokens := func(i int) int {
		if replacement[i] < 0 {
			replacement[i] = condensedReplacementTokens(a, msgs[i])
		}
		return replacement[i]
	}
	best, bestLeft := -1, 0
	for i, m := range msgs {
		if i == lastReq || !condensableMessage(m) || protectedByPin(msgs, i, pins) {
			continue
		}
		left := condensationLeft(total, counts[i], replacementTokens(i))
		if left >= limit {
			continue
		}
		if best < 0 || left < bestLeft {
			best, bestLeft = i, left
		}
	}
	if best >= 0 {
		return best, true
	}
	// No other message fits. The largest unprotected message, which may
	// be the request, is condensed when its own replacement fits. A pin
	// is still skipped: compaction kept that stretch, and an unpinned
	// message was the first choice.
	if idx, ok := largestFittingCondensable(total, msgs, counts, limit, pins, true, replacementTokens); ok {
		return idx, true
	}
	// Nothing outside the pin brings the view under the window. The pin
	// is what no longer fits. Last resort condenses the largest message
	// in that stretch. Compaction still keeps the pin.
	return largestFittingCondensable(total, msgs, counts, limit, nil, false, replacementTokens)
}

// largestFittingCondensable is the largest condensable message whose
// published replacement brings the view under the window. A larger
// message that would stay over the window does not hide a smaller one
// that fits. honorPins skips a message the pin covers. replacement
// returns the published size for a message and is shared with the
// caller's scan. ok is false when none fits.
func largestFittingCondensable(total int, msgs []llm.Message, counts []int, limit int, pins map[int]struct{}, honorPins bool, replacement func(int) int) (int, bool) {
	best, bestTok := -1, 0
	for i, m := range msgs {
		if !condensableMessage(m) {
			continue
		}
		if honorPins && protectedByPin(msgs, i, pins) {
			continue
		}
		if condensationLeft(total, counts[i], replacement(i)) >= limit {
			continue
		}
		if counts[i] > bestTok {
			best, bestTok = i, counts[i]
		}
	}
	if best < 0 {
		return -1, false
	}
	return best, true
}

// protectedByPin reports whether a pin keeps idx verbatim through
// compaction. Compaction preserves the pinned message and everything
// after it, except a pin on the head, which compaction ignores. Last
// resort skips that stretch until nothing outside it fits.
func protectedByPin(msgs []llm.Message, idx int, pins map[int]struct{}) bool {
	if len(pins) == 0 || idx < 0 || idx >= len(msgs) {
		return false
	}
	head := -1
	for i, m := range msgs {
		if contextmgr.IsRequest(m) {
			head = i
			break
		}
	}
	for p := range pins {
		if p <= head || p >= len(msgs) {
			continue
		}
		if p <= idx {
			return true
		}
	}
	return false
}

// latestRequestIndex is the index of the latest user request, or -1.
// The walk lives in contextmgr so the agent and the compactor share it.
func latestRequestIndex(msgs []llm.Message) int {
	return contextmgr.LatestRequestIndex(msgs)
}

// condensableMessage reports whether last resort may replace msg. Assistant
// messages are never candidates. A user message with images is not either:
// the summarizer is text-only.
func condensableMessage(m llm.Message) bool {
	switch m.Role {
	case "user":
		return !m.HasImages() && m.Content != "" &&
			!contextmgr.IsCompactionSummary(m.Content) && !contextmgr.IsCondensedMessage(m.Content)
	case "tool":
		return m.Content != "" && !contextmgr.IsCondensedMessage(m.Content)
	default:
		return false
	}
}

// requestBounds returns the first and last real-user-request index in
// msgs, or -1, -1 when there is none.
func requestBounds(msgs []llm.Message) (first, last int) {
	first, last = -1, -1
	for i, m := range msgs {
		if !contextmgr.IsRequest(m) {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	return first, last
}

// requestAfterHead reports whether a user request sits after the first one.
// The first request is the head. A later one is what the deep-dive layout
// keeps, and the last keep summarizes it when the regions around it wrote
// nothing.
func requestAfterHead(msgs []llm.Message) bool {
	first, last := requestBounds(msgs)
	return first >= 0 && last > first
}

// layoutKeptRequest reports whether a deep-dive compaction kept the latest
// request verbatim. That result has a progress summary after the request
// or, when the progress region was empty, one session summary between the
// head and the request. A tail may follow the request.
func layoutKeptRequest(msgs []llm.Message) bool {
	first, last := requestBounds(msgs)
	if last < 0 {
		return false
	}
	for _, m := range msgs[last+1:] {
		if strings.HasPrefix(m.Content, contextmgr.ProgressSummaryPrefix) {
			return true
		}
	}
	if last == first {
		return false
	}
	between := msgs[first+1 : last]
	return len(between) == 1 && strings.HasPrefix(between[0].Content, contextmgr.SummaryPrefix)
}

// compactNoTail runs one compaction with no preserved tail. The caller sets
// Forced and SingleSummary; the pins, counts, view prefix and
// tools come from the live session, so every no-tail caller compacts the
// same way. It publishes the result and reports true when the history
// changed: a shorter count, or a summary that replaced one message. An
// unchanged summary text is left unpublished. A summarizer failure is
// returned for the caller to record.
func (a *Agent) compactNoTail(ctx context.Context, opts contextmgr.CompactOptions) (bool, error) {
	if a.PinManager != nil {
		opts.Pinned = a.PinManager.PinnedSet()
	}
	a.statsMu.RLock()
	opts.Counts = append([]int(nil), a.tokenCounts...)
	a.statsMu.RUnlock()
	opts.ViewPrefix = a.systemPromptPrefix()
	opts.Keep = contextmgr.NoTailKeep
	opts.Tools = a.llmTools()
	opts.AllowedTools = a.AllowedToolNames()
	compacted, newPins, err := a.Context.Compact(ctx, a.Messages, opts)
	if err != nil {
		return false, err
	}
	// A one-message region becomes one shorter summary: the count stays
	// the same and the summary text changes. That result is published.
	// An unchanged summary text saved nothing.
	if !historyChanged(a.Messages, compacted) {
		return false, nil
	}
	a.publishCompaction(compacted, newPins)
	return true, nil
}

// compactSingleSummary is deep dive's fit fallback: one compaction with the
// single-summary layout and no tail, which summarizes the current request
// with the rest. Pre-flight uses it when the layout around the request left
// the view over the window. The emergency tier uses it when that layout left
// the history at or above the hard limit. Overflow recovery runs the
// layout first. This pass runs only when that result is still over the
// window.
// forced selects the forced compaction; otherwise it runs as the emergency
// tier. It publishes the result and reports true when the history changed.
// A summarizer failure is returned for the caller to record.
func (a *Agent) compactSingleSummary(ctx context.Context, forced bool) (bool, error) {
	return a.compactNoTail(ctx, contextmgr.CompactOptions{
		Forced:        forced,
		SingleSummary: true,
	})
}

// publishCompaction publishes a compacted history with the post-compaction
// bookkeeping: fresh per-message counts (the conversation just shrank, so
// counting it is cheap) published atomically with the messages, pin remap,
// cleared turn usage (lastTurnUsage is no longer representative after
// compaction), and reset save tracking. It returns the new counts so a
// retrying caller (compactToFit) can pass them to the next compaction pass.
func (a *Agent) publishCompaction(compacted []llm.Message, newPins map[int]struct{}) []int {
	counts := contextmgr.CountTokens(compacted)
	a.replaceMessagesWithCounts(compacted, counts)
	if a.PinManager != nil {
		a.PinManager.ReplacePins(newPins)
	}
	a.clearTurnUsage()
	a.resetSaveTracking()
	a.noteCompactSuccess()
	return counts
}

// lastResortCondenseAllowance is the token ceiling assumed for a model
// summary of one message that fits the summary input budget. The framed
// marker adds a fixed prefix. The ceiling is deliberately conservative —
// after the summary comes back, the actual framed size is re-measured
// before the replacement is published, so an over-long summary is refused
// instead of shipped. A message over that budget is not planned with this
// ceiling: condensation publishes a truncation, and the fit check uses
// that size.
const lastResortCondenseAllowance = 2000

// lastResortCondense is Phase 0e — the last-resort condensation for a
// message that cannot fit the context window. It is strictly last-resort:
// callers reach it only after all compaction (the 0d hand-off) has left
// the request provably over the window. It fixes the permanently-stuck
// case — a single user message bigger than the window (e.g. a fresh
// session where the message is the head): firstUserIndex preserves it
// verbatim, there is no middle to summarize, every request is refused.
//
// It finds a condensable message (a text-only user message or tool
// result — not a compaction summary, not already condensed) whose
// published replacement brings the view under the window. Deep dive
// prefers a message other than the latest request. Context mode off
// uses the largest message, as before. It then checks that
// replacement against the window (the message plus the
// irreducible floor — head, minimal tail, wire overhead — fits). When it
// would, it condenses that message in place via the summarizer, archives
// the original to the session's archive sidecar (Phase 5), and announces
// the condensation in-band (h.OnCondensed).
//
// condensePublished means the replacement was published and the caller
// rebuilds the view. condenseNothingFits and condenseGuarded allow the
// caller to summarize the request. condenseSummarizerError is a failed
// summarizer call that did not arm the guard. condenseAttemptMissed does
// not allow the summary: the planned replacement still fits, and a later
// summary can be smaller. Error mode returns an error and leaves the
// history untouched. A cancel returns ctx.Err() and does not set a guard.
type condenseOutcome int

const (
	condenseSkipped condenseOutcome = iota
	condensePublished
	condenseNothingFits
	condenseAttemptMissed
	condenseGuarded
	condenseSummarizerError
)

func (a *Agent) lastResortCondense(ctx context.Context, h *llm.StreamHandlers, view []llm.Message) (condenseOutcome, error) {
	if a.Context == nil {
		return condenseSkipped, nil
	}
	limit := a.Context.ContextLimit()
	if limit <= 0 {
		return condenseSkipped, nil
	}
	total := a.outgoingViewEstimate(view)
	if total < limit {
		return condenseSkipped, nil // defensive: strictly last-resort
	}
	msgs, counts := a.messageCounts()
	idx, ok := a.condensableChoice(total, msgs, counts, limit)
	if !ok {
		return condenseNothingFits, nil
	}
	c := counts[idx]
	if a.Context.CompactLastResort() != "condense" {
		// Error mode: the actionable diagnostic replaces the raw provider
		// refusal. No attempt is made, so no guard is needed.
		return condenseNothingFits, fmt.Errorf("message is ~%d tokens vs a %d-token window; shorten it or start a fresh session (/new)", c, limit)
	}
	if a.lastResortGuarded() {
		return condenseGuarded, nil
	}
	return a.condenseChosen(ctx, h, msgs, idx, c, total, limit, true)
}

// preservedBounds returns the live messages and counts and the head and
// first preserved-tail index of a forced compaction. ok is false when the
// session is not in condense mode or the history has no head. The caller
// still applies its own tail checks.
func (a *Agent) preservedBounds() (msgs []llm.Message, counts []int, head, tail int, ok bool) {
	if a == nil || a.Context == nil || a.Context.CompactLastResort() != "condense" {
		return nil, nil, 0, 0, false
	}
	var pins map[int]struct{}
	if a.PinManager != nil {
		pins = a.PinManager.PinnedSet()
	}
	msgs, counts = a.messageCounts()
	head, tail, ok = contextmgr.ForcedPreserveStart(msgs, pins, a.Context.DeepDive())
	return msgs, counts, head, tail, ok
}

// condensePreserved publishes the condensation of a preserved message.
// The outgoing view and the window are measured here; recordGuard is
// passed to condenseChosen.
func (a *Agent) condensePreserved(ctx context.Context, h *llm.StreamHandlers, msgs []llm.Message, idx, c int, recordGuard bool) (condenseOutcome, error) {
	limit := a.Context.ContextLimit()
	total := a.outgoingViewEstimate(a.rebuildView())
	return a.condenseChosen(ctx, h, msgs, idx, c, total, limit, recordGuard)
}

// condensePreservedTail condenses the largest message a forced compaction
// would leave verbatim, when the provider has refused and the local
// estimate is still under the window. That message is the head, the
// trailing tool group, or the pinned stretch. The head is chosen when
// it is larger than the tail, because a single summary does not replace
// it. A tail message is chosen when it is larger than the middle, so
// a larger request is still summarized and a short or equal tool is
// left. A same-size message does not spend the retry: the summary of
// the middle is what shrinks the history. A cancel is returned.
// Nothing is published when no such message exists.
func (a *Agent) condensePreservedTail(ctx context.Context, h *llm.StreamHandlers) (condenseOutcome, error) {
	msgs, counts, head, tail, ok := a.preservedBounds()
	if !ok || head < 0 || head >= len(msgs) || head >= len(counts) {
		return condenseSkipped, nil
	}
	// An empty tail means compaction would summarize everything after
	// the head. The head can still be the message the provider refused.
	if tail < head+1 {
		tail = head + 1
	}
	if tail > len(msgs) {
		tail = len(msgs)
	}
	middle := middleTokenSum(counts, head, tail)
	best, bestTok := largestCondensableFrom(msgs, counts, tail)
	// The head is not in the tail and a single summary does not replace
	// it. When it is the largest message, condensing a smaller tool or
	// summarizing the middle would spend the only retry and send the
	// head again.
	headTok := 0
	if head >= 0 && head < len(counts) && condensableMessage(msgs[head]) {
		headTok = counts[head]
	}
	// Choose the preserved message before the guard. When none is larger
	// than the middle, the middle is the bulk and can still be summarized.
	// A guard or a summarizer error then applies only to that message.
	idx, tok := -1, 0
	if headTok > bestTok && headTok > middle {
		idx, tok = head, headTok
	} else if best >= 0 && bestTok > middle {
		idx, tok = best, bestTok
	}
	if idx < 0 {
		return condenseSkipped, nil
	}
	if a.lastResortGuarded() {
		return condenseGuarded, nil
	}
	return a.condensePreserved(ctx, h, msgs, idx, tok, true)
}

// preservedLargerThanMiddle is the token count of the largest message a
// forced compaction leaves verbatim, when that message is larger than
// the middle the summary replaces. Zero means there is no such message.
// Measured before the summary, so a shorter replacement cannot make a
// small tool look like the bulk.
func (a *Agent) preservedLargerThanMiddle() int {
	msgs, counts, head, tail, ok := a.preservedBounds()
	if !ok || tail >= len(msgs) {
		return 0
	}
	middle := middleTokenSum(counts, head, tail)
	_, best := largestCondensableFrom(msgs, counts, tail)
	if best <= middle {
		return 0
	}
	return best
}

// middleTokenSum is the token count of the region a forced summary
// replaces, messages[head+1:tail]. The preserved message is larger only
// when it beats that whole region, not the largest message inside it.
func middleTokenSum(counts []int, head, tail int) int {
	sum := 0
	for i := head + 1; i < tail && i < len(counts); i++ {
		if counts[i] > 0 {
			sum += counts[i]
		}
	}
	return sum
}

// largestCondensableFrom returns the index and token count of the largest
// condensable message in msgs[start:], or (-1, 0) when none qualifies.
func largestCondensableFrom(msgs []llm.Message, counts []int, start int) (idx, tokens int) {
	best, bestTok := -1, 0
	for i := start; i < len(msgs) && i < len(counts); i++ {
		if !condensableMessage(msgs[i]) {
			continue
		}
		if counts[i] > bestTok {
			best, bestTok = i, counts[i]
		}
	}
	return best, bestTok
}

// condenseLargerPreserved condenses the preserved message measured by
// preservedLargerThanMiddle. The summary may already have dropped the
// estimate under the window. The message is found again, because a
// summary that drops the count moves the tail. A smaller preserved
// message is left alone.
func (a *Agent) condenseLargerPreserved(ctx context.Context, h *llm.StreamHandlers, largerThan int) (condenseOutcome, error) {
	if largerThan <= 0 || a == nil || a.lastResortGuarded() {
		return condenseSkipped, nil
	}
	msgs, counts, _, tail, ok := a.preservedBounds()
	if !ok || tail >= len(msgs) {
		return condenseSkipped, nil
	}
	best, bestTok := largestCondensableFrom(msgs, counts, tail)
	if best < 0 || bestTok < largerThan {
		return condenseSkipped, nil
	}
	// The view already fits. A summarizer error here must not arm the
	// last-resort guard: the provider has not refused this view, and the
	// overflow path is what retries the condensation.
	return a.condensePreserved(ctx, h, msgs, best, bestTok, false)
}

// condenseChosen publishes a condensation of msgs[idx]. total and limit
// are the view estimate and the window. A summary that is not smaller, or
// that would leave the view over the window, is not published and is not
// recorded as a failure. recordGuard arms the last-resort guard on a
// summarizer error. The overflow attempt does. A condensation after a
// summary that already fits does not: the view has not been refused, and
// arming the guard would stop the overflow path from condensing that
// message. A cancel is returned.
func (a *Agent) condenseChosen(ctx context.Context, h *llm.StreamHandlers, msgs []llm.Message, idx, c, total, limit int, recordGuard bool) (condenseOutcome, error) {
	if h != nil && h.OnCompacting != nil {
		h.OnCompacting()
	}
	summary, err := a.Context.CondenseMessage(ctx, msgs[idx])
	if err != nil {
		if ctx.Err() != nil {
			return condenseSkipped, ctx.Err()
		}
		log.Printf("last-resort condensation failed: %v", err)
		if recordGuard {
			a.noteProgressFailure(&a.compactionGuards.lastResort)
			return condenseGuarded, nil
		}
		return condenseSummarizerError, nil
	}
	framed := llm.Message{
		Role:       msgs[idx].Role,
		Content:    contextmgr.CondensedMessagePrefix + summary,
		ToolCallID: msgs[idx].ToolCallID,
	}
	framedTok := contextmgr.ComputeMessageTokens(framed)
	if framedTok >= c {
		// No shrink: the summary is as big as the message it replaces.
		// Leave the history untouched (and do not record a failure — a
		// later attempt may produce a smaller summary).
		log.Printf("last-resort condensation aborted: framed summary %d tokens not smaller than the message %d", framedTok, c)
		return condenseAttemptMissed, nil
	}
	if condensationLeft(total, c, framedTok) >= limit {
		// The actual framed summary is bigger than the allowance the
		// pre-check assumed. Leave the message alone so a later summary
		// can be smaller. The caller must not summarize the request:
		// the planned replacement still fits.
		log.Printf("last-resort condensation aborted: request would still be over the window (%d - %d + %d >= %d)", total, c, framedTok, limit)
		return condenseAttemptMissed, nil
	}
	// Archive the original (Phase 5 sidecar), condense in place, publish.
	archived := a.archiveShadowedMessage(idx, msgs[idx], c)
	a.condenseMessageAt(idx, framed)
	// lastTurnUsage is no longer representative after condensation.
	a.clearTurnUsage()
	// resetSaveTracking forces a full snapshot on the next save: the
	// in-place content change is invisible to the incremental delta path.
	a.resetSaveTracking()
	a.FlushSession()
	a.noteCondensationPublished()
	if h != nil && h.OnCondensed != nil {
		suffix := " The original was archived."
		if !archived {
			suffix = " Archiving the original failed (session persistence off or sidecar write error)."
		}
		h.OnCondensed(fmt.Sprintf("A message was ~%d tokens vs a %d-token context window; it was condensed to continue.%s", c, limit, suffix))
	}
	return condensePublished, nil
}

// messageCounts returns the live messages and their per-message token
// counts: the cached counts when complete, else freshly computed for every
// message. Both slices are copies safe to use after the lock is released.
func (a *Agent) messageCounts() ([]llm.Message, []int) {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	msgs := append([]llm.Message(nil), a.Messages...)
	if a.tokenCounts != nil && len(a.tokenCounts) == len(msgs) {
		return msgs, append([]int(nil), a.tokenCounts...)
	}
	counts := contextmgr.CountTokens(msgs)
	return msgs, counts
}

// condenseMessageAt replaces the message at idx in place with the framed
// condensed version and updates the cached count. The index, role, and
// position are preserved — only the content changes — so pins and the
// tool-call/result protocol are untouched. Leaf lock.
func (a *Agent) condenseMessageAt(idx int, framed llm.Message) {
	a.statsMu.Lock()
	a.Messages[idx] = framed
	if a.tokenCounts != nil && len(a.tokenCounts) == len(a.Messages) {
		a.tokenCounts[idx] = contextmgr.ComputeMessageTokens(framed)
	}
	// The content of a cached message changed: bump the epoch so a
	// concurrent ContextStats that computed counts for the older snapshot
	// cannot publish stale numbers.
	a.countsEpoch++
	a.statsMu.Unlock()
}

// handleOverflowError is Phase 3 — the last line of defense that makes
// "stuck" impossible: the pre-flight estimates are approximate (cl100k vs
// the provider's real tokenizer), so a request that looked like it would
// fit can still be refused by the provider. Instead of aborting the turn,
// it recovers in-loop: run the forced compaction (two-stage cap-first,
// keep=0, minMiddleTokens bypassed) and let the caller retry the round
// once — the next runTurn rebuilds the view from the changed history. An
// unchanged history is not retried.
//
// Returns (true, nil) when the result measures under the window and the
// caller should continue the loop. A shrink that is still over does not
// spend the retry: last resort runs on that history, and the retry is
// spent only when the published result is under the window. Last resort
// also runs when the forced compaction changed nothing. When the estimate
// is still under the window, a tool result or pin that compaction would
// leave verbatim is condensed first when it is larger than the middle,
// in deep dive and in the usual mode, and the retry is that condensation.
// Returns (false, nil) when the
// error is NOT a context-window refusal or no context manager is
// configured: the caller returns the original error untouched. Returns
// (false, ctx.Err()) when the context was cancelled: the user cancelled,
// and the loop reports the cancellation (as the toolRoundCancelled path
// does) rather than the refusal that happened to arrive with it. Returns
// (false, terminal) when recovery is exhausted — a second refusal in the
// same turn, or a failed or still-over compaction that condensation did
// not bring under the window: terminal is an actionable error that wraps
// (never masks) the original refusal.
//
// Guards against infinite loops: the per-turn counter (overflowRetries,
// reset at Run start) allows at most one recovery per turn, and the
// unchanged-history abort in runForcedSummarization makes a compaction
// that did not change the history a terminal failure instead of a retry.
func (a *Agent) handleOverflowError(ctx context.Context, h *llm.StreamHandlers, err error) (retry bool, terminal error) {
	if !llm.IsContextWindowError(err) {
		// Strict classifier: non-overflow errors take the untouched
		// err != nil path — even when the context is also cancelled, the
		// original error is surfaced (never masked).
		return false, nil
	}
	if ctx.Err() != nil {
		// No retry on cancellation: the user cancelled, and the run loop
		// surfaces ctx.Err() for that (as the toolRoundCancelled path
		// does).
		return false, ctx.Err()
	}
	if a.Context == nil {
		// Nothing to compact with; surface the original error.
		return false, nil
	}
	if a.overflowRetries.Load() >= 1 {
		// The forced compaction already ran this turn and the shrunken
		// request was refused too: another retry cannot converge.
		return false, a.overflowTerminalError(err)
	}
	// Stage 1: cap oversized tool results — a model-free win. The estimate
	// is wrong here (that is why the provider refused), so capping alone
	// is never trusted to have fixed the request. Stage 2 runs unless
	// deep dive is about to condense a different message: summarizing
	// first would delete it.
	a.capToolResultsForCompact()
	// Stage 2: forced summarization (keep=0, minMiddleTokens bypassed,
	// unchanged history aborted, post-success bookkeeping identical to
	// the pre-flight path).
	// In deep dive the request is summarized in this pass when no other
	// message's condensation would fit. The provider already refused.
	// When a condensation would fit, or the last one came back too big,
	// summarizing the request would undo that decision. The usual case
	// is a local estimate still under the window. A message that
	// compaction would leave verbatim, the trailing tool group or a pin,
	// is condensed first when it is larger than the middle, in deep dive
	// and in the usual mode. The retry is that condensation. A
	// condensation that comes back no smaller does not summarize the
	// smaller middle and does not spend the retry: the large message
	// would be sent again, and the next refusal would end the turn.
	// The miss is not recorded, so a later summary can be smaller.
	// The request is summarized only when the layout leaves the view
	// at or over the window. A fitting layout keeps it.
	var compacted bool
	if est, limit := a.outgoingViewEstimate(a.rebuildView()), a.Context.ContextLimit(); a.Context.CompactLastResort() == "condense" && limit > 0 && est < limit {
		outcome, condErr := a.condensePreservedTail(ctx, h)
		if condErr != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
		}
		if outcome == condensePublished && a.outgoingViewEstimate(a.rebuildView()) < limit {
			a.overflowRetries.Add(1)
			return true, nil
		}
		if outcome == condenseAttemptMissed || outcome == condenseGuarded {
			// The larger preserved message was not published. A miss
			// is not recorded, so a later summary can be smaller. A
			// summarizer error is recorded on the overflow attempt.
			// Either way the smaller middle is not summarized and the
			// retry is not spent: that would send the large message
			// again, and the next refusal would end the turn.
			return false, a.overflowTerminalError(err)
		}
	}
	if !a.leaveForCondensation() && !a.singleSummaryFailed() {
		compacted = a.runForcedSummarization(ctx, h, false)
	}
	if ctx.Err() != nil {
		// Cancelled during the forced pass: do not condense on the
		// cancelled context, and do not spend the retry.
		return false, ctx.Err()
	}
	if compacted {
		// The history changed and the estimate is under the window.
		// Send it again. A shrink that is still over does not spend
		// the only retry: last resort runs on that history below, and
		// the retry is spent only when the result measures under.
		if limit := a.Context.ContextLimit(); limit > 0 && a.outgoingViewEstimate(a.rebuildView()) < limit {
			a.overflowRetries.Add(1)
			return true, nil
		}
	}
	view := a.rebuildView()
	// The forced compaction did not change the history. Last resort
	// condenses the one message whose published replacement fits. The
	// retry is spent when that replacement was published and measured
	// under the window. A condensation that publishes nothing ends the
	// turn.
	outcome, condErr := a.lastResortCondense(ctx, h, view)
	if condErr != nil {
		// Error mode returns the actionable diagnostic. A cancel returns
		// ctx.Err() and does not spend the retry or arm the guard.
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		a.finishStillOver(condErr)
		return false, condErr
	}
	if outcome == condensePublished {
		view = a.rebuildView()
		if a.outgoingViewEstimate(view) < a.Context.ContextLimit() {
			a.overflowRetries.Add(1)
			return true, nil
		}
	}
	if outcome == condenseAttemptMissed {
		changed, layErr := a.layoutAfterMissedCondensation(ctx, h)
		if layErr != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, a.overflowTerminalError(err)
		}
		if changed && a.outgoingViewEstimate(a.rebuildView()) < a.Context.ContextLimit() {
			a.overflowRetries.Add(1)
			return true, nil
		}
	}
	a.finishStillOver(fmt.Errorf("still over the context window after overflow recovery"))
	return false, a.overflowTerminalError(err)
}

// layoutAfterMissedCondensation runs the deep-dive layout after a
// condensation summary came back too big to publish. leaveForCondensation
// is still true, so the ordinary forced pass returns before the layout.
// This pass keeps the request. It does not repeat at a message count the
// emergency guard already records: that guard is set when this attempt
// gives up, and a new message re-arms it. A cancel is returned. A
// summarizer error is recorded here.
func (a *Agent) layoutAfterMissedCondensation(ctx context.Context, h *llm.StreamHandlers) (bool, error) {
	if a == nil || a.Context == nil || !a.Context.DeepDive() {
		return false, nil
	}
	a.statsMu.RLock()
	tried := a.compactionGuards.emergency >= len(a.Messages)
	a.statsMu.RUnlock()
	if tried {
		return false, nil
	}
	if h != nil && h.OnCompacting != nil {
		h.OnCompacting()
	}
	changed, err := a.compactNoTail(ctx, contextmgr.CompactOptions{Forced: true, SingleSummary: false})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		a.noteCompactFailure(err)
		a.noteProgressFailure(&a.compactionGuards.emergency)
		return false, nil
	}
	return changed, nil
}

// overflowTerminalError builds the actionable terminal error for an
// exhausted Phase 3 recovery: the current size vs the window, what was
// tried, and the /compact and /new suggestions. The original refusal is
// wrapped (never masked) so the provider's own diagnostic stays in the
// chain.
func (a *Agent) overflowTerminalError(cause error) error {
	limit := 0
	if a.Context != nil {
		limit = a.Context.ContextLimit()
	}
	size := a.compactionTokenTotal()
	sizeStr := "unknown"
	if size >= 0 {
		sizeStr = strconv.Itoa(size)
	}
	return fmt.Errorf("context window exceeded: the outgoing request is ~%s tokens but the model window is %d; forced compaction (cap oversized tool results, then summarize the old history) was tried without success. Try /compact to shrink the history, or /new to start a fresh session: %w", sizeStr, limit, cause)
}

// historyChanged reports whether compaction replaced part of before.
// A one-message summary keeps the count and changes the summary text.
// An unchanged history has the same count and the same summary text.
func historyChanged(before, after []llm.Message) bool {
	return len(before) != len(after) || summaryBlob(before) != summaryBlob(after)
}

// compactToFit runs one auto-compaction pass and, when the post-compaction
// request is still over the compaction budget, repeats it with a shrinking
// preserved tail — keep, keep/2, 1, 0 — up to 3 additional summarization
// calls per turn (port of the harness compactionRetries loop). Each accepted
// pass is a smaller summary (the smaller-summary guard), so the loop always
// terminates. An unchanged result is not published, and a smaller keep is
// tried. Extra passes happen only when the first pass failed to reach the
// budget (rare); when the first pass already fits, the behavior is exactly
// the single pass.
//
// Giving up on a summarizer error records a compact failure so the next
// turn's attempt is backed off. An exhausted sequence that is still over
// budget does the same, except a wait that is already running is left as
// it is. A wait that has already ended is recorded again with the doubled
// delay. The emergency guard is set on that give-up when the total is
// still at or above the hard limit, or the view is over the window.
// Deep dive also sets it, so a miss of the hard limit is not retried at
// the same count. In deep dive,
// a middle under the minimum size, a summary that was not smaller, and a
// result that wrote nothing are not that failure while a smaller keep
// remains: the next keep takes in what this one left in the tail. Default
// mode stops on those errors, as it did before deep dive, so the message
// just sent is not summarized to get past a short middle. A cancel is not
// a failure.
func (a *Agent) compactToFit(ctx context.Context, pinned map[int]struct{}, counts []int, keep int, emergency bool) {
	// The request a condensation kept is still verbatim and the view is
	// under the window. Summarizing it is refused, and recording a
	// failure would block the normal tier. A tool result does not
	// change this. Over the window the tier still runs.
	if emergency && a.keptRequestUnderWindow() {
		return
	}
	// Shrinking-tail sequence: the chosen keep first, then keep/2, 1, 0.
	// Duplicates are dropped so every retry preserves strictly fewer recent
	// messages (a keep of 1 or 0 already leaves at most one / no retries).
	keeps := []int{keep}
	for _, k := range []int{keep / 2, 1, 0} {
		if k < keeps[len(keeps)-1] {
			keeps = append(keeps, k)
		}
	}
	budget := a.Context.CompactBudget()
	// fits is this tier's goal. The normal tier stops at the budget.
	// Deep dive's emergency tier stops at the hard limit: the budget
	// belongs to the normal tier, and a failure recorded here would
	// arm the backoff and suppress that tier. Context mode off stops
	// at the budget.
	fits := func(total int) bool {
		if budget <= 0 || total < budget {
			return true
		}
		// Deep dive stops at the hard limit. The budget belongs to the
		// normal tier, and a failure recorded here would arm the backoff.
		// Context mode off keeps shrinking to the budget.
		return emergency && a.Context.DeepDive() && total < a.Context.HardLimit()
	}
	for i, k := range keeps {
		// Ask before Compact, on the emergency tier and the normal tier.
		// A cold token cache makes the emergency tier look idle, and the
		// normal tier would otherwise publish. Publishing any summary can
		// drop the view under the window while it stays over the hard
		// limit, and the last keep would then summarize the request. The
		// message whose condensation would fit is left for preflight.
		// The emergency guard stays clear: arming it here would make
		// preflight skip the layout when that summary comes back too big.
		if a.leaveForCondensation() {
			return
		}
		// k=0 means "no tail" — map to NoTailKeep so the zero-value
		// CompactOptions.Keep (use default) is not triggered.
		keepOpt := k
		if keepOpt == 0 {
			keepOpt = contextmgr.NoTailKeep
		}
		compacted, newPins, err := a.Context.Compact(ctx, a.Messages, contextmgr.CompactOptions{
			ViewPrefix:   a.systemPromptPrefix(),
			Counts:       counts,
			Pinned:       pinned,
			Keep:         keepOpt,
			Tools:        a.llmTools(),
			AllowedTools: a.AllowedToolNames(),
		})
		plain := false
		if err != nil {
			if ctx.Err() != nil {
				// Cancelled during this keep. Recording it would back
				// off the next turn and set the emergency guard.
				return
			}
			// In deep dive, a middle under the minimum, or a summary
			// that was not smaller, is this keep's answer. The next
			// keep puts more of the history into the middle.
			// Recording it here would block that keep. Default mode
			// stops, as it did before deep dive: continuing would
			// summarize the message just sent. A summarizer failure
			// still stops the sequence in both modes. On the last
			// keep, deep dive's not-smaller refusal still reaches
			// the single summary below. Default mode sets the
			// emergency guard and returns, as it did before.
			retryable := a.Context.DeepDive() && (errors.Is(err, contextmgr.ErrNotEnoughHistory) || errors.Is(err, contextmgr.ErrSummaryNotSmaller))
			if retryable && i < len(keeps)-1 {
				continue
			}
			if !(a.Context.DeepDive() && i == len(keeps)-1 && errors.Is(err, contextmgr.ErrSummaryNotSmaller)) {
				a.noteCompactFailure(err)
				if emergency {
					a.noteProgressFailure(&a.compactionGuards.emergency)
				}
				return
			}
			plain = true
		} else if historyChanged(a.Messages, compacted) {
			// Publish with the post-compaction bookkeeping. The fresh
			// counts are carried into the next pass (a nil cache here
			// would make the next turn's shouldCompactUsingCounts fall
			// back to a full EstimateTokens pass; the cache is otherwise
			// only backfilled by ContextStats/doPersist), and the remapped
			// pins become the next pass's input. An unchanged result is
			// not published: publishing would drop the provider token
			// count, and the local estimate can then look under the
			// budget while a smaller keep remains.
			counts = a.publishCompaction(compacted, newPins)
			pinned = newPins
			if fits(a.compactionTokenTotal()) {
				return
			}
		} else if i < len(keeps)-1 {
			continue
		} else {
			plain = true
		}
		if i == len(keeps)-1 {
			// In deep dive the layout around the request can shrink the
			// history and still leave a request verbatim. The emergency
			// tier summarizes that request on the last keep, including
			// when this keep wrote nothing: the smaller keeps are
			// exhausted. It does not, when the view is over the window
			// and another message's condensation would fit, including
			// a message this keep does not summarize. That message is
			// left for preflight. A request a condensation kept is
			// summarized once the view is over the window: the
			// under-window case already returned. A history that is
			// already the single summary is left to the failure below.
			if emergency && a.leaveForCondensation() {
				return
			}
			if emergency && a.Context.DeepDive() && !a.singleSummaryFailed() && (layoutKeptRequest(a.Messages) || (plain && requestAfterHead(a.Messages))) {
				// Under the window, last resort does not run, so the
				// request is summarized.
				shrank, err := a.compactSingleSummary(ctx, false)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					a.noteCompactFailure(err)
					a.noteProgressFailure(&a.compactionGuards.emergency)
					a.noteSingleSummaryFailure()
					return
				}
				if shrank {
					if fits(a.compactionTokenTotal()) {
						return
					}
				}
			}
			// Exhausted the shrinking-tail sequence and this tier's goal
			// is still missed. A wait that is still running is left as
			// it is: nothing published means the earlier wait was not
			// cleared, and lengthening it would stack two waits. A
			// deadline that has already passed is recorded again with
			// the doubled delay. The emergency guard is set when
			// this tier's trigger is still true: the total is at or
			// above the hard limit, or the view is over the window.
			// Deep dive also sets it, so a miss of the hard limit is
			// not retried at the same count. The emergency tier
			// ignores the backoff, so a clear guard would call the
			// summarizer again on the next turn and double the wait.
			total := a.compactionTokenTotal()
			a.statsMu.RLock()
			backoffActive := !time.Now().After(a.compactBackoffUntil)
			a.statsMu.RUnlock()
			if !backoffActive {
				a.noteCompactFailure(fmt.Errorf("still over budget after %d compaction passes (%d tokens, budget %d)", i+1, total, budget))
			}
			limit := a.Context.ContextLimit()
			hard := a.Context.HardLimit()
			overWindow := limit > 0 && a.outgoingViewEstimate(a.rebuildView()) >= limit
			overHard := hard > 0 && total >= hard
			if emergency && (overWindow || overHard || a.Context.DeepDive()) {
				a.noteProgressFailure(&a.compactionGuards.emergency)
			}
			return
		}
	}
}

// systemPromptPrefix returns the system/enrichment messages that precede
// canonical history on the wire (the view minus a.Messages). CompactPinned
// prepends these to the summarization request so the conversation prefix is
// byte-identical to the previous turn and provider prompt caching applies.
// Built without copying the history: the prefix is either empty (the history
// already carries a system message that buildSystemView enriches in place)
// or the single prepended system message.
func (a *Agent) systemPromptPrefix() []llm.Message {
	if len(a.Messages) == 0 {
		return nil
	}
	for _, m := range a.Messages {
		if m.Role == "system" {
			return nil
		}
	}
	return []llm.Message{{
		Role: "system",
		Content: a.systemPromptBase(a.WorkingDir) +
			buildSystemSuffix(a.ProjectFilePath, a.EffectiveGuidelines(), a.ensureProjectProfile(), a.Mode, a.mcpToolHint()),
	}}
}

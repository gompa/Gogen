package contextmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// segmentResult is one deep-dive segment after compaction: its replacement
// messages, whether a new summary was written, and skip when a summary was
// written but refused as not smaller (nil when the segment was left alone).
//
// recap and tokens carry the written summary's UNFRAMED text and the token
// weight of the region it replaces, so the caller can re-frame it: the state
// manifest goes to whichever region actually produced a summary (see
// compactAroundRequest), and re-framing has to apply the same smaller-summary
// guard compactSegment applied.
type segmentResult struct {
	msgs       []llm.Message
	summarized bool
	skip       error
	recap      string
	tokens     int
}

// deepDiveRefusal is a deep-dive compaction that summarized neither region
// because a summary came back no smaller. Unlike a summarizer failure, the
// single-summary layout may still succeed.
type deepDiveRefusal struct{ error }

func (r deepDiveRefusal) Unwrap() error { return r.error }

// compactAroundRequest is the deep-dive layout (Settings.DeepDive). The latest
// real user request at reqIdx stays verbatim, and the two regions around it
// are summarized separately:
//
//	pre-head, first real request, session summary (what came before the
//	request), the request, progress summary (the work since), tail
//
// A region that already is exactly one summary of its kind is left alone, so
// during one long turn only the progress summary is rewritten and the prefix
// up to the request stays byte-identical (prompt cache, no repeated
// re-summarizing of older history). A new request moves everything before it
// into the session summary at the next compaction.
//
// Each region keeps the single-summary guards: the minimum size unless
// forced and the smaller-summary guard. The deterministic state manifest is
// rendered into the progress summary when that region produced one — it is
// the live region, rewritten on every deep-dive compaction, while the session
// summary is kept byte-identical (isOnlySummary) so a block there would freeze
// at the moment it was written. If the progress region produced no summary,
// the session summary carries the block instead: a session-region-only
// compaction would otherwise drop every handle it summarized away. When
// neither region produced a summary, the history comes back unchanged, or
// refused when a summary was not smaller. A session summary that succeeded
// is kept when the progress call fails: the work since the request stays
// verbatim and the caller measures the result. A session call that fails
// for any reason other than cancel still summarizes the progress region.
// Whichever region succeeded is published. A cancel drops both.
// wrote reports whether a summary was written; the input slice is returned
// when it was not.
func (m *Manager) compactAroundRequest(ctx context.Context, messages []llm.Message, headIdx, reqIdx, tailStart int, opts CompactOptions) ([]llm.Message, map[int]struct{}, bool, error) {
	// One state for both regions: the manifest is cumulative, so the session
	// region's handles must be in it when the progress summary renders it. A
	// nil State (a direct Manager user) becomes one per-compaction state here
	// rather than one per segment, so the two regions still share it.
	state := opts.State
	if state == nil {
		state = NewSessionState()
		opts.State = state
	}
	before, sessionErr := m.compactSegment(ctx, messages, headIdx+1, reqIdx, SummaryPrefix, sessionSummaryInstruction, opts)
	if sessionErr != nil {
		if ctx.Err() != nil {
			return nil, nil, false, sessionErr
		}
		before = segmentResult{msgs: cloneMessages(messages[headIdx+1 : reqIdx])}
	}
	since, err := m.compactSegment(ctx, messages, reqIdx+1, tailStart, ProgressSummaryPrefix, progressSummaryInstruction, opts)
	if err != nil {
		if ctx.Err() != nil || !before.summarized {
			return nil, nil, false, err
		}
		since = segmentResult{msgs: cloneMessages(messages[reqIdx+1 : tailStart])}
	}
	// The progress region produced no summary (left verbatim, refused as not
	// smaller, or the summarizer failed), so this compaction has nowhere live
	// to render the manifest: without the re-frame below a session-region-only
	// compaction wrote NO state block at all and every handle the summarized
	// span created became unaddressable — the exact loss the manifest exists to
	// prevent, and something the per-region executed-call roster it replaced did
	// cover. The block is a snapshot from here on (the session region is not
	// re-summarized until a new request moves it), which is the price of not
	// dropping the handles; a later progress summary renders a fresh one, and
	// SessionState.Fold imports the newest block last (see importBlock).
	if before.summarized && !since.summarized {
		if framed, gerr := m.frameSegmentSummary(SummaryPrefix, before.recap, before.tokens, state, true); gerr == nil {
			before.msgs = []llm.Message{framed}
		}
	}
	if !before.summarized && !since.summarized {
		if sessionErr != nil {
			return nil, nil, false, sessionErr
		}
		// A summary that came back no smaller is returned as an error, as in
		// the single-summary layout. The fit loop tries a smaller tail
		// before it backs off. Otherwise nothing was attempted: the
		// regions are single summaries or under the minimum size, the
		// normal state shortly after a compaction. The history then comes
		// back unchanged with no error, so the caller tries a smaller tail.
		for _, seg := range []segmentResult{since, before} {
			if errors.Is(seg.skip, ErrSummaryNotSmaller) {
				return nil, nil, false, deepDiveRefusal{seg.skip}
			}
		}
		return messages, copyIntSet(opts.Pinned), false, nil
	}

	tail := cloneMessages(messages[tailStart:])
	compact := make([]llm.Message, 0, headIdx+1+len(before.msgs)+1+len(since.msgs)+len(tail))
	compact = append(compact, cloneMessages(messages[:headIdx+1])...)
	compact = append(compact, before.msgs...)
	compact = append(compact, cloneMessage(messages[reqIdx]))
	compact = append(compact, since.msgs...)
	compact = append(compact, tail...)
	return compact, remapPinsAfterCompact(opts.Pinned, headIdx, tailStart, len(compact)-len(tail)), true, nil
}

// compactSegment summarizes messages[start:end] into one message framed with
// prefix, using instruction. The summarization request carries messages[:start]
// as context, which is the wire history up to this region, so the provider's
// prompt cache covers it. For the progress region that is the original
// history up to and including the request, unsummarized: the same size as
// the single-summary request, and when it does not fit the summary budget,
// summarizeMiddle uses its flattened fallback, which the prompt cache does
// not cover. Only a summarizer failure is returned as err. A region
// planSegment leaves alone comes back unchanged; one whose summary would not
// shrink comes back unchanged with skip set.
func (m *Manager) compactSegment(ctx context.Context, messages []llm.Message, start, end int, prefix, instruction string, opts CompactOptions) (segmentResult, error) {
	seg := messages[start:end]
	plan := m.planSegment(messages, opts.Counts, start, end, prefix, m.minMiddleTokens, opts.Forced)
	unchanged := segmentResult{msgs: cloneMessages(seg)}
	if !plan.summarize {
		return unchanged, nil
	}
	segTokens := plan.tokens
	knownTokens := -1
	if opts.Counts != nil && len(opts.Counts) >= end {
		knownTokens = middleTokensBetween(messages, opts.Counts, 0, end)
	}

	summary, err := m.summarizeMiddle(ctx, opts.ViewPrefix, messages[:start], seg, knownTokens, opts.Tools, opts.AllowedTools, instruction)
	if err != nil {
		return segmentResult{}, err
	}
	// Fold every summarized region into the session state: the cumulative
	// memory spans both regions. The manifest itself is rendered into the
	// progress summary — the LIVE region, rewritten on every deep-dive
	// compaction — because a block written into the session summary freezes
	// with it (isOnlySummary skips that region from then on), so it would
	// report live handles as of the moment it was written. When the progress
	// region produced no summary this call, compactAroundRequest re-frames the
	// session summary with the block instead: dropping it there would lose
	// every handle the summarized span created.
	state := opts.State
	if state == nil {
		state = NewSessionState()
	}
	state.Fold(seg)
	framed, gerr := m.frameSegmentSummary(prefix, summary, segTokens, state, prefix == ProgressSummaryPrefix)
	if gerr != nil {
		unchanged.skip = gerr
		return unchanged, nil
	}
	return segmentResult{msgs: []llm.Message{framed}, summarized: true, recap: summary, tokens: segTokens}, nil
}

// frameSegmentSummary frames one region's summary into the message that
// replaces it and, when withState is set, appends the deterministic state
// manifest. The smaller-summary guard is applied to the RESULT, so an
// over-long manifest is refused here exactly like an over-long recap (a
// replacement larger than the region it replaces would break the fit loop's
// shrinking guarantee).
func (m *Manager) frameSegmentSummary(prefix, recap string, regionTokens int, state *SessionState, withState bool) (llm.Message, error) {
	content := prefix + recap
	if withState {
		if block := m.renderState(state, stateBudget(regionTokens)); block != "" {
			content += "\n\n" + block
		}
	}
	framed := llm.Message{Role: "assistant", Content: content}
	if m.requireSummaryShrink {
		if framedTokens := m.EstimateTokens([]llm.Message{framed}); framedTokens >= regionTokens {
			return llm.Message{}, fmt.Errorf("%w (summary %d tokens, region %d tokens)", ErrSummaryNotSmaller, framedTokens, regionTokens)
		}
	}
	return framed, nil
}

// segmentPlan is what a deep-dive compaction does with one region: whether
// it is summarized, and its token weight with images excluded (0 when the
// region is left alone before it is measured).
type segmentPlan struct {
	summarize bool
	tokens    int
}

// planSegment holds the region rules shared by compactSegment and
// deepDiveFreeable, so the emergency sizing frees what the compaction does.
// An empty region and a region that already is one summary of its kind are
// left alone, and so is a region under minTokens unless forced. The weight
// comes from counts when they cover the region, otherwise from an estimate.
func (m *Manager) planSegment(messages []llm.Message, counts []int, start, end int, prefix string, minTokens int, forced bool) segmentPlan {
	seg := messages[start:end]
	if len(seg) == 0 || isOnlySummary(seg, prefix) {
		return segmentPlan{}
	}
	var tokens int
	if counts != nil && len(counts) >= end {
		tokens = middleTokensBetween(messages, counts, start, end)
	} else {
		tokens = m.EstimateTokens(seg)
		for _, msg := range seg {
			tokens -= imageTokenEstimate(msg)
		}
	}
	return segmentPlan{summarize: forced || tokens >= minTokens, tokens: tokens}
}

// isOnlySummary reports whether seg is exactly one summary framed with prefix.
func isOnlySummary(seg []llm.Message, prefix string) bool {
	return len(seg) == 1 && strings.HasPrefix(seg[0].Content, prefix)
}

// deepDiveFreeable is the summarizable token weight of messages[headIdx+1:tailStart]
// for an emergency compaction in deep dive, sized as compactPinned will run
// it. The verbatim request, and a region planSegment leaves alone (already
// one summary of its kind, or under minTokens), free nothing.
// A split where no region would be summarized, or a summarized region
// would not drop a message, counts as freeing nothing, so the keep is
// lowered until a split frees enough. The compaction still returns a
// one-message summary, and the caller measures it. When a split writes
// nothing, the caller shrinks the tail. The request is summarized on the
// last keep when the total is still at or above the hard limit, except
// when the view is also over the window and another message's condensation
// would fit. Pre-flight summarizes it when the estimate is still over the
// window, the request is still verbatim, and no such condensation fits. Overflow
// recovery skips the layout and summarizes the request in its one pass,
// because the provider refused. The caller
// budgets one summary (EmergencySummaryAllowance); when both regions are
// summarized, the second summary's allowance is charged here.
func (m *Manager) deepDiveFreeable(messages []llm.Message, counts []int, headIdx, tailStart, minTokens int) int {
	reqIdx := deepDiveRequestIndex(messages, headIdx, tailStart)
	if reqIdx < 0 {
		return middleTokensBetween(messages, counts, headIdx+1, tailStart)
	}
	summaries, dropped, freed := 0, 0, 0
	region := func(start, end int, prefix string) {
		plan := m.planSegment(messages, counts, start, end, prefix, minTokens, false)
		if !plan.summarize {
			return
		}
		summaries++
		dropped += end - start - 1
		freed += plan.tokens
	}
	region(headIdx+1, reqIdx, SummaryPrefix)
	region(reqIdx+1, tailStart, ProgressSummaryPrefix)
	if summaries == 0 || dropped == 0 {
		return 0
	}
	if summaries > 1 {
		freed -= EmergencySummaryAllowance
	}
	return freed
}

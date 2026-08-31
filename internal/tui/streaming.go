package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/debuglog"
	"gogen/internal/llm"
	"gogen/internal/streamutil"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// --- Bubble Tea messages for streaming ---

// Every streaming message carries its owning session id (sid) and the
// turn generation (seq) it was produced by; see streamEventAttribution
// and liveSession.turnSeq for the drop rules.

// streamStartMsg is sent when the agent first begins processing.
type streamStartMsg struct {
	sid string
	seq uint64
}

// streamRoundStartMsg is sent at the start of each LLM round after the first.
type streamRoundStartMsg struct {
	sid string
	seq uint64
}

type streamTokenMsg struct {
	token string
	sid   string
	seq   uint64
}

type streamThinkingMsg struct {
	token string
	sid   string
	seq   uint64
}

type streamToolCallMsg struct {
	index int
	id    string
	name  string
	sid   string
	seq   uint64
}

type streamToolCallArgsMsg struct {
	index int
	id    string
	delta string
	sid   string
	seq   uint64
}

type streamToolResultMsg struct {
	id      string
	name    string
	result  string
	success bool
	sid     string
	seq     uint64
}

// streamToolCallFinalMsg is sent when tool call args are fully parsed.
type streamToolCallFinalMsg struct {
	index int
	tc    llm.ToolCall
	sid   string
	seq   uint64
}

// streamToolExecuteMsg is sent immediately before a tool runs.
type streamToolExecuteMsg struct {
	name string
	sid  string
	seq  uint64
}

// streamRoundEndMsg is sent at the end of each streaming round
// (including intermediate tool-call rounds). It resets buffers
// but does NOT set streaming=false.
type streamRoundEndMsg struct {
	sid string
	seq uint64
}

// streamEndMsg is sent when all streaming is complete (final message from
// goroutine). sid attributes the turn to its owning live session so a
// background session's finish never mutates the focused transcript; seq
// attributes it to the turn generation so a superseded turn's late
// terminal (cancel + resubmit) is dropped instead of clobbering the new
// turn's state.
type streamEndMsg struct {
	sid string
	seq uint64
}

type streamErrorMsg struct {
	err error
	sid string
	seq uint64
}

// condensedNoteMsg carries the last-resort condensation announcement
// (Phase 0e) for rendering as a system line.
type condensedNoteMsg struct {
	note string
	sid  string
	seq  uint64
}

type contextStatsMsg struct {
	stats agent.TurnContext
	// sid is the session the probe was taken for, captured on the Update
	// thread at request time: the probe runs off-thread, so a slow probe
	// for a session that lost focus before it landed must not overwrite
	// the focused session's indicator (handler drops it).
	sid string
}

// compactResultMsg carries the outcome of an async /compact (cmdCompact)
// back to the Update thread. agent is the compacted session — the focused
// one may have changed while the summarization ran. seq identifies the
// compaction generation: a result from a SUPERSEDED run (cancelled, then
// restarted before the old goroutine finished unwinding) is dropped — it
// must neither clear the new run's flags nor report the old cancellation.
type compactResultMsg struct {
	agent *agent.Agent
	err   error
	seq   uint64
}

// modelListMsg carries the async /models list result back to the Update
// thread. agent is the listing session — the focused one may have changed
// while the provider round trip ran (the list would then describe the
// wrong endpoint).
type modelListMsg struct {
	agent *agent.Agent
	list  []llm.ModelInfo
	err   error
}

// modelSwitchMsg carries the async model-switch result (inline
// /models <sel> or the models modal selection) back to the Update thread.
type modelSwitchMsg struct {
	agent *agent.Agent
	out   string
	err   error
	// thinking is the models modal's staged thinking level, applied AFTER
	// a successful switch ("" = none, e.g. the inline /models <sel>
	// path). The modal validated it against the new model's accepted set
	// before the switch ran; applying it post-switch makes
	// HandleThinkingCommand's validation run against that same model.
	thinking agent.ThinkingLevel
}

// savedSessionsMsg carries an async persisted-session index snapshot
// (requestSavedSessions) back to the Update thread.
type savedSessionsMsg struct {
	seq      uint64
	sessions []agent.SessionInfo
	ok       bool
}

// programSender is the minimal program surface the adapter needs;
// *tea.Program satisfies it, and tests may record sends instead.
type programSender interface {
	Send(msg tea.Msg)
}

// StreamAdapter adapts llm.StreamHandlers to emit Bubble Tea messages
// that can be processed by the Model's Update method.
type StreamAdapter struct {
	program programSender
	// owner is the live-session id whose turn this adapter renders.
	owner string
	// seq is the turn generation this adapter was created for; every
	// emitted message carries it so the Update thread can drop stragglers
	// from a superseded turn (see liveSession.turnSeq).
	seq uint64
	// sess is the owning live session. Focused → events go to the
	// program; background → they are buffered for replay when focus
	// arrives (the turn keeps running either way; completion stays
	// attributed via sid).
	sess *liveSession
}

// NewStreamAdapter creates a new StreamAdapter for one live session's turn.
// seq must be the session's turnSeq at submit time (the submit paths
// increment it before constructing the adapter).
func NewStreamAdapter(owner string, seq uint64, p programSender, sess *liveSession) *StreamAdapter {
	return &StreamAdapter{program: p, owner: owner, seq: seq, sess: sess}
}

// send emits a rendering message, or buffers it while the session streams
// in the background.
func (s *StreamAdapter) send(msg tea.Msg) {
	if s.sess != nil && !s.sess.focused.Load() {
		s.sess.enqueue(msg)
		return
	}
	s.program.Send(msg)
}

// Handlers returns a full set of stream handlers that emit tea.Msg values.
func (s *StreamAdapter) Handlers() *llm.StreamHandlers {
	tuiSend := func(think bool, text string) {
		if think {
			s.send(streamThinkingMsg{token: text, sid: s.owner, seq: s.seq})
		} else {
			s.send(streamTokenMsg{token: text, sid: s.owner, seq: s.seq})
		}
	}
	batch := streamutil.NewTokenBatcher(tuiSend, 32*time.Millisecond)
	// Tool-call args arrive as a high-rate delta stream (one message per
	// provider chunk); without coalescing the Update thread would do a
	// transcript update per chunk. The batcher concatenates per-index
	// deltas and flushes on a timer — the accumulated-args bookkeeping in
	// handleStreamToolArgs is unaffected (it sums the same bytes).
	// Shared with the WebSocket frontend (streamutil.ArgsBatcher) so both
	// coalesce identically.
	argsBatch := streamutil.NewArgsBatcher(func(index int, id, _ string, delta string) {
		s.send(streamToolCallArgsMsg{index: index, id: id, delta: delta, sid: s.owner, seq: s.seq})
	}, 32*time.Millisecond)

	// The round-buffer feeds (s.sess.round*) run on EVERY event regardless
	// of focus — unlike send, which only enqueues while unfocused. The
	// buffer must accumulate from round start even while the session is
	// the focused one: that is the gap it closes (watch A stream, switch
	// away, come back mid-round — web liveTurnState parity). The nil
	// guards keep adapters built without a session (unit tests) quiet.
	return &llm.StreamHandlers{
		OnStart: func() {
			if s.sess != nil {
				s.sess.roundReset() // new turn: fresh buffer
			}
			batch.Reset()
			argsBatch.Reset()
			s.send(streamStartMsg{sid: s.owner, seq: s.seq})
		},
		OnCondensed: func(note string) {
			s.send(condensedNoteMsg{note: note, sid: s.owner, seq: s.seq})
		},
		OnRoundStart: func() {
			if s.sess != nil {
				s.sess.roundReset() // new round: completed content is in Messages
			}
			batch.Reset()
			argsBatch.Reset()
			s.send(streamRoundStartMsg{sid: s.owner, seq: s.seq})
		},
		OnThinkingToken: func(token string) {
			if s.sess != nil {
				s.sess.roundAppendThinking(token)
			}
			batch.ThinkToken(token)
		},
		OnToken: func(token string) {
			if s.sess != nil {
				s.sess.roundAppendContent(token)
			}
			batch.StreamToken(token)
		},
		OnStreamEnd: func() {
			batch.Flush()
			batch.Close()
			argsBatch.Flush()
			argsBatch.Close()
			if s.sess != nil {
				// Round complete: the assistant message is appended to
				// Messages immediately after, so the buffer only ever
				// carries content a history snapshot would otherwise miss.
				s.sess.roundReset()
			}
			s.send(streamRoundEndMsg{sid: s.owner, seq: s.seq})
		},
		OnToolCallStart: func(index int, id, name string) {
			batch.Flush()
			argsBatch.Flush()
			if s.sess != nil {
				s.sess.roundToolStart(index, id, name)
			}
			s.send(streamToolCallMsg{index: index, id: id, name: name, sid: s.owner, seq: s.seq})
		},
		OnToolCallArgsDelta: func(index int, id, name, argsDelta string) {
			argsBatch.Add(index, id, name, argsDelta)
			if s.sess != nil {
				s.sess.roundToolArgsAppend(index, argsDelta)
			}
		},
		OnToolCall: func(tc llm.ToolCall) {
			// Deliver any buffered args deltas before the final so the
			// Update thread sees the complete argument accumulation first.
			argsBatch.Flush()
			s.send(streamToolCallFinalMsg{index: tc.Index, tc: tc, sid: s.owner, seq: s.seq})
		},
		OnToolExecute: func(name string) {
			argsBatch.Flush()
			s.send(streamToolExecuteMsg{name: name, sid: s.owner, seq: s.seq})
		},
		OnToolResult: func(id, name, result string, success bool) {
			s.send(streamToolResultMsg{id: id, name: name, result: result, success: success, sid: s.owner, seq: s.seq})
		},
		OnRecoverPartialStream: func() {},
	}
}

// appendChatLine adds a line to the chat buffer and updates the viewport.
func (m *Model) appendChatLine(line string) {
	// Move the current last line's wrapping into the prefix so that only
	// the *new* last line needs re-wrapping on subsequent updates.
	if len(m.chatLines) > 0 {
		parts := m.wrapLine(m.chatLines[len(m.chatLines)-1])
		wrapped := strings.Join(parts, "\n")
		// wrappedPrefix already ends with "\n" (or is empty on the first
		// line); appending the wrapped line keeps a single separator.
		m.wrappedPrefix += wrapped + "\n"
		// Keep the incremental viewport prefix in sync (see buildFromPrefix).
		m.prefixLines = append(m.prefixLines, strings.Split(wrapped, "\n")...)
	}
	m.chatLines = append(m.chatLines, line)
	if isUserPromptLine(line) {
		m.tocAppendAnchor(line) // prompt rail: the web's appendTocDot
	}
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

// appendToLastLine appends text to the last line in the chat buffer.
func (m *Model) appendToLastLine(text string) {
	if len(m.chatLines) == 0 {
		m.appendChatLine(text)
		return
	}
	// Only the last line changes — prefix stays unchanged.
	m.chatLines[len(m.chatLines)-1] += text
	m.tocRefreshLastPrompt()
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

// replaceLastLine replaces the last line in the chat buffer.
func (m *Model) replaceLastLine(text string) {
	if len(m.chatLines) == 0 {
		m.chatLines = append(m.chatLines, text)
	} else {
		m.chatLines[len(m.chatLines)-1] = text
	}
	m.tocRefreshLastPrompt()
	// Prefix is unchanged; only the last line may have been replaced.
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamToken(token string) {
	m.closeThinkingBlock()
	if m.streamAssistantBuf.Len() == 0 {
		label := AssistantStyle.Render(assistantLabel)
		m.streamAssistantLine = len(m.chatLines)
		m.appendChatLine(label + " ")
	}
	m.streamAssistantBuf.WriteString(token)
	m.appendToStreamLine(m.streamAssistantLine, token)
	m.bumpContextEstimate(token)
}

// renderStyledBlock renders multi-line text with style applied per line.
// lipgloss pads every line (except the widest) with trailing spaces when a
// multi-line string is rendered in a single call; those padding runs make
// wrapLine (reflow wrap) emit spurious blank visual lines, because reflow
// drops every consecutive space after a forced wrap and the content's own
// newline then fires a second line break. Rendering each line separately
// never pads, so the artifact cannot occur.
func renderStyledBlock(style lipgloss.Style, text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = style.Render(l)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) handleStreamThinking(token string) {
	// Guard: if tool calls are already in progress, thinking tokens belong
	// before them (OpenAI protocol ensures this ordering).  Silently ignore
	// post-tool-call thinking to avoid placing it below tool call lines.
	if len(m.streamToolCallNames) > 0 {
		return
	}
	m.streamThinkingBuf.WriteString(token)
	m.bumpContextEstimate(token)

	if !m.streamThinkingOpen {
		m.streamThinkingOpen = true
		m.streamThinkingLine = len(m.chatLines)
		m.appendChatLine(ThinkingTagStyle.Render("<thinking>"))
	}

	// Rebuild the thinking line cleanly from the accumulated buffer.  This
	// avoids two problems with per-delta append+style: (1) interleaved
	// \x1b[0m codes destabilise word-wrap (line height jumps when the block
	// closes and normalises the styling), and (2) whitespace-only tokens
	// that the batcher splits into standalone segments are silently dropped
	// from the display by TrimSpace, only to re-appear after close.
	// When the buffer ends with a newline, wrapLine splits it into an extra
	// blank visual line that will flash away when the next token fills it in
	// or when </thinking> replaces it on close.  Trim trailing newlines so
	// the streaming display stays stable.
	displayBuf := strings.TrimRight(m.streamThinkingBuf.String(), "\n")
	m.replaceStreamLine(m.streamThinkingLine, renderStyledBlock(ThinkingStyle, "<thinking>"+displayBuf))
}

// closeThinkingBlock finalizes an open thinking line in place (not necessarily
// the last chat line — assistant text or tool calls may have been appended).
func (m *Model) closeThinkingBlock() {
	if !m.streamThinkingOpen {
		return
	}
	m.streamThinkingOpen = false
	var line string
	if m.streamThinkingBuf.Len() > 0 {
		line = renderStyledBlock(ThinkingTagStyle, "<thinking>"+m.streamThinkingBuf.String()+"</thinking>")
	} else {
		line = renderStyledBlock(ThinkingTagStyle, "<thinking></thinking>")
	}
	m.replaceStreamLine(m.streamThinkingLine, line)
	m.streamThinkingBuf.Reset()
	m.streamThinkingLine = -1
	// Reset assistant state so content tokens arriving after this thinking
	// block create a new line below it, preserving temporal order.
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
}

// appendToStreamLine appends text to a tracked streaming line. When that line
// is last, uses the cheap prefix rebuild; otherwise full rewrap.
// If the recorded lineIdx is no longer valid (e.g. the chat was modified
// concurrently), fall back to appending to the last line so the token is
// not silently dropped. The mismatch is logged at debug level.
func (m *Model) appendToStreamLine(lineIdx int, text string) {
	if lineIdx < 0 || lineIdx >= len(m.chatLines) {
		debuglog.Write("tui/stream", "appendToStreamLine: slot invalid", "stream-slot-lost", map[string]any{
			"lineIdx":  lineIdx,
			"chatLen":  len(m.chatLines),
			"textSize": len(text),
		})
		m.appendToLastLine(text)
		return
	}
	m.chatLines[lineIdx] += text
	if lineIdx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()
}

// replaceStreamLine replaces text in a tracked streaming line. When that line
// is last, uses the cheap prefix rebuild; otherwise full rewrap. If the
// recorded lineIdx is no longer valid, fall back to the last line and log.
func (m *Model) replaceStreamLine(lineIdx int, text string) {
	if lineIdx < 0 || lineIdx >= len(m.chatLines) {
		debuglog.Write("tui/stream", "replaceStreamLine: slot invalid", "stream-slot-lost", map[string]any{
			"lineIdx":  lineIdx,
			"chatLen":  len(m.chatLines),
			"textSize": len(text),
		})
		m.replaceLastLine(text)
		return
	}
	m.chatLines[lineIdx] = text
	if lineIdx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamToolCall(index int, id string, name string) {
	// Close thinking if open — finalize the block in chat
	m.closeThinkingBlock()
	// Close assistant buffer (text tokens before tool call are shown as-is in chat)
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	m.streamToolCallNames[index] = name
	m.setActiveTool(name)
	m.streamToolCallArgs[index] = ""
	m.streamToolCallIDs[index] = id
	m.streamToolCallLines[index] = len(m.chatLines) // appendChatLine will add at this index
	prefix := ToolCallStyle.Render("  →")
	m.appendChatLine(prefix + " " + name)
}

func (m *Model) handleStreamToolArgs(index int, id string, delta string) {
	lineIdx, ok := m.streamToolCallLines[index]
	if !ok || lineIdx < 0 || lineIdx >= len(m.chatLines) {
		return
	}
	m.streamToolCallArgs[index] += delta
	raw := m.streamToolCallArgs[index]
	m.bumpContextEstimate(delta)

	// For patch_file: progressively render diff content as it streams in.
	// This avoids a jarring "pop-up" of the entire diff block in the result.
	if m.streamToolCallNames[index] == "patch_file" {
		if diff, ok := extractDiffValue(raw); ok && diff != "" {
			rendered := renderDiff(diff)
			renderedLines := strings.Split(rendered, "\n")
			prevCount := m.streamToolDiffCount[index]
			newCount := len(renderedLines)

			// First time we're showing diff lines: add the top border
			if prevCount == 0 {
				m.chatLines = append(m.chatLines, DiffMetaStyle.Render("  ╭─ diff ─"))
				m.streamToolDiffStart[index] = len(m.chatLines)
			}

			diffStart := m.streamToolDiffStart[index]
			// Update existing diff lines (content may have grown within a line)
			for i := 0; i < prevCount && i < newCount; i++ {
				m.chatLines[diffStart+i] = "  " + renderedLines[i]
			}
			// Append new diff lines
			for i := prevCount; i < newCount; i++ {
				m.chatLines = append(m.chatLines, "  "+renderedLines[i])
			}
			m.streamToolDiffCount[index] = newCount

			// Diff lines were appended directly to chatLines (not via
			// appendChatLine), so the incremental prefix is stale: full rebuild.
			m.setViewportContent()
			m.viewport.GotoBottom()
		}
		// Don't try to parse JSON for the args line; diff values are huge and
		// formatToolArgs would just truncate them. Show a clean compact line.
		prefix := ToolCallStyle.Render("  →")
		shortArgs := formatArgsCompact(raw, 120)
		toolName := m.streamToolCallNames[index]
		if shortArgs == "" {
			m.chatLines[lineIdx] = prefix + " " + toolName
		} else {
			m.chatLines[lineIdx] = prefix + " " + toolName + " " + ToolCallArgsStyle.Render(shortArgs)
		}
		if lineIdx == len(m.chatLines)-1 {
			m.buildFromPrefix()
		}
		m.viewport.GotoBottom()
		return
	}

	// Only show args once JSON is fully parseable.  Raw / truncated JSON
	// varies in length enough to cause the line to re-wrap and make
	// content below jump when handleStreamToolCallFinal normalises it.
	args, parseErr := parseInlineJSONArgs(raw)
	if parseErr != nil {
		return
	}

	// Format the same way handleStreamToolCallFinal does, so the line is
	// already in its final form when that fires.  This eliminates the jump
	// entirely for multi-key args and only leaves the minimal name→name+args
	// transition for tools whose last arg key completes the JSON.
	if len(args) == 0 {
		return
	}
	argStr := formatToolArgs(args)

	// Rebuild the line cleanly from the accumulated buffer so there is a
	// single contiguous SGR wrapper.  Per-delta styling produces interleaved
	// \x1b[0m sequences that destabilise word-wrap, causing the line height
	// to jump when handleStreamToolCallFinal normalises the styling.
	name := m.streamToolCallNames[index]
	prefix := ToolCallStyle.Render("  →")
	m.chatLines[lineIdx] = prefix + " " + name + " " + ToolCallArgsStyle.Render(argStr)

	if lineIdx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()
}

// handleStreamToolCallFinal replaces the streaming tool call line with the final
// cleanly-formatted args (from the fully-parsed ToolCall).
func (m *Model) handleStreamToolCallFinal(index int, tc llm.ToolCall) {
	name, ok := m.streamToolCallNames[index]
	if !ok {
		return
	}
	lineIdx, ok := m.streamToolCallLines[index]
	if !ok || lineIdx < 0 || lineIdx >= len(m.chatLines) {
		return
	}
	prefix := ToolCallStyle.Render("  →")
	argStr := formatToolArgs(tc.Args)
	if argStr == "" {
		m.chatLines[lineIdx] = prefix + " " + name
	} else {
		m.chatLines[lineIdx] = prefix + " " + name + " " + ToolCallArgsStyle.Render(argStr)
	}
	if lineIdx == len(m.chatLines)-1 {
		m.buildFromPrefix()
	} else {
		m.setViewportContent()
	}
	m.viewport.GotoBottom()

	// Capture diff content for patch_file calls so we can render it in the result
	// (fallback if progressive rendering didn't cover the full diff).
	if tc.Name == "patch_file" {
		if diff, ok := tc.Args["diff"].(string); ok && diff != "" {
			m.toolCallDiffs[tc.ID] = diff
			// If we already rendered diff lines progressively, close the border
			// and mark so the result handler skips the block render.
			if m.streamToolDiffCount[index] > 0 {
				m.appendChatLine(DiffMetaStyle.Render("  ╰───────"))
				m.toolDiffShown[tc.ID] = true
			}
		}
	}
}

// parseInlineJSONArgs attempts to parse incomplete streaming JSON args.
// Returns the parsed map on success; nil+error when JSON is not yet complete.
func parseInlineJSONArgs(raw string) (map[string]any, error) {
	s := strings.TrimSpace(raw)
	if s == "" || !strings.HasPrefix(s, "{") {
		return nil, fmt.Errorf("incomplete")
	}
	var args map[string]any
	err := json.Unmarshal([]byte(s), &args)
	return args, err
}

func (m *Model) handleStreamToolResult(id string, name string, result string, success bool) {
	// Collect all new lines first, then append in a batch so the viewport
	// rebuilds once instead of on every appendChatLine call.
	var newLines []string
	// The tool finished; the next indicator phase is "thinking" for the next
	// model round (which re-announces any new tool via handleStreamToolCall).
	m.setActiveTool("")

	newLines = append(newLines, toolResultStatusLine(name, success))

	// For patch_file: render the original diff with colors
	showDiffResult := name == "show_diff" && isDiffContent(result)
	if name == "patch_file" {
		// If diff was already shown progressively during arg streaming,
		// skip the full block render.
		if m.toolDiffShown[id] {
			// Summary only — border was already closed in handleStreamToolCallFinal
			if m.verbose {
				for _, line := range strings.Split(result, "\n") {
					newLines = append(newLines, ToolResultBodyStyle.Render("  │ "+line))
				}
			} else {
				summary := summarizeResult(result, success)
				newLines = append(newLines, DimStyle.Render(fmt.Sprintf("  %s", summary)))
			}
		} else if diff, ok := m.toolCallDiffs[id]; ok && diff != "" {
			newLines = append(newLines, diffBlock(diff)...)
		}
	} else if showDiffResult {
		newLines = append(newLines, diffBlock(result)...)
	} else if m.verbose {
		for _, line := range strings.Split(result, "\n") {
			newLines = append(newLines, ToolResultBodyStyle.Render("  │ "+line))
		}
	} else {
		summary := summarizeResult(result, success)
		newLines = append(newLines, DimStyle.Render(fmt.Sprintf("  %s", summary)))
	}

	// If this already came from a diff path, stop — don't double-append.
	if showDiffResult {
		m.appendChatLines(newLines)
		// Account for tool result tokens in the live context estimate even
		// when the visual block was already rendered during arg streaming.
		m.bumpContextEstimate(result)
		return
	}

	m.bumpContextEstimate(result)
	m.appendChatLines(newLines)
}

// appendChatLines adds multiple lines to chat and rebuilds the viewport once.
func (m *Model) appendChatLines(lines []string) {
	if len(lines) == 0 {
		return
	}
	for _, line := range lines {
		if len(m.chatLines) > 0 {
			parts := m.wrapLine(m.chatLines[len(m.chatLines)-1])
			wrapped := strings.Join(parts, "\n")
			m.wrappedPrefix += wrapped + "\n"
			m.prefixLines = append(m.prefixLines, strings.Split(wrapped, "\n")...)
		}
		m.chatLines = append(m.chatLines, line)
		if isUserPromptLine(line) {
			m.tocAppendAnchor(line)
		}
	}
	m.buildFromPrefix()
	m.viewport.GotoBottom()
}

// resetStreamState clears all per-turn streaming buffers and indices. When
// keepToolDiffShown is true the toolDiffShown map is preserved so a patch
// diff rendered in an earlier round is not re-shown in the next round. Pass
// false on new-turn / cancel / error paths to wipe everything.
func (m *Model) resetStreamState(keepToolDiffShown bool) {
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	m.streamThinkingBuf.Reset()
	m.streamThinkingOpen = false
	m.streamThinkingLine = -1
	m.streamToolCallNames = make(map[int]string)
	m.streamToolCallArgs = make(map[int]string)
	m.streamToolCallIDs = make(map[int]string)
	m.streamToolCallLines = make(map[int]int)
	m.setActiveTool("")
	m.toolCallDiffs = make(map[string]string)
	m.streamToolDiffCount = make(map[int]int)
	m.streamToolDiffStart = make(map[int]int)
	if !keepToolDiffShown {
		m.toolDiffShown = make(map[string]bool)
	}
}

func (m *Model) handleStreamStart() {
	m.resetStreamState(false)
	// Snapshot the last authoritative context usage as the baseline for the
	// live streaming estimate (see bumpContextEstimate).
	m.contextStreamBaseUsed = m.contextStats.Snapshot.Used
	m.contextStreamEstAdded = 0
}

func (m *Model) handleStreamRoundStart() {
	m.resetStreamState(true)
	// Refresh the authoritative context stats before re-basing the live
	// estimate. By the time round N+1 starts, recordTurnUsage has stored
	// round N's exact API prompt_tokens and the tool results are already
	// appended to a.Messages, so ContextStats now reports the true
	// pre-round usage (API baseline + local estimates for the appended
	// messages). Re-basing from the stale end-of-previous-turn mirror
	// instead made the (est.) indicator visibly drop back to the
	// pre-reply level at every round boundary of a multi-round
	// (tool-calling) reply. Resetting contextStreamEstAdded keeps the
	// (est.) indicator incrementally accurate across multi-round turns.
	m.refreshContextStatsMidTurn()
	m.contextStreamBaseUsed = m.contextStats.Snapshot.Used
	m.contextStreamEstAdded = 0
}

func (m *Model) handleStreamRoundEnd() {
	// Trim trailing newlines from the assistant content line before
	// finalizing the round, so intermediate display doesn't have trailing
	// blank lines.
	if m.streamAssistantLine >= 0 && m.streamAssistantLine < len(m.chatLines) {
		m.chatLines[m.streamAssistantLine] = strings.TrimRight(m.chatLines[m.streamAssistantLine], "\n")
	}
	m.closeThinkingBlock()
	m.streamAssistantBuf.Reset()
	m.streamAssistantLine = -1
	// Keep streamToolCallNames / toolCallDiffs until OnRoundStart or turn end so
	// OnToolCall finals and patch diffs still resolve after OnStreamEnd.

	m.setViewportContent()
	m.viewport.GotoBottom()
}

func (m *Model) handleStreamEnd() {
	// Trim trailing newlines from the assistant content line before
	// finalizing, so the display doesn't end with a blank line.
	if m.streamAssistantLine >= 0 && m.streamAssistantLine < len(m.chatLines) {
		m.chatLines[m.streamAssistantLine] = strings.TrimRight(m.chatLines[m.streamAssistantLine], "\n")
	}
	m.closeThinkingBlock()
	m.dismissApproval(false)
	m.streaming = false
	m.clearProgress()
	// ContextStats is read-only and local (no provider I/O) — safe on the
	// Update thread once StreamProcessInput has returned.
	m.refreshContextStats()
	if m.agent != nil {
		if err := m.agent.ConsumePersistError(); err != nil {
			m.statusMsg = fmt.Sprintf("Warning: failed to save session: %v", err)
		}
	}
	m.setViewportContent()
	m.viewport.GotoBottom()
}

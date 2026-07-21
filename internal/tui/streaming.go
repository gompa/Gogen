package tui

import (
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"

	tea "github.com/charmbracelet/bubbletea"
)

// --- Bubble Tea messages for streaming ---

// streamStartMsg is sent when the agent first begins processing.
type streamStartMsg struct{}

// streamRoundStartMsg is sent at the start of each LLM round after the first.
type streamRoundStartMsg struct{}

type streamTokenMsg struct{ token string }
type streamThinkingMsg struct{ token string }
type streamToolCallMsg struct {
	index int
	id    string
	name  string
}
type streamToolCallArgsMsg struct {
	index int
	id    string
	delta string
}
type streamToolResultMsg struct {
	id      string
	name    string
	result  string
	success bool
}

// streamToolCallFinalMsg is sent when tool call args are fully parsed.
type streamToolCallFinalMsg struct {
	index int
	tc    llm.ToolCall
}

// streamToolExecuteMsg is sent immediately before a tool runs.
type streamToolExecuteMsg struct {
	name string
}

// streamRoundEndMsg is sent at the end of each streaming round
// (including intermediate tool-call rounds). It resets buffers
// but does NOT set streaming=false.
type streamRoundEndMsg struct{}

// streamEndMsg is sent when all streaming is complete (final message from goroutine).
type streamEndMsg struct{}

type streamErrorMsg struct{ err error }

type contextStatsMsg struct {
	stats agent.TurnContext
}

// StreamAdapter adapts llm.StreamHandlers to emit Bubble Tea messages
// that can be processed by the Model's Update method.
type StreamAdapter struct {
	program *tea.Program
}

// tokenSeg is one coalesced run of either content or thinking tokens.
// Adjacent tokens of the same kind are merged; kind switches preserve order.
type tokenSeg struct {
	think bool
	text  string
}

// tokenBatcher coalesces stream/thinking tokens so the Bubble Tea channel
// is not flooded with one message per token. Flushes at 32ms intervals.
// All fields are guarded by mu because AfterFunc runs flush off the stream goroutine.
//
// Segments are flushed in arrival order. Flushing all content before all
// thinking (or the reverse) would reverse interleaved batches and make
// <thinking> tags appear mid-sentence or as tiny one-word blocks.
type tokenBatcher struct {
	mu    sync.Mutex
	send  func(tea.Msg)
	segs  []tokenSeg
	timer *time.Timer // created lazily; always stopped before reuse
	// closed is set by Close() at stream round end to prevent the
	// timer goroutine from delivering stale thinking/content tokens
	// after the thinking block has already been finalized.
	closed bool
}

func (b *tokenBatcher) scheduleFlushLocked() {
	// Lazily create a one-shot timer. Only start a timer if one isn't
	// already pending. We intentionally do NOT extend the deadline on
	// subsequent tokens (the old Stop+Reset approach delayed flushes
	// indefinitely for fast streams). The first token in each batch
	// sets a fixed 32ms window, guaranteeing at most 32ms of batching
	// and keeping the UI responsive even when tokens arrive faster
	// than the flush interval.
	if b.timer == nil {
		b.timer = time.AfterFunc(32*time.Millisecond, b.flush)
	}
}

func (b *tokenBatcher) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
}

func (b *tokenBatcher) flushLocked() {
	for _, seg := range b.segs {
		if seg.text == "" {
			continue
		}
		if seg.think {
			b.send(streamThinkingMsg{token: seg.text})
		} else {
			b.send(streamTokenMsg{token: seg.text})
		}
	}
	b.segs = b.segs[:0]
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

// Close drains all pending segments, stops the timer, and sets the closed
// flag so that any late-arriving timer goroutine flush is a no-op. Called
// at stream round end (OnStreamEnd / OnToolCallStart) to prevent stale
// thinking tokens from creating duplicate <thinking> blocks in the TUI.
func (b *tokenBatcher) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.segs = b.segs[:0]
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

func (b *tokenBatcher) appendLocked(think bool, token string) {
	if token == "" {
		return
	}
	if b.closed {
		return
	}
	n := len(b.segs)
	if n > 0 && b.segs[n-1].think == think {
		b.segs[n-1].text += token
	} else {
		b.segs = append(b.segs, tokenSeg{think: think, text: token})
	}
	b.scheduleFlushLocked()
}

func (b *tokenBatcher) streamToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendLocked(false, token)
}

func (b *tokenBatcher) thinkToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendLocked(true, token)
}

// NewStreamAdapter creates a new StreamAdapter.
func NewStreamAdapter(p *tea.Program) *StreamAdapter {
	return &StreamAdapter{program: p}
}

// Handlers returns a full set of stream handlers that emit tea.Msg values.
func (s *StreamAdapter) Handlers() *llm.StreamHandlers {
	batch := &tokenBatcher{send: s.program.Send}

	return &llm.StreamHandlers{
		OnStart: func() {
			batch.mu.Lock()
			batch.closed = false
			batch.mu.Unlock()
			s.program.Send(streamStartMsg{})
		},
		OnRoundStart: func() {
			batch.mu.Lock()
			batch.closed = false
			batch.mu.Unlock()
			s.program.Send(streamRoundStartMsg{})
		},
		OnThinkingToken: func(token string) {
			batch.thinkToken(token)
		},
		OnToken: func(token string) {
			batch.streamToken(token)
		},
		OnStreamEnd: func() {
			batch.flush()
			batch.Close()
			s.program.Send(streamRoundEndMsg{})
		},
		OnToolCallStart: func(index int, id, name string) {
			// Flush any pending thinking/content tokens before tool calls
			// reach the TUI.  Without this, batched thinking tokens can
			// arrive after the tool call message and get silently dropped
			// by handleStreamThinking's post-tool-call guard.
			batch.flush()
			s.program.Send(streamToolCallMsg{index: index, id: id, name: name})
		},
		OnToolCallArgsDelta: func(index int, id, name, argsDelta string) {
			s.program.Send(streamToolCallArgsMsg{index: index, id: id, delta: argsDelta})
		},
		OnToolCall: func(tc llm.ToolCall) {
			s.program.Send(streamToolCallFinalMsg{index: tc.Index, tc: tc})
		},
		OnToolExecute: func(name string) {
			s.program.Send(streamToolExecuteMsg{name: name})
		},
		OnToolResult: func(id, name, result string, success bool) {
			s.program.Send(streamToolResultMsg{id: id, name: name, result: result, success: success})
		},
		OnRecoverPartialStream: func() {},
	}
}

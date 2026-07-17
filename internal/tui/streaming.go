package tui

import (
	"strings"
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

type sessionRestoredMsg struct {
	sessionID string
	messages  []llm.Message
}

type clearChatMsg struct{}

// StreamAdapter adapts llm.StreamHandlers to emit Bubble Tea messages
// that can be processed by the Model's Update method.
type StreamAdapter struct {
	program *tea.Program
}

// tokenBatcher coalesces stream/thinking tokens so the Bubble Tea channel
// is not flooded with one message per token. Flushes at 32ms intervals.
// All fields are guarded by mu because AfterFunc runs flush off the stream goroutine.
type tokenBatcher struct {
	mu     sync.Mutex
	send   func(tea.Msg)
	stream strings.Builder
	think  strings.Builder
	timer  *time.Timer
}

func (b *tokenBatcher) scheduleFlushLocked() {
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
	if s := b.stream.String(); s != "" {
		b.send(streamTokenMsg{token: s})
		b.stream.Reset()
	}
	if s := b.think.String(); s != "" {
		b.send(streamThinkingMsg{token: s})
		b.think.Reset()
	}
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

func (b *tokenBatcher) streamToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stream.WriteString(token)
	b.scheduleFlushLocked()
}

func (b *tokenBatcher) thinkToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.think.WriteString(token)
	b.scheduleFlushLocked()
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
			s.program.Send(streamStartMsg{})
		},
		OnRoundStart: func() {
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
			s.program.Send(streamRoundEndMsg{})
		},
		OnToolCallStart: func(index int, id, name string) {
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

package tui

import (
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/streamutil"

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

// NewStreamAdapter creates a new StreamAdapter.
func NewStreamAdapter(p *tea.Program) *StreamAdapter {
	return &StreamAdapter{program: p}
}

// Handlers returns a full set of stream handlers that emit tea.Msg values.
func (s *StreamAdapter) Handlers() *llm.StreamHandlers {
	tuiSend := func(think bool, text string) {
		if think {
			s.program.Send(streamThinkingMsg{token: text})
		} else {
			s.program.Send(streamTokenMsg{token: text})
		}
	}
	batch := streamutil.NewTokenBatcher(tuiSend, 32*time.Millisecond)

	return &llm.StreamHandlers{
		OnStart: func() {
			batch.Reset()
			s.program.Send(streamStartMsg{})
		},
		OnRoundStart: func() {
			batch.Reset()
			s.program.Send(streamRoundStartMsg{})
		},
		OnThinkingToken: func(token string) {
			batch.ThinkToken(token)
		},
		OnToken: func(token string) {
			batch.StreamToken(token)
		},
		OnStreamEnd: func() {
			batch.Flush()
			batch.Close()
			s.program.Send(streamRoundEndMsg{})
		},
		OnToolCallStart: func(index int, id, name string) {
			batch.Flush()
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

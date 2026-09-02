// Package streambuf provides a thread-safe buffer for one in-flight LLM
// round's streamed output, shared by every host that needs to render a
// mid-round join (the TUI's live-session transcript and the web server's
// attach rewind).
//
// The assistant message only reaches the agent's committed history when a
// round completes, so without this buffer a join mid-round would show the
// committed history with the in-flight reply missing until the turn-end
// rebuild. The buffer is therefore appended from round start REGARDLESS of
// whether the session is focused/viewed, and cleared at turn start, round
// start, and round end: the empty state IS the "between rounds" marker —
// completed content lives in the committed history already, so a join
// between rounds must not render it a second time (Snapshot returns nil).
package streambuf

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"unicode/utf16"
)

// UTF16Len returns the number of UTF-16 code units in s — the unit the
// browser's JS string length and slice operate in. Positions stamped on
// the wire use this (not byte counts) so a client's rewind trim can slice
// exactly, including multi-byte (emoji/CJK) content.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if l := utf16.RuneLen(r); l > 0 {
			n += l
		} else {
			n++
		}
	}
	return n
}

// RoundBuffer accumulates the current round's in-flight LLM output so a
// mid-turn join can render the current reply from its first character.
//
// It is self-synchronizing: every method takes its own mutex, and the
// buffer is a leaf lock — callers must never hold it while acquiring
// other locks. The turn/stream goroutine appends; join/attach goroutines
// snapshot.
//
// The *Units counters track each stream's length in UTF-16 code units and
// are updated under the SAME lock as the builders: a rewind snapshot
// reports the full text plus its unit length, and the client trims its
// live stream to exactly that position — a snapshot that ever saw a
// character whose unit count had not landed yet would make the client
// drop it permanently.
type RoundBuffer struct {
	mu sync.Mutex

	thinking      strings.Builder
	thinkingUnits int
	content       strings.Builder
	contentUnits  int
	toolNames     map[int]string
	toolIDs       map[int]string
	toolArgs      map[int]*strings.Builder
	toolArgsUnits map[int]int
}

// ToolCall is one in-progress tool call in a Snapshot.
//
// The JSON tags are the WIRE contract: the web server emits Snapshot
// directly in the history payload's rewind field (WSMessage.Rewind), and
// the client's rewind merge (appendStreamingToolArgs + trimToEnd) slices
// against these exact field names. server/rewind_wire_test.go pins them.
type ToolCall struct {
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	// Args is the raw args accumulated so far (may be incomplete JSON).
	Args string `json:"args,omitempty"`
	// ArgsPos is the UTF-16 code-unit length of Args (wire rewind merge).
	ArgsPos int `json:"argsPos,omitempty"`
}

// Snapshot is a join-time copy of a RoundBuffer.
//
// It is the single rewind payload type for every host: the web server
// attaches it to a mid-turn attach/resume history payload (the client
// renders it through the normal stream machinery and trims the live
// stream to the *Pos boundaries), and the TUI renders it directly on a
// mid-turn join. The JSON tags are the wire contract (see ToolCall);
// a nil Snapshot means "between rounds" and is omitted on the wire.
type Snapshot struct {
	Thinking    string     `json:"thinking,omitempty"`
	ThinkingPos int        `json:"thinkingPos,omitempty"` // UTF-16 code-unit length of Thinking
	Content     string     `json:"content,omitempty"`
	ContentPos  int        `json:"contentPos,omitempty"` // UTF-16 code-unit length of Content
	ToolCalls   []ToolCall `json:"toolCalls,omitempty"`
}

// Reset clears the buffer (turn start, round start, round end).
func (b *RoundBuffer) Reset() {
	b.mu.Lock()
	b.resetLocked()
	b.mu.Unlock()
}

func (b *RoundBuffer) resetLocked() {
	b.thinking.Reset()
	b.thinkingUnits = 0
	b.content.Reset()
	b.contentUnits = 0
	b.toolNames = nil
	b.toolIDs = nil
	b.toolArgs = nil
	b.toolArgsUnits = nil
}

// AppendContent records a streamed content token (OnToken).
func (b *RoundBuffer) AppendContent(text string) {
	if text == "" {
		return
	}
	b.mu.Lock()
	b.content.WriteString(text)
	b.contentUnits += UTF16Len(text)
	b.mu.Unlock()
}

// AppendThinking records a streamed thinking token (OnThinkingToken).
func (b *RoundBuffer) AppendThinking(text string) {
	if text == "" {
		return
	}
	b.mu.Lock()
	b.thinking.WriteString(text)
	b.thinkingUnits += UTF16Len(text)
	b.mu.Unlock()
}

// ToolStart records the start of a streamed tool call (OnToolCallStart).
func (b *RoundBuffer) ToolStart(index int, id, name string) {
	b.mu.Lock()
	if b.toolNames == nil {
		b.toolNames = make(map[int]string)
		b.toolIDs = make(map[int]string)
		b.toolArgs = make(map[int]*strings.Builder)
	}
	b.toolNames[index] = name
	b.toolIDs[index] = id
	b.toolArgs[index] = &strings.Builder{}
	if b.toolArgsUnits == nil {
		b.toolArgsUnits = make(map[int]int)
	}
	b.toolArgsUnits[index] = 0
	b.mu.Unlock()
}

// AppendToolArgs records one args delta for a streaming tool call
// (OnToolCallArgsDelta). It must run synchronously on every delta (not at
// flush time) so Snapshot always reports the complete args the client
// will eventually receive.
func (b *RoundBuffer) AppendToolArgs(index int, delta string) {
	if delta == "" {
		return
	}
	b.mu.Lock()
	if b.toolArgs == nil {
		b.toolArgs = make(map[int]*strings.Builder)
		b.toolArgsUnits = make(map[int]int)
	} else if b.toolArgsUnits == nil {
		b.toolArgsUnits = make(map[int]int)
	}
	buf := b.toolArgs[index]
	if buf == nil {
		buf = &strings.Builder{}
		b.toolArgs[index] = buf
		b.toolArgsUnits[index] = 0
	}
	buf.WriteString(delta)
	b.toolArgsUnits[index] += UTF16Len(delta)
	b.mu.Unlock()
}

// Snapshot returns the in-flight round's output, or nil when the round is
// empty — between rounds or before the first token (completed content
// lives in the committed history already). Tool calls are sorted by index.
func (b *RoundBuffer) Snapshot() *Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.content.Len() == 0 && b.thinking.Len() == 0 && len(b.toolNames) == 0 {
		return nil
	}
	snap := &Snapshot{
		Thinking:    b.thinking.String(),
		ThinkingPos: b.thinkingUnits,
		Content:     b.content.String(),
		ContentPos:  b.contentUnits,
	}
	for idx, name := range b.toolNames {
		args := ""
		argsPos := 0
		if buf := b.toolArgs[idx]; buf != nil {
			args = buf.String()
			argsPos = b.toolArgsUnits[idx]
		}
		snap.ToolCalls = append(snap.ToolCalls, ToolCall{
			Index: idx, ID: b.toolIDs[idx], Name: name, Args: args, ArgsPos: argsPos,
		})
	}
	slices.SortFunc(snap.ToolCalls, func(a, c ToolCall) int {
		return cmp.Compare(a.Index, c.Index)
	})
	return snap
}

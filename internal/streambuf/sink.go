package streambuf

// RoundSink maps stream-callback events onto the RoundBuffer operations
// with the reset/append timing every host must use:
//
//   - Cleared at turn start, round start, and round end — the empty state
//     IS the "between rounds" marker (Snapshot returns nil), so a join
//     between rounds must not render completed content a second time.
//   - Appended on EVERY token/tool event regardless of focus/view, so a
//     mid-turn join can render the current reply from its first character.
//
// The hosts (the TUI's StreamAdapter, the web server's wsStreamSink) call
// the matching method from their streamutil.Sink implementation, so this
// timing rule lives in one place instead of drifting between the two.
//
// A zero RoundSink is a no-op: adapters built without a session (unit
// tests) may call it unconditionally.
type RoundSink struct {
	// Buf is the round buffer fed by the sink; nil makes every method a
	// no-op.
	Buf *RoundBuffer
	// Reset, when set, replaces Buf.Reset as the reset implementation: the
	// web server's reset also clears its wire-side sent markers, which the
	// buffer does not know about.
	Reset func()
	// OnToolStart, when set, runs after Buf.ToolStart for the same event:
	// the web server uses it to restart the wire sent marker for a
	// recycled tool-call index.
	OnToolStart func(index int, id, name string)
}

// TurnBegin clears the buffer for a new turn (OnStart).
func (r *RoundSink) TurnBegin() { r.reset() }

// RoundBegin clears the buffer for a new round (OnRoundStart): the
// previous round's completed content is in the committed history.
func (r *RoundSink) RoundBegin() { r.reset() }

// RoundEnd clears the buffer (OnStreamEnd): the round's assistant message
// is appended to the committed history immediately after, so the buffer
// only ever carries content a history snapshot would otherwise miss.
func (r *RoundSink) RoundEnd() { r.reset() }

// Token records a streamed content token (OnToken).
func (r *RoundSink) Token(text string) {
	if r.Buf != nil {
		r.Buf.AppendContent(text)
	}
}

// Thinking records a streamed thinking token (OnThinkingToken).
func (r *RoundSink) Thinking(text string) {
	if r.Buf != nil {
		r.Buf.AppendThinking(text)
	}
}

// ToolStart records the start of a streamed tool call (OnToolCallStart).
func (r *RoundSink) ToolStart(index int, id, name string) {
	if r.Buf != nil {
		r.Buf.ToolStart(index, id, name)
	}
	if r.OnToolStart != nil {
		r.OnToolStart(index, id, name)
	}
}

// ToolArgs records one args delta for a streaming tool call
// (OnToolCallArgsDelta). It must run synchronously on every delta (not at
// flush time) so the snapshot always reports the complete args the client
// will eventually receive.
func (r *RoundSink) ToolArgs(index int, delta string) {
	if r.Buf != nil {
		r.Buf.AppendToolArgs(index, delta)
	}
}

func (r *RoundSink) reset() {
	if r.Reset != nil {
		r.Reset()
		return
	}
	if r.Buf != nil {
		r.Buf.Reset()
	}
}

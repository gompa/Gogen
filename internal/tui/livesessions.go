package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/bubbletea/v2"

	"gogen/internal/agent"
)

// maxPendingStreamEvents bounds one background session's replay buffer
// (oldest dropped first, freshness wins — same policy as the delivery
// queue).
const maxPendingStreamEvents = 4096

// Sidebar geometry: clamp range for the live-sessions panel and the
// narrowest main column we tolerate before auto-hiding it.
//
// The live clamp range is TERMINAL-RELATIVE (web parity: the web sidebar
// scales from 12% of the viewport, floored at 120px, up to 50% of the
// viewport) — see sidebarMinWidth/sidebarMaxWidth. minSidebarWidth is the
// absolute floor (the web's 120px) and maxSidebarWidth the absolute
// ceiling (also the sanity bound for persisted widths).
const (
	defaultSidebarWidth = 38 // ≈ the web's 300px default at ~8px per cell
	minSidebarWidth     = 16
	maxSidebarWidth     = 80
	minMainWidth        = 40
)

// liveSession is one actively hosted conversation inside the TUI process.
// The focused session owns the transcript buffers and progress UI; other
// live sessions continue running turns in the background — their stream
// events are buffered in pending (see enqueue/popAll) and replayed when
// focus arrives, while their terminal state always surfaces attributed.
//
// Ownership contract: streaming/cancel state is mutated only from the
// Update thread (turn-start writes happen synchronously inside submit
// paths; finalization happens in handleTurnFinishedMsg), mirroring the
// existing single-session Messages-ownership rule.
type liveSession struct {
	id    string
	label string
	agent *agent.Agent

	cancel    context.CancelFunc // cancels the in-flight turn; nil when idle
	streaming bool

	// lastActive is the last OUTPUT time (completed turn, session spawn) —
	// the sidebar's ordering key for live sessions (web: pane.lastActivity,
	// written on output only — never on focus, submit, or persistence).
	// Mutated only on the Update thread.
	lastActive time.Time

	// focused reports whether this session currently owns the transcript
	// UI. Read from stream goroutines (the adapter gate), hence atomic.
	focused atomic.Bool

	// pending buffers rendering events emitted while this session streamed
	// in the background; replayed on focus (mu guards cross-thread).
	mu      sync.Mutex
	pending []tea.Msg
}

// enqueue buffers one rendering event produced while this session streamed
// in the background.
func (s *liveSession) enqueue(msg tea.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) >= maxPendingStreamEvents {
		s.pending = s.pending[1:]
	}
	s.pending = append(s.pending, msg)
}

// popAll detaches buffered events for replay on the Update thread once
// this session gains focus.
func (s *liveSession) popAll() []tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = nil
	return out
}

// liveSessions hosts every active session. Index 0 is the startup agent;
// additional entries are spawned through the shared web lifecycle core
// (server.NewWorkspaceForHost → NewSessionAgent).
//
// This is the TUI counterpart of the web server's session registry: the
// sidebar's ACTIVE section lists exactly these entries, while INACTIVE
// sessions are the persisted snapshots in the shared session store.
type liveSessions struct {
	sessions []*liveSession
	active   int
}

func newLiveSessions(root *agent.Agent) *liveSessions {
	root0 := &liveSession{id: "s1", label: "main", agent: root, lastActive: time.Now()}
	root0.focused.Store(true)
	return &liveSessions{sessions: []*liveSession{root0}}
}

// Active returns the focused session (never nil after construction).
func (ls *liveSessions) Active() *liveSession {
	return ls.sessions[ls.active]
}

// ByID resolves a session by id; nil when unknown (e.g. closed mid-turn).
func (ls *liveSessions) ByID(id string) *liveSession {
	for _, s := range ls.sessions {
		if s.id == id {
			return s
		}
	}
	return nil
}

// Add registers a new live session WITHOUT switching focus. The caller
// owns building the agent (shared workspace factory) and flushing it on
// close.
func (ls *liveSessions) Add(a *agent.Agent, label string) *liveSession {
	s := &liveSession{id: nextLiveID(ls), label: label, agent: a, lastActive: time.Now()}
	ls.sessions = append(ls.sessions, s)
	return s
}

// Switch focuses session i, flipping the adapter gates accordingly.
// Switching away from a streaming session leaves its turn running.
func (ls *liveSessions) Switch(i int) {
	if i < 0 || i >= len(ls.sessions) || i == ls.active {
		return
	}
	old := ls.sessions[ls.active]
	old.focused.Store(false)
	ls.active = i
	ls.sessions[i].focused.Store(true)
}

// nextLiveID derives the next stable routing id ("s2", "s3", …).
func nextLiveID(ls *liveSessions) string {
	n := len(ls.sessions) + 1
	for {
		id := "s" + itoa(n)
		if ls.ByID(id) == nil {
			return id
		}
		n++
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// switchToLive focuses live session i and rebinds every piece of focused
// Model state to its agent — including the streaming mirror: m.streaming
// describes the FOCUSED session, so leaving a mid-turn session unlocks
// input on the newly focused one, and joining one locks it again. The
// transcript is rebuilt from Messages using the same converge-on-switch
// contract as /resume; the previous session keeps its turn running in the
// background either way.
func (m *Model) switchToLive(i int) tea.Cmd {
	if m.lives == nil {
		return nil
	}
	target := m.lives.ByIndex(i)
	if target == nil || target == m.lives.Active() {
		return nil
	}
	// Drain the replay buffer on BOTH sides of the focus flip (see
	// joinStreamingSession): before it, while the adapter gate is still
	// closed, and after it, to catch events emitted in between. A single
	// pre-flip drain would strand those straddlers in the buffer forever;
	// a single post-flip drain would replay them after newer live events.
	var pending []tea.Msg
	if target.streaming {
		pending = target.popAll()
	}
	m.lives.Switch(i)
	if target.streaming {
		pending = append(pending, target.popAll()...)
	}
	m.agent = target.agent
	m.sessionID = target.agent.SessionID
	var cmds []tea.Cmd
	if target.streaming {
		// Joined mid-turn: Messages are owned by that session's stream
		// goroutine, so no snapshot — show a join notice and replay the
		// buffered events live; the authoritative history rebuild happens
		// at end-of-turn convergence.
		m.joinStreamingSession(target, pending)
	} else {
		// Rebuild transcript (mirrors cmdSession's clear-chat resume path).
		m.chatLines = renderMessages(
			target.agent.Messages,
			target.agent.WorkingDir,
			target.agent.CurrentModel(),
			target.agent.Mode.String(),
		)
		m.resetStreamState(false)
	}
	// Sync the focused-streaming mirror + progress UI to the target.
	m.streaming = target.streaming
	if target.streaming {
		cmds = append(cmds, m.setProgress(progressThinking, "thinking"))
	} else {
		m.clearProgress()
	}
	m.setViewportContent()
	m.viewport.GotoBottom()
	m.refreshContextStats()
	// Keep the panel's cursor on the focused row (web: the focused row is
	// the highlighted one).
	m.sidebarCursor = m.sidebarFocusedRow()
	return tea.Batch(cmds...)
}

// joinStreamingSession renders the join notice and replays the events the
// session buffered while it streamed in the background; subsequent events
// flow live because focus has already flipped.
//
// pending is drained by the caller AROUND the focus flip (see switchToLive):
// once before it (adapter gate still closed) and once after (catching events
// emitted between the two drains). Every replayed event is therefore older
// than anything that reaches the program after the flip — total order is
// preserved at the join boundary.
func (m *Model) joinStreamingSession(target *liveSession, pending []tea.Msg) {
	m.chatLines = []string{
		SystemStyle.Render("▍ joined \"" + target.label + "\" mid-turn — full history appears when the reply completes"),
	}
	m.resetStreamState(false)
	for _, msg := range pending {
		m.replayStreamEvent(msg)
	}
}

// replayStreamEvent applies one buffered rendering event through the normal
// stream handlers. Only adapter output reaches here, so the cases mirror
// Update's streaming subset (terminal msgs never enter the buffer — they
// carry sid attribution instead).
func (m *Model) replayStreamEvent(msg tea.Msg) {
	switch v := msg.(type) {
	case streamStartMsg:
		m.handleStreamStart()
	case streamRoundStartMsg:
		m.handleStreamRoundStart()
	case streamTokenMsg:
		m.handleStreamToken(v.token)
	case streamThinkingMsg:
		m.handleStreamThinking(v.token)
	case streamToolCallMsg:
		m.handleStreamToolCall(v.index, v.id, v.name)
	case streamToolCallArgsMsg:
		m.handleStreamToolArgs(v.index, v.id, v.delta)
	case streamToolCallFinalMsg:
		m.handleStreamToolCallFinal(v.index, v.tc)
	case streamToolExecuteMsg:
		// Deliberately not replayed: a "running X" line surfaced after the
		// fact would mislead (the tool already ran); its result line follows.
	case streamToolResultMsg:
		m.handleStreamToolResult(v.id, v.name, v.result, v.success)
	case streamRoundEndMsg:
		m.handleStreamRoundEnd()
	case condensedNoteMsg:
		m.appendChatLine(SystemStyle.Render(v.note))
	}
}

// Close detaches an idle background session after flushing it; it stays
// resumable under /resume. The focused or still-streaming session cannot
// be closed.
func (ls *liveSessions) Close(i int) error {
	if i < 0 || i >= len(ls.sessions) {
		return fmt.Errorf("no such session")
	}
	s := ls.sessions[i]
	if i == ls.active {
		return fmt.Errorf("session %q is focused", s.label)
	}
	if s.streaming {
		return fmt.Errorf("session %q is streaming — cancel its turn first", s.label)
	}
	if s.agent != nil {
		s.agent.FlushSession()
	}
	ls.sessions = append(ls.sessions[:i], ls.sessions[i+1:]...)
	if ls.active > i {
		ls.active--
	}
	return nil
}

// ByIndex returns the session at index i, or nil when out of range.
func (ls *liveSessions) ByIndex(i int) *liveSession {
	if i < 0 || i >= len(ls.sessions) {
		return nil
	}
	return ls.sessions[i]
}

// uiPrefs persists sidebar panel state per working directory, so the
// panel reopens with the geometry the user left it at (the TUI analogue
// of the web UI's localStorage preferences).
type uiPrefs struct {
	SidebarVisible bool `json:"sidebarVisible"`
	SidebarWidth   int  `json:"sidebarWidth"`
}

func uiPrefsPath(workingDir string) string {
	return filepath.Join(workingDir, ".gogen", "tui.json")
}

// loadUIPrefs best-effort reads saved panel state. found=false means no
// usable file existed (missing or corrupt) — distinct from an explicit
// saved choice, because the two drive different defaults.
func loadUIPrefs(workingDir string) (uiPrefs, bool) {
	p := uiPrefs{SidebarWidth: defaultSidebarWidth}
	data, err := os.ReadFile(uiPrefsPath(workingDir))
	if err != nil {
		return p, false
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return uiPrefs{SidebarWidth: defaultSidebarWidth}, false
	}
	if p.SidebarWidth < minSidebarWidth || p.SidebarWidth > maxSidebarWidth {
		p.SidebarWidth = defaultSidebarWidth
	}
	return p, true
}

// resolveSidebarStart resolves the panel's startup state: ALWAYS visible
// (web parity — the web sidebar opens by default), with the width the user
// last chose. A hidden preference from older builds is ignored: the panel
// is the multi-session surface and must stay discoverable.
func resolveSidebarStart(workingDir string) uiPrefs {
	p, _ := loadUIPrefs(workingDir)
	return uiPrefs{SidebarVisible: true, SidebarWidth: p.SidebarWidth}
}

// saveUIPrefs writes panel state. Errors are silently ignored — losing a
// width preference must never surface as a user-facing failure.
func saveUIPrefs(workingDir string, p uiPrefs) {
	path := uiPrefsPath(workingDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// persistSidebarPrefs saves current panel state for the active session's
// working directory (no-op without an agent).
func (m *Model) persistSidebarPrefs() {
	if m.agent == nil || m.agent.WorkingDir == "" {
		return
	}
	saveUIPrefs(m.agent.WorkingDir, uiPrefs{
		SidebarVisible: m.sidebarVisible,
		SidebarWidth:   m.sidebarWidth,
	})
}

// streamEventSid reports the owning-session id embedded in a streaming
// rendering message (ok=false for every other message kind).
func streamEventSid(msg tea.Msg) (string, bool) {
	switch v := msg.(type) {
	case streamStartMsg:
		return v.sid, true
	case streamRoundStartMsg:
		return v.sid, true
	case streamTokenMsg:
		return v.sid, true
	case streamThinkingMsg:
		return v.sid, true
	case streamToolCallMsg:
		return v.sid, true
	case streamToolCallArgsMsg:
		return v.sid, true
	case streamToolCallFinalMsg:
		return v.sid, true
	case streamToolResultMsg:
		return v.sid, true
	case streamToolExecuteMsg:
		return v.sid, true
	case streamRoundEndMsg:
		return v.sid, true
	case condensedNoteMsg:
		return v.sid, true
	}
	return "", false
}

// endOfStream / failOfStream are the ONLY constructors for a turn's
// terminal messages: they enforce that every terminal carries its owning
// session id, so handleTurnFinishedMsg can always route finalization.
// (A bare streamEndMsg{} would finalize nothing — ByID("") misses — and
// brick the focused session's input forever.)
func endOfStream(sid string) tea.Msg {
	return streamEndMsg{sid: sid}
}

func failOfStream(sid string, err error) tea.Msg {
	return streamErrorMsg{err: err, sid: sid}
}

// ownsStream reports whether sid belongs to the focused session.
func (m *Model) ownsStream(sid string) bool {
	return m.lives == nil || m.lives.Active().id == sid
}

// turnSession returns the session that owns a newly-started turn,
// lazily materializing the registry for Models built without one
// (unit tests, embedded hosts).
func (m *Model) turnSession() *liveSession {
	if m.lives == nil {
		if m.agent != nil {
			m.lives = newLiveSessions(m.agent)
		} else {
			s := &liveSession{id: "transient"}
			s.focused.Store(true)
			return s
		}
	}
	return m.lives.Active()
}

// sidebarOffsetX is the horizontal shift of the chat column caused by the
// visible sessions panel (0 when hidden or auto-hidden). Mouse hit-testing
// must apply it so drag-selection maps to content coordinates.
func (m *Model) sidebarOffsetX() int {
	if m.sidebarVisible && m.width-m.sidebarWidth >= minMainWidth {
		return m.sidebarWidth
	}
	return 0
}

// mainWidth is the horizontal space the chat column may use: the full
// terminal width, minus the sidebar when it is visible and fits.
func (m *Model) mainWidth() int {
	return m.width - m.sidebarOffsetX()
}

// sidebarMinWidth is the live lower clamp: 12% of the terminal (web
// parity), floored at minSidebarWidth (the web's 120px floor).
func (m *Model) sidebarMinWidth() int {
	if w := m.width * 12 / 100; w > minSidebarWidth {
		return w
	}
	return minSidebarWidth
}

// sidebarMaxWidth is the live upper clamp: 50% of the terminal (web
// parity), never squeezing the main column below minMainWidth, and capped
// at maxSidebarWidth.
func (m *Model) sidebarMaxWidth() int {
	w := m.width / 2
	if cap := m.width - minMainWidth; w > cap {
		w = cap
	}
	if w > maxSidebarWidth {
		w = maxSidebarWidth
	}
	if w < minSidebarWidth {
		w = minSidebarWidth
	}
	return w
}

// toggleSidebar shows/hides the panel and re-lays-out the chat column.
func (m *Model) toggleSidebar() {
	m.sidebarVisible = !m.sidebarVisible
	if m.sidebarVisible && m.sidebarWidth == 0 {
		m.sidebarWidth = defaultSidebarWidth
	}
	if m.width > 0 {
		m.SetSize(m.width, m.height)
	}
	if m.sidebarVisible {
		m.refreshSavedSessions()
	} else if m.focus == FocusSidebar {
		// Never strand keyboard focus on a hidden panel.
		m.focus = FocusInput
	}
	m.persistSidebarPrefs()
}

// resizeSidebar steps the panel width by delta (clamped to the live
// terminal-relative range) and re-lays-out. The upper clamp never exceeds
// width - minMainWidth, so keyboard resize can never auto-hide the panel.
func (m *Model) resizeSidebar(delta int) {
	if !m.sidebarVisible {
		return
	}
	w := m.sidebarWidth + delta
	if w < m.sidebarMinWidth() {
		w = m.sidebarMinWidth()
	}
	if w > m.sidebarMaxWidth() {
		w = m.sidebarMaxWidth()
	}
	if w == m.sidebarWidth {
		return
	}
	m.sidebarWidth = w
	if m.width > 0 {
		m.SetSize(m.width, m.height)
	}
	m.persistSidebarPrefs()
}

// onSidebarBorder reports whether a mouse event landed on the panel's
// right border (±1 column) — the drag handle.
func (m *Model) onSidebarBorder(ev mouseEvent) bool {
	if !m.sidebarVisible || m.sidebarOffsetX() == 0 {
		return false
	}
	bx := m.sidebarWidth - 1
	return ev.x >= bx-1 && ev.x <= bx+1
}

// handleSidebarResizeMouse drives border-drag resizing. Returns true when
// the event was consumed (it must NOT start a text selection).
//
// Precedence: a left-press within the border tolerance begins the drag;
// motion with the button held maps the panel width to the cursor; release
// finalizes and persists. While dragging, every event is swallowed so the
// transcript neither selects nor scrolls.
func (m *Model) handleSidebarResizeMouse(ev mouseEvent) bool {
	switch {
	case ev.kind == mousePress && ev.button == tea.MouseLeft && m.onSidebarBorder(ev):
		m.sidebarDragging = true
		return true
	case m.sidebarDragging && ev.kind == mouseMotion:
		w := ev.x + 1
		if w < m.sidebarMinWidth() {
			w = m.sidebarMinWidth()
		}
		if w > m.sidebarMaxWidth() {
			w = m.sidebarMaxWidth()
		}
		if w != m.sidebarWidth {
			m.sidebarWidth = w
			if m.width > 0 {
				m.SetSize(m.width, m.height)
			}
		}
		return true
	case m.sidebarDragging && ev.kind == mouseRelease:
		m.sidebarDragging = false
		m.persistSidebarPrefs()
		return true
	case m.sidebarDragging:
		return true // button-less motion etc.: swallow mid-drag
	}
	// Wheel over the panel is handled by handleSidebarMouse (it scrolls
	// the session list); this handler only owns the border drag.
	return false
}

// cancelActiveStream cancels the focused session's in-flight turn, if any.
//
// Deliberately does NOT clear streaming/cancel here: those flags stay set
// until the attributed terminal message (context-cancel error) arrives,
// because Messages remain owned by the unwinding stream goroutine until
// then — treating the session as idle early would let an idle-path
// transcript rebuild race that teardown. The dot may show ● for a few ms
// after esc; that is the contract working, not a bug.
func (m *Model) cancelActiveStream() {
	if m.lives == nil {
		return
	}
	if s := m.lives.Active(); s.cancel != nil {
		s.cancel()
	}
}

// handleTurnFinishedMsg finalizes the owning session's turn. The FOCUSED
// session runs the normal end-of-turn pipeline (transcript finalize,
// context refresh, persist-error surfacing); a BACKGROUND session only
// drops its runtime flags and reports completion via the status line —
// its transcript is rebuilt from Messages whenever it is next focused
// (the same converge-on-switch contract as the web UI's mid-turn attach).
func (m *Model) handleTurnFinishedMsg(sid string, err error) (tea.Model, tea.Cmd) {
	if m.lives == nil {
		// Degenerate single-session construction without a registry.
		return m.finishFocusedTurn(err)
	}
	s := m.lives.ByID(sid)
	if s == nil {
		return m, nil
	}
	s.streaming = false
	s.cancel = nil
	// Only a COMPLETED turn produced output: cancelled/failed turns must
	// not bump the row (web parity — pane.lastActivity is written on
	// output events only, never on focus or turn start).
	if err == nil {
		s.lastActive = time.Now()
		if s.agent != nil {
			// Per-id memory (touchSessionActivity): the row keeps this
			// stamp after the session stops being focused.
			m.touchSessionActivity(s.agent.SessionID, s.lastActive)
		}
	}
	// The store index just changed (label derived from the first user
	// message, message count, updatedAt) — refresh the unified list.
	m.refreshSavedSessions()
	if m.lives.Active() != s {
		if err != nil {
			m.statusMsg = "✗ " + s.label + ": " + err.Error()
		} else {
			m.statusMsg = "✓ " + s.label + " finished"
		}
		m.bellIfBlurred() // web parity: background pane finished/failed
		return m, nil
	}
	return m.finishFocusedTurn(err)
}

// finishFocusedTurn runs the normal end-of-turn pipeline (success or
// error variant) for the FOCUSED session. Internal helper: terminal msgs
// from the wire go through handleTurnFinishedMsg; this skips the envelope.
func (m *Model) finishFocusedTurn(err error) (tea.Model, tea.Cmd) {
	if err != nil {
		m.handleStreamError(err)
		return m, tea.Batch(m.refocusInput(), m.drainDeliveries())
	}
	return m.handleStreamEndMsg()
}

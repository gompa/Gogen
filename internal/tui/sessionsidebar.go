package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/session"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Sidebar panel layout constants (row indices inside the panel):
//
//	0  ┌──┐ top border
//	1  "SESSIONS" header
//	2  working directory
//	3  blank
//	4+ session rows, three lines each (title + meta + blank separator;
//	     the last rendered session has no trailing separator)
//	-3 blank
//	-2 footer hints
//	-1 └──┘ bottom border
const (
	sidebarHeaderLines = 3
	sidebarFooterLines = 2
	// sidebarBorderLines is the top + bottom border rows of the panel box.
	sidebarBorderLines = 2
)

// Footer button ids double as the hover state; sidebarFooterNone is the
// zero value so literal-built models (tests) start unhovered.
const (
	sidebarFooterNone   = 0
	sidebarFooterNew    = 1 // "n new" — same action as the n key
	sidebarFooterClose  = 2 // "x close" — same action as the x key (cursor row)
	sidebarFooterDelete = 3 // "d del" — same action as the d key (cursor row)
)

// Footer labels: the clickable button segments plus the plain hint tail.
// The buttons render brighter than the hints (cyan; the hovered one gets
// the row-highlight style). "d del" (not "d delete") keeps all three
// buttons inside the default panel width.
const (
	sidebarFooterNewLabel    = "n new"
	sidebarFooterCloseLabel  = "x close"
	sidebarFooterDeleteLabel = "d del"
	// "size" (not "resize") keeps the full footer (39 cells) inside any
	// panel ≥ 42 wide; the help modal carries the full wording.
	sidebarFooterRest = "[/] size  ^b hide"
)

// sidebarFooterZone is one clickable footer button: its id and the
// inclusive terminal-column range it occupies (the "│ " prefix is 2
// columns, same convention as the row ✕ zone in sidebarRowAt).
type sidebarFooterZone struct {
	button int
	x0, x1 int
}

// sidebarFooterZones returns the hit-test ranges of the footer buttons.
// Layout: "n new"(cols 2-6) + "  " + "x close"(cols 9-15) + "  " +
// "d del"(cols 18-22).
func sidebarFooterZones() []sidebarFooterZone {
	newX := 2
	closeX := newX + len(sidebarFooterNewLabel) + 2
	delX := closeX + len(sidebarFooterCloseLabel) + 2
	return []sidebarFooterZone{
		{button: sidebarFooterNew, x0: newX, x1: newX + len(sidebarFooterNewLabel) - 1},
		{button: sidebarFooterClose, x0: closeX, x1: closeX + len(sidebarFooterCloseLabel) - 1},
		{button: sidebarFooterDelete, x0: delX, x1: delX + len(sidebarFooterDeleteLabel) - 1},
	}
}

// sidebarRow is one entry of the unified session list — the TUI port of the
// web sidebar's row model (components/sessions.js): ONE list of sessions in
// which a live (hosted) session overlays onto its saved entry by id.
// Focus/streaming are ATTRIBUTES of the row, never its position: ordering is
// by last output time (the in-process output stamp when known, else the
// store's updatedAt), so focusing a session never reorders the list.
type sidebarRow struct {
	id         string
	label      string
	live       bool // hosted in this TUI process
	focused    bool // live and currently focused
	streaming  bool // live and a turn is in flight
	liveIdx    int  // index into m.lives.sessions; -1 for saved-only rows
	msgCount   int
	updatedAt  time.Time // store timestamp (zero when unknown)
	lastActive time.Time // last in-process output (zero when the session never ran here)
	seq        int       // persistent first-seen order (tie-break)
}

// activity is the row's sort key: last OUTPUT time.
//
// Rows with an in-process output stamp (live rows, or saved rows for
// sessions that ran in this process) use ONLY that stamp: the store's
// Updated is a PERSIST timestamp (rewritten by every flush/debounce/touch),
// so mixing it in would bump a row on bookkeeping writes, not on output —
// including the resume flush that rewrites the LEFT-BEHIND session's
// timestamp. Rows without in-process history fall back to the store
// timestamp.
func (r sidebarRow) activity() time.Time {
	if !r.lastActive.IsZero() {
		return r.lastActive
	}
	return r.updatedAt
}

// touchSessionActivity records a session's last in-process OUTPUT time in
// the per-id recency map (web pane.lastActivity). Keyed by session id —
// not by live slot — so a session keeps its earned list position after it
// stops being focused (resume rebind, pane close). Mutated only on the
// Update thread.
func (m *Model) touchSessionActivity(id string, t time.Time) {
	if id == "" {
		return
	}
	if m.sessionActivity == nil { // Models built as literals (tests) skip NewModel
		m.sessionActivity = make(map[string]time.Time)
	}
	m.sessionActivity[id] = t
}

// storeSessionTime returns a session's persisted timestamp from the
// saved-session index (ok=false when the index has no usable entry).
func (m *Model) storeSessionTime(id string) (time.Time, bool) {
	for _, si := range m.savedCache {
		if si.ID != id {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, si.UpdatedAt); err == nil {
			return ts, true
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// effectiveLastActive is a live session's in-process output stamp: the max
// of the slot's stamp and the per-id memory. The two are kept in sync (turn
// end, rebind re-stamp); the max is the safety net so a rebound slot can
// never strand the previous session's stamp on the new row.
func (m *Model) effectiveLastActive(s *liveSession) time.Time {
	t := s.lastActive
	if s.agent != nil {
		if v := m.sessionActivity[s.agent.SessionID]; v.After(t) {
			t = v
		}
	}
	return t
}

// rebindActivity re-stamps the focused live session after the agent was
// rebound to a different session id (resume, /new, delete-current — web:
// makePane's initialActivity seed). A session that produced output in this
// process keeps its in-process stamp; a resumed session with no in-process
// history is pinned to its store timestamp so it keeps its earned list
// position instead of jumping to the top; a brand-new session (no store
// entry yet) starts fresh at the top.
func (m *Model) rebindActivity() {
	if m.lives == nil || m.sessionID == "" {
		return
	}
	if _, ok := m.sessionActivity[m.sessionID]; !ok {
		if ts, ok := m.storeSessionTime(m.sessionID); ok {
			m.touchSessionActivity(m.sessionID, ts)
		} else {
			m.touchSessionActivity(m.sessionID, time.Now())
		}
	}
	m.lives.Active().lastActive = m.sessionActivity[m.sessionID]
}

// buildSidebarRows merges the persisted-session index (m.savedCache) with
// the live registry into the unified list. Saved entries come first (store
// order, recency-sorted); live sessions overlay onto their saved row by
// agent SessionID, and live sessions with no store entry yet get fallback
// rows so they never disappear (the web's "creating…" pane fallback).
// sidebarSeqOf returns the row's persistent first-seen sequence: the
// tie-break for equal activity. Assigned once per id and never reset, so
// the order is immune to group reassignment (a session gaining its first
// store entry) and to the store's unstable equal-timestamp ordering —
// equal-activity rows keep their relative position forever.
func (m *Model) sidebarSeqOf(id string) int {
	if n, ok := m.sidebarSeq[id]; ok {
		return n
	}
	if m.sidebarSeq == nil { // Models built as literals (tests) skip NewModel
		m.sidebarSeq = make(map[string]int)
	}
	m.sidebarSeqNext++
	m.sidebarSeq[id] = m.sidebarSeqNext
	return m.sidebarSeqNext
}

func (m *Model) buildSidebarRows() []sidebarRow {
	if m.lives == nil {
		return nil
	}
	rows := make([]sidebarRow, 0, len(m.savedCache))
	covered := make(map[int]bool, len(m.lives.sessions)) // live sessions overlaid below
	for _, si := range m.savedCache {
		if si.ParentID != "" {
			continue // nested (subagent) rows are not part of the flat list
		}
		r := sidebarRow{id: si.ID, label: si.Label, msgCount: si.MessageCount, liveIdx: -1}
		if ts, err := time.Parse(time.RFC3339Nano, si.UpdatedAt); err == nil {
			r.updatedAt = ts
		}
		for i, s := range m.lives.sessions {
			if s.agent == nil || s.agent.SessionID != si.ID {
				continue
			}
			covered[i] = true
			r.live, r.liveIdx = true, i
			r.focused = i == m.lives.active
			r.streaming = s.streaming
			// Live label wins: the agent derives it from the first user
			// message (SessionLabelSnapshot — the same source the web
			// pushes as sessionLabel), so a fresh session shows
			// "New session…" until its first turn names it.
			if l := s.agent.SessionLabelSnapshot(); l != "" {
				r.label = l
			}
			// Message count: the live count is authoritative, but only
			// safe to read when the session is not mid-turn (Messages is
			// owned by the stream goroutine while streaming).
			if !s.streaming {
				r.msgCount = len(s.agent.Messages)
			}
			r.lastActive = m.effectiveLastActive(s)
			break
		}
		// In-process memory for a session that is no longer live (resume
		// rebind, pane close): its row keeps the output stamp instead of
		// the store's persist timestamp (which a flush rewrites to now).
		if !r.live {
			r.lastActive = m.sessionActivity[si.ID]
		}
		if r.label == "" {
			r.label = si.ID
		}
		r.seq = m.sidebarSeqOf(r.id)
		rows = append(rows, r)
	}
	for i, s := range m.lives.sessions {
		if covered[i] {
			continue
		}
		// No store entry yet: the row id is the agent's session id, or
		// the live registry id when the agent is not keyed yet.
		id := s.id
		if s.agent != nil && s.agent.SessionID != "" {
			id = s.agent.SessionID
		}
		r := sidebarRow{id: id, live: true, liveIdx: i, focused: i == m.lives.active,
			streaming: s.streaming, lastActive: m.effectiveLastActive(s)}
		if s.agent != nil {
			r.label = s.agent.SessionLabelSnapshot()
			if !s.streaming {
				r.msgCount = len(s.agent.Messages)
			}
		}
		if r.label == "" {
			r.label = "New session…"
		}
		r.seq = m.sidebarSeqOf(r.id)
		rows = append(rows, r)
	}
	// Sort by last output, newest first; ties by persistent first-seen
	// order (deterministic regardless of group membership or store order).
	slices.SortFunc(rows, func(a, b sidebarRow) int {
		if c := b.activity().Compare(a.activity()); c != 0 {
			return c
		}
		return a.seq - b.seq
	})
	return rows
}

// seedRootLastActive pins the startup session's output time to its store
// timestamp so a resumed OLD session keeps its earned position instead of
// starting at the top (web: a restored pane is not bumped; only genuinely
// new sessions start fresh). A session with no store entry keeps the
// process-start time set by newLiveSessions.
func (m *Model) seedRootLastActive() {
	if m.lives == nil || m.agent == nil {
		return
	}
	for _, si := range m.savedCache {
		if si.ID != m.agent.SessionID {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, si.UpdatedAt); err == nil {
			m.lives.sessions[0].lastActive = ts
			m.touchSessionActivity(m.agent.SessionID, ts)
		}
		return
	}
}

// relativeTime formats a timestamp the way the web sidebar's "3m ago"
// labels do.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 45*time.Second:
		return "now"
	case d < 90*time.Second:
		return "1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// sidebarVisibleRows is how many sessions fit in a panel of mainLines rows
// (border + header + footer reserved). Each session takes two lines (title
// + meta) plus a blank separator after it — except the last rendered one —
// so k sessions occupy 3k-1 lines. Zero when the panel is too short for
// even one session — the footer still pins to the bottom.
func (m *Model) sidebarVisibleRows(mainLines int) int {
	avail := mainLines - sidebarBorderLines - sidebarHeaderLines - sidebarFooterLines
	v := (avail + 1) / 3
	if v < 0 {
		v = 0
	}
	return v
}

// clampSidebarScroll keeps m.sidebarScroll within the current list bounds.
func (m *Model) clampSidebarScroll() {
	if m.sidebarMainLines <= 0 {
		m.sidebarScroll = 0
		return
	}
	maxScroll := len(m.buildSidebarRows()) - m.sidebarVisibleRows(m.sidebarMainLines)
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.sidebarScroll < 0 {
		m.sidebarScroll = 0
	}
	if m.sidebarScroll > maxScroll {
		m.sidebarScroll = maxScroll
	}
}

// sidebarFocusedRow is the list index of the focused session (for cursor
// initialization when the panel gains keyboard focus).
func (m *Model) sidebarFocusedRow() int {
	for i, r := range m.buildSidebarRows() {
		if r.focused {
			return i
		}
	}
	return 0
}

// renderSidebar renders the unified sessions panel: EXACTLY mainLines rows
// tall (no trailing newline), so JoinHorizontal can never grow the combined
// frame past the terminal. Layout: box border (top row, right column,
// bottom row) around the header, working dir, and session rows (title:
// label + ✕ action; meta: state dot + state · msgs · relative time — the
// web row's structure), with a blank separator line between consecutive
// sessions. The border is dim, highlighted while the mouse is over the
// panel.
func (m *Model) renderSidebar(mainLines int) string {
	w := m.sidebarWidth
	if w < minSidebarWidth {
		w = minSidebarWidth
	}
	inner := w - 3 // "│ " prefix + content cells (the right "│" is appended)
	m.sidebarMainLines = mainLines

	rows := m.buildSidebarRows()
	visible := m.sidebarVisibleRows(mainLines)
	if m.sidebarCursor < 0 {
		m.sidebarCursor = 0
	}
	if m.sidebarCursor >= len(rows) {
		m.sidebarCursor = max(0, len(rows)-1)
	}
	// Keep the cursor inside the visible window — but only when the
	// cursor itself moved since the last render (keyboard/click nav). A
	// wheel scroll moves the window away from the cursor and must not be
	// snapped back to it on the next frame.
	if m.sidebarCursor != m.sidebarLastCursor {
		if m.sidebarCursor < m.sidebarScroll {
			m.sidebarScroll = m.sidebarCursor
		}
		if m.sidebarCursor >= m.sidebarScroll+visible {
			m.sidebarScroll = m.sidebarCursor - visible + 1
		}
	}
	m.sidebarLastCursor = m.sidebarCursor
	m.clampSidebarScroll()

	// dim pads a (possibly pre-styled) content string to the panel width.
	// Measurement is ANSI-aware so styled meta lines truncate and pad by
	// their VISIBLE width (raw-rune counting would break on escape codes).
	dim := func(s string) string {
		return ansiDimOn + "│ " + ansi.Cut(s, 0, inner) +
			strings.Repeat(" ", max(0, inner-ansi.StringWidth(s))) + ansiReset
	}
	out := make([]string, 0, mainLines)
	add := func(s string) {
		if len(out) < mainLines-sidebarBorderLines {
			out = append(out, s)
		}
	}

	add(ansiHighlightOn + "│ " + "SESSIONS" +
		strings.Repeat(" ", max(0, inner-8)) + ansiReset)
	wd := ""
	if m.agent != nil {
		wd = m.agent.WorkingDir
	}
	add(dim(wd))
	add(dim(""))

	for i := 0; i < visible && m.sidebarScroll+i < len(rows); i++ {
		r := rows[m.sidebarScroll+i]
		add(m.renderSidebarTitle(r, i+m.sidebarScroll == m.sidebarCursor, inner))
		add(dim(m.sidebarMeta(r)))
		// Blank separator between consecutive sessions (none after the
		// last rendered one).
		if i+1 < visible && m.sidebarScroll+i+1 < len(rows) {
			add(dim(""))
		}
	}

	// Pad, then pin the footer to the bottom two rows (above the bottom
	// border).
	for len(out) < mainLines-sidebarBorderLines-sidebarFooterLines {
		add(dim(""))
	}
	add(dim(""))
	// A modal overlay parks the mouse handlers, so never render a stale
	// hover highlight under it.
	if m.modal != ModalNone {
		m.sidebarHover = sidebarFooterNone
	}
	add(m.renderSidebarFooter(inner))
	if len(out) > mainLines-sidebarBorderLines {
		out = out[:mainLines-sidebarBorderLines]
	}

	// Box border: dim normally, highlighted while the mouse is over the
	// panel (sidebarHovering, tracked in handleMouseMsg). Every inner row
	// is exactly w-1 cells wide, so the appended right border brings each
	// line to exactly w — the panel's column budget.
	borderStyle := ansiDimOn
	if m.sidebarHovering {
		borderStyle = ansiCyanOn
	}
	for i := range out {
		out[i] += borderStyle + "│" + ansiReset
	}
	top := borderStyle + "┌" + strings.Repeat("─", w-2) + "┐" + ansiReset
	bottom := borderStyle + "└" + strings.Repeat("─", w-2) + "┘" + ansiReset
	return strings.Join(append(append([]string{top}, out...), bottom), "\n")
}

// sidebarFooterPlain is the footer's unstyled text (the measure for the
// narrow-panel fallback).
const sidebarFooterPlain = sidebarFooterNewLabel + "  " + sidebarFooterCloseLabel + "  " +
	sidebarFooterDeleteLabel + "  " + sidebarFooterRest

// sidebarFooterButtonsLen is the content width of the three button
// segments plus their gaps — the minimum inner width at which they render
// (and are hit-testable).
const sidebarFooterButtonsLen = len(sidebarFooterNewLabel) + 2 + len(sidebarFooterCloseLabel) +
	2 + len(sidebarFooterDeleteLabel)

// renderSidebarFooter renders the panel's footer hint line with the
// "n new", "x close", and "d del" segments styled as clickable buttons
// (cyan; the hovered button gets the row-highlight style). The buttons
// always render first; only the hint tail truncates when the panel is
// narrow. The line is padded to the panel width like the other rows, with
// one cell reserved before the right border so the hint never runs flush
// against it; a panel too narrow for the buttons falls back to the plain
// truncated hint (and sidebarFooterAt misses too).
func (m *Model) renderSidebarFooter(inner int) string {
	if inner < sidebarFooterButtonsLen {
		// Reserve one cell before the right border like the button form.
		return ansiDimOn + "│ " + sliceByRuneCount(sidebarFooterPlain, max(0, inner-1)) +
			" " + ansiReset
	}
	btn := func(b int) string {
		if m.sidebarHover == b {
			return ansiHighlightOn
		}
		return ansiCyanOn
	}
	tail := "  " + sidebarFooterRest
	if inner < len(sidebarFooterPlain)+1 {
		tail = sliceByRuneCount(tail, inner-sidebarFooterButtonsLen-1)
	}
	line := btn(sidebarFooterNew) + sidebarFooterNewLabel + ansiReset + "  " +
		btn(sidebarFooterClose) + sidebarFooterCloseLabel + ansiReset + "  " +
		btn(sidebarFooterDelete) + sidebarFooterDeleteLabel + ansiReset +
		ansiDimOn + tail
	// line carries ANSI codes — measure the VISIBLE width (sliceRuneLen
	// counts the escape runes too, which collapsed the padding and left
	// the right border off the panel edge on wide panels).
	pad := inner - lipgloss.Width(line)
	if pad < 0 {
		pad = 0
	}
	return ansiDimOn + "│ " + line + strings.Repeat(" ", pad) + ansiReset
}

// sidebarFooterAt reports which footer button a position lands on
// (sidebarFooterNew/sidebarFooterClose), with the same ±2 tolerance the row
// ✕ zone uses. ok=false for the non-interactive footer parts, every other
// row, and a panel too narrow to render the buttons.
func (m *Model) sidebarFooterAt(x, y int) (button int, ok bool) {
	if m.sidebarMainLines <= 0 || y != m.sidebarMainLines-sidebarBorderLines {
		return sidebarFooterNone, false
	}
	if m.sidebarWidth-3 < sidebarFooterButtonsLen {
		return sidebarFooterNone, false // buttons not rendered at this width
	}
	for _, z := range sidebarFooterZones() {
		if x >= z.x0-2 && x <= z.x1+2 {
			return z.button, true
		}
	}
	return sidebarFooterNone, false
}

// renderSidebarTitle renders one row's title line the way the web row's
// title does: a plain full-fg label (the cursor row gets the highlight
// style — the web "current" row's emphasis) with the right-aligned ✕
// action. The state dot lives on the meta line (web parity — see
// sidebarMeta). The ✕ sits two cells inside the drag handle so click zones
// never overlap (see sidebarRowAt).
func (m *Model) renderSidebarTitle(r sidebarRow, cursor bool, inner int) string {
	prefix := "  "
	if cursor {
		prefix = "▸ "
	}
	// Content cells: prefix(2) + label + pad + "✕ "(2).
	labelMax := inner - 5
	if labelMax < 2 {
		labelMax = 2
	}
	label := sliceByRuneCount(r.label, labelMax)
	pad := inner - 4 - sliceRuneLen(label)
	if pad < 1 {
		pad = 1
	}

	var b strings.Builder
	b.WriteString(ansiDimOn)
	b.WriteString("│ " + prefix)
	if cursor {
		b.WriteString(ansiHighlightOn)
	} else {
		b.WriteString(ansiReset) // web title is full fg, not dim
	}
	b.WriteString(label)
	b.WriteString(ansiReset)
	b.WriteString(strings.Repeat(" ", pad))
	b.WriteString(ansiDimOn)
	b.WriteString("✕ ")
	b.WriteString(ansiReset)
	return b.String()
}

// sidebarMeta renders one row's meta line the way the web row's meta row
// does: a colored state dot (amber ● responding / green ● active / cyan ●
// open) ahead of the muted state label, then message count and relative
// time, " · "-separated. Saved rows carry no state — the web shows no dot
// for them either.
func (m *Model) sidebarMeta(r sidebarRow) string {
	dotStyle, state := "", ""
	switch {
	case r.streaming:
		dotStyle, state = ansiPromptOn, "responding"
	case r.live && r.focused:
		dotStyle, state = ansiGreenOn, "active"
	case r.live:
		dotStyle, state = ansiCyanOn, "open"
	}
	var b strings.Builder
	b.WriteString("  ")
	if state != "" {
		b.WriteString(dotStyle)
		b.WriteString("● ")
		b.WriteString(ansiDimOn)
		b.WriteString(state)
	}
	if r.msgCount > 0 {
		if state != "" {
			b.WriteString(" · ")
		}
		b.WriteString(ansiDimOn)
		b.WriteString(fmt.Sprintf("%d msgs", r.msgCount))
	}
	if rel := relativeTime(r.activity()); rel != "" {
		if state != "" || r.msgCount > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(ansiDimOn)
		b.WriteString(rel)
	}
	b.WriteString(ansiReset)
	return b.String()
}

// sidebarRowAt maps a mouse position to (row index, action-zone). ok=false
// when the position is outside the row area (border/header/footer rows).
// Each session occupies a three-line group (title + meta + blank
// separator); the separator line maps to the session above it. The action
// zone is the ✕ glyph at the right edge of a TITLE line: it closes a live
// row (stays saved) or deletes a saved row (confirm). Row groups start at
// sidebarHeaderLines+1 (the top border row is 0).
func (m *Model) sidebarRowAt(x, y int) (row int, actionZone bool, ok bool) {
	if m.sidebarOffsetX() == 0 || x < 0 || x >= m.sidebarOffsetX() {
		return 0, false, false
	}
	start := sidebarHeaderLines + sidebarBorderLines - 1
	if y < start || m.sidebarMainLines <= 0 {
		return 0, false, false
	}
	visible := m.sidebarVisibleRows(m.sidebarMainLines)
	// The window is 3*visible-1 lines tall (no trailing separator).
	if y >= start+3*visible-1 {
		return 0, false, false
	}
	rows := m.buildSidebarRows()
	i := (y-start)/3 + m.sidebarScroll
	if i < 0 || i >= len(rows) {
		return 0, false, false
	}
	// ✕ sits at content column inner-2 → terminal column sidebarWidth-3;
	// accept a ±2 tolerance. The drag handle (sidebarWidth-2..+1) is
	// consumed earlier by handleSidebarResizeMouse, so the zones are
	// disjoint. The title line is the first line of each 3-line group.
	actionZone = (y-start)%3 == 0 &&
		x >= m.sidebarWidth-5 && x <= m.sidebarWidth-3
	return i, actionZone, true
}

// handleSidebarMouse processes mouse events over the panel: wheel scrolls
// the list, left-press selects the row and acts on it (row body →
// focus/resume, ✕ → close/delete, footer "n new"/"x close" → the n/x key
// actions), and button-less motion tracks the footer-button hover
// highlight. Returns true when the event was consumed (it must NOT start a
// text selection or scroll the chat).
func (m *Model) handleSidebarMouse(ev mouseEvent) bool {
	if m.modal != ModalNone || m.sidebarOffsetX() == 0 {
		return false
	}
	switch {
	case ev.kind == mouseWheelEvent && ev.x < m.sidebarOffsetX():
		// Scroll the session list (the web list scrolls the same way);
		// never let the wheel move the chat under the panel.
		if ev.button == tea.MouseWheelUp {
			m.sidebarScroll--
		} else {
			m.sidebarScroll++
		}
		m.clampSidebarScroll()
		return true
	case ev.kind == mouseMotion && ev.button == tea.MouseNone && ev.x < m.sidebarOffsetX():
		// Hover highlight over the footer buttons (cell motion is on).
		// Button-less motion never drives text selection, so consuming it
		// over the panel is safe; held-button motion falls through.
		hover := sidebarFooterNone
		if b, ok := m.sidebarFooterAt(ev.x, ev.y); ok {
			hover = b
		}
		if hover != m.sidebarHover {
			m.sidebarHover = hover
		}
		return true
	case ev.kind == mousePress && ev.button == tea.MouseLeft && ev.x < m.sidebarOffsetX():
		m.sidebarHover = sidebarFooterNone
		// Footer buttons act like their keys: "n new" spawns a live
		// session, "x close" closes the cursor row's pane (stays saved),
		// "d del" deletes the cursor row with confirmation.
		if b, ok := m.sidebarFooterAt(ev.x, ev.y); ok {
			switch b {
			case sidebarFooterNew:
				m.openNewLiveSession("")
				return true
			case sidebarFooterClose:
				if len(m.buildSidebarRows()) > 0 {
					m.sidebarCloseRow(m.sidebarCursor)
				}
				return true
			case sidebarFooterDelete:
				if len(m.buildSidebarRows()) > 0 {
					m.sidebarDeleteRow(m.sidebarCursor)
				}
				return true
			}
		}
		row, actionZone, ok := m.sidebarRowAt(ev.x, ev.y)
		if !ok {
			return true // header/footer: consume, no selection
		}
		m.sidebarCursor = row
		m.focus = FocusSidebar
		if actionZone {
			// Web parity: ✕ on a live row closes the pane (stays
			// saved); ✕ on a saved row deletes it (with confirmation).
			if rows := m.buildSidebarRows(); rows[row].live {
				m.sidebarCloseRow(row)
			} else {
				m.sidebarDeleteRow(row)
			}
			return true
		}
		m.sidebarOpenRow(row)
		return true
	}
	return false
}

// handleSidebarKey dispatches keys when the sessions panel has focus.
// Mouse is the primary interaction (web parity); this is the keyboard
// equivalent: ↑↓/jk move, enter open, x close (stays saved), d delete
// (confirm), n new, i/esc back.
func (m *Model) handleSidebarKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Help) {
		m.modal = ModalHelp
		return m, nil
	}
	if key.Matches(msg, m.keys.CancelTurn) {
		if m.streaming {
			m.streaming = false
			m.clearProgress()
			m.cancelActiveStream()
			m.resetStreamState(false)
			m.appendChatLine(SystemStyle.Render("Cancelled."))
			return m, nil
		}
		m.flushAndQuit()
		return m, tea.Quit
	}
	rows := m.buildSidebarRows()
	n := len(rows)
	switch msg.String() {
	case "up", "k":
		if m.sidebarCursor > 0 {
			m.sidebarCursor--
		}
	case "down", "j":
		if n > 0 && m.sidebarCursor < n-1 {
			m.sidebarCursor++
		}
	case "pgup":
		if m.sidebarCursor > 10 {
			m.sidebarCursor -= 10
		} else {
			m.sidebarCursor = 0
		}
	case "pgdown":
		if n > 0 && m.sidebarCursor < n-10 {
			m.sidebarCursor += 10
		} else {
			m.sidebarCursor = max(0, n-1)
		}
	case "home", "g":
		m.sidebarCursor = 0
	case "end", "G":
		if n > 0 {
			m.sidebarCursor = n - 1
		}
	case "enter":
		return m, m.sidebarOpenRow(m.sidebarCursor)
	case "x":
		if n > 0 {
			return m, m.sidebarCloseRow(m.sidebarCursor)
		}
	case "d":
		if n > 0 {
			return m, m.sidebarDeleteRow(m.sidebarCursor)
		}
	case "n":
		return m, m.openNewLiveSession("")
	case "i", "esc":
		m.focus = FocusInput
		return m, m.textarea.Focus()
	case "[":
		m.resizeSidebar(-4)
	case "]":
		m.resizeSidebar(4)
	}
	// Any other printable character jumps to the input and types (same as
	// viewport focus) — the panel is a browser, not a text field.
	if c := msg.Code; c >= 32 && c < 127 && msg.Mod == 0 {
		m.focus = FocusInput
		focusCmd := m.textarea.Focus()
		var updateCmd tea.Cmd
		m.textarea, updateCmd = m.textarea.Update(msg)
		return m, tea.Batch(focusCmd, updateCmd)
	}
	return m, nil
}

// sidebarOpenRow focuses a live row (switchToLive) or resumes a saved row
// (rebinds the focused agent — the same converge-on-switch contract as
// /resume). No-op when the row is already the focused session.
func (m *Model) sidebarOpenRow(row int) tea.Cmd {
	rows := m.buildSidebarRows()
	if row < 0 || row >= len(rows) {
		return nil
	}
	r := rows[row]
	if r.live {
		if r.liveIdx == m.lives.active {
			return nil
		}
		return m.switchToLive(r.liveIdx)
	}
	return m.resumeSavedRow(r.id)
}

// sidebarCloseRow closes the row's live pane — the session stays SAVED and
// its row reappears as a saved row (web closePane). Close and delete are
// distinct actions: this never deletes. A saved row has nothing to close;
// a responding session must be cancelled first; the FOCUSED session closes
// by moving focus to the newest-output remaining live session (web
// closePane parity) — the last open session cannot be closed.
func (m *Model) sidebarCloseRow(row int) tea.Cmd {
	rows := m.buildSidebarRows()
	if row < 0 || row >= len(rows) {
		return nil
	}
	r := rows[row]
	if !r.live {
		m.statusMsg = "Close: session is saved, not open in this TUI (d deletes it)"
		return nil
	}
	if s := m.lives.sessions[r.liveIdx]; s.streaming {
		m.statusMsg = "Close: " + r.label + " is responding — cancel its turn first"
		return nil
	}
	if r.focused {
		if len(m.lives.sessions) == 1 {
			m.statusMsg = "Close: the only open session cannot be closed (d deletes it)"
			return nil
		}
		// Web parity (closePane): focus moves to the newest-output
		// remaining live session.
		target := -1
		var best time.Time
		for i, s := range m.lives.sessions {
			if i == r.liveIdx {
				continue
			}
			if at := m.effectiveLastActive(s); target == -1 || at.After(best) {
				target, best = i, at
			}
		}
		cmd := m.switchToLive(target)
		if err := m.lives.Close(r.liveIdx); err != nil {
			m.statusMsg = "Close: " + err.Error()
			return nil
		}
		m.statusMsg = "Closed " + r.label + " (still saved)"
		m.refreshSavedSessions()
		m.sidebarCursor = max(0, min(m.sidebarCursor, len(m.buildSidebarRows())-1))
		return cmd
	}
	if err := m.lives.Close(r.liveIdx); err != nil {
		m.statusMsg = "Close: " + err.Error()
		return nil
	}
	m.statusMsg = "Closed " + r.label + " (still saved)"
	m.refreshSavedSessions()
	m.sidebarCursor = max(0, min(m.sidebarCursor, len(m.buildSidebarRows())-1))
	return nil
}

// sidebarDeleteRow asks for confirmation and PERMANENTLY deletes the row's
// session (web deleteSession). A background LIVE row must be closed first
// (x): deleting behind a hosted agent would leave it running on a deleted
// id. The focused live row deletes through the agent, which starts a fresh
// session in its place.
func (m *Model) sidebarDeleteRow(row int) tea.Cmd {
	rows := m.buildSidebarRows()
	if row < 0 || row >= len(rows) {
		return nil
	}
	r := rows[row]
	if r.live && !r.focused {
		m.statusMsg = "Delete: close the session first (x), then delete it"
		return nil
	}
	m.confirmText = fmt.Sprintf("Delete session %q?\nIts message history will be permanently deleted.", r.label)
	m.confirmAction = func() (tea.Model, tea.Cmd) {
		m.modal = ModalNone
		return m, m.deleteSavedRow(r.id)
	}
	m.modal = ModalConfirm
	return nil
}

// resumeSavedRow attaches a saved session to the focused agent (the web's
// openSessionPane). Shared by the sidebar and the /resume sessions modal.
func (m *Model) resumeSavedRow(id string) tea.Cmd {
	if id == "" || id == m.sessionID {
		return nil
	}
	result, _, err := m.agent.HandleSessionCommand(m.ctx, "resume "+id, session.NewID())
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
		return nil
	}
	if result.Action == agent.SessionActionClearChat {
		m.chatLines = nil
		m.chatLines = append(m.chatLines, SystemStyle.Render(result.Output))
		if len(result.History) > 0 {
			m.chatLines = append(m.chatLines, renderMessages(result.History, m.agent.WorkingDir, m.agent.CurrentModel(), m.agent.Mode.String())...)
		}
		m.setViewportContent()
		m.viewport.GotoBottom()
		m.sessionID = m.agent.SessionID
		m.refreshContextStats()
		m.refreshSavedSessions()
		// The resumed session keeps its earned position (web: makePane
		// seeds initialActivity from the saved session's updatedAt); the
		// left-behind session keeps its in-process output stamp.
		m.rebindActivity()
		m.sidebarCursor = m.sidebarFocusedRow()
	}
	return nil
}

// deleteSavedRow deletes a saved session by id (the web's deleteSession).
// Shared by the sidebar ✕/x action and the /resume sessions modal.
func (m *Model) deleteSavedRow(id string) tea.Cmd {
	result, _, err := m.agent.HandleSessionCommand(m.ctx, "resume del "+id, session.NewID())
	if err != nil {
		m.appendChatLine(ErrorStyle.Render(fmt.Sprintf("Session: %v", err)))
		return nil
	}
	if result.Action == agent.SessionActionClearChat {
		// The deleted session was the current one: the agent started a
		// fresh session — rebuild the transcript around the notice.
		m.chatLines = nil
		m.chatLines = append(m.chatLines, SystemStyle.Render(result.Output))
		m.setViewportContent()
		m.viewport.GotoBottom()
		m.sessionID = m.agent.SessionID
		// The fresh session has no store entry yet: creation is its first
		// recency event, so it starts at the top (web: makePane without
		// initialActivity).
		m.rebindActivity()
		m.refreshContextStats()
	} else {
		m.appendChatLine(SystemStyle.Render(result.Output))
	}
	m.refreshSavedSessions()
	m.sidebarCursor = 0
	return nil
}

// openNewLiveSession spawns an additional live session through the shared
// web lifecycle core and focuses it immediately (the web sidebar's "New"
// button). Shared by /open and the panel's n key.
func (m *Model) openNewLiveSession(label string) tea.Cmd {
	if m.workspace == nil {
		m.appendChatLine(ErrorStyle.Render("Open: no workspace attached (single-session host)."))
		return nil
	}
	a := m.workspace.NewSessionAgent(nil, session.NewID())
	// Mirror NewModel's subagent wiring: the TUI spawner is parent-generic
	// (it builds each child from the calling agent + shared cfg), so opened
	// sessions get working subagents instead of "spawner not installed".
	if m.cfg != nil && m.cfg.SubagentEnabled() {
		a.SetSubagentSpawner(&tuiSubagentSpawner{cfg: m.cfg, m: m})
	}
	s := m.lives.Add(a, "")
	// Creation is the fresh session's first recency event (web: makePane
	// without initialActivity) — it starts at the top of the list.
	m.touchSessionActivity(a.SessionID, time.Now())
	if label == "" {
		// Derive from the unique registry id so labels never collide after
		// closes ("session-2", "session-4", …).
		label = "session-" + strings.TrimPrefix(s.id, "s")
	}
	s.label = label
	// Job-completion notices for opened sessions: the workspace deliverer
	// is nil on the TUI host, so wire a focus-aware hook here. Focused →
	// normal delivery turn; background → attributed system line via the
	// replay buffer (a global delivery turn would run on whichever session
	// is focused, not this one).
	if m.cfg != nil && m.cfg.JobNoticesEnabled() {
		// Capture the sender HERE (Update thread): the hook fires from a
		// background goroutine and must not read m.program, which is
		// written once in TUI.Run without synchronization.
		sender := m.program
		a.SetJobNoticeHook(func(summary string) {
			if s.focused.Load() && sender != nil {
				sender.Send(deliveryRequestMsg{text: summary})
				return
			}
			s.enqueue(condensedNoteMsg{note: noticeLabel + " job finished: " + summary, sid: s.id})
		})
	}
	i := len(m.lives.sessions) - 1
	cmd := m.switchToLive(i)
	m.appendChatLine(SystemStyle.Render(fmt.Sprintf("Opened live session %q (%s).", label, s.id)))
	m.refreshSavedSessions()
	m.sidebarCursor = 0 // newest output sorts first
	return cmd
}

// sidebarTickMsg re-renders the panel's relative-time labels and picks up
// store changes (the web's 30 s sidebar tick). One loop runs for the whole
// program lifetime (started in Init); it is a no-op while the panel is
// hidden, so toggling can never fork a second loop.
type sidebarTickMsg struct{}

// sidebarTickCmd schedules the next 30 s panel refresh.
func sidebarTickCmd() tea.Cmd {
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg { return sidebarTickMsg{} })
}

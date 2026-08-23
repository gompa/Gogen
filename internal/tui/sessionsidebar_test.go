package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// newSidebarFullModel builds a model over a real agent with a session
// store (WorkingDir set) so resume/delete/refresh paths run for real.
func newSidebarFullModel(t *testing.T) *Model {
	t.Helper()
	a := newSwitchTestAgent(t)
	a.SessionStore = session.NewStore(true)
	a.SessionID = "cur"
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	ta := textarea.New()
	ta.SetHeight(3)
	m := &Model{
		agent:          a,
		lives:          newLiveSessions(a),
		viewport:       vp,
		textarea:       ta,
		ctx:            context.Background(),
		width:          100,
		height:         30,
		sidebarVisible: true,
		sidebarWidth:   defaultSidebarWidth,
	}
	return m
}

func TestBuildSidebarRowsOverlay(t *testing.T) {
	m := newSidebarFullModel(t)
	// A saved session the live agent is NOT on.
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.refreshSavedSessions()
	m.lives.Add(newSwitchTestAgent(t), "bg") // fresh id, not in the store

	rows := m.buildSidebarRows()
	byID := map[string]sidebarRow{}
	for _, r := range rows {
		byID[r.id] = r
	}
	// The saved row stays in the list, not live.
	saved, ok := byID["abc"]
	if !ok || saved.live {
		t.Fatalf("saved row missing or marked live: %+v", byID)
	}
	// The focused session has no store entry yet → fallback row.
	cur, ok := byID["cur"]
	if !ok || !cur.live || !cur.focused || cur.label != "New session…" {
		t.Fatalf("focused fallback row wrong: %+v", cur)
	}
	// The background live session overlays as live, unfocused.
	bg := byID["s2"]
	if !bg.live || bg.focused || bg.liveIdx != 1 {
		t.Fatalf("background live row wrong: %+v", bg)
	}
}

// Ordering is by last output time; focusing a session must NOT reorder the
// list (web parity: only new output moves a row).
func TestSidebarOrderingFocusNeverReorders(t *testing.T) {
	m := newSidebarFullModel(t)
	m.agent.SessionID = "a" // the focused live session is the OLDER one
	m.lives.sessions[0].agent.SessionID = "a"
	// No new output since the store timestamp (process start is not output).
	m.lives.sessions[0].lastActive = time.Time{}
	m.savedCache = []agent.SessionInfo{
		{ID: "a", UpdatedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)},
		{ID: "b", UpdatedAt: time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339Nano)},
	}
	rows := m.buildSidebarRows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].id != "b" || rows[1].id != "a" {
		t.Fatalf("order = %q,%q — newer output must sort first", rows[0].id, rows[1].id)
	}
	if !rows[1].live || !rows[1].focused {
		t.Fatalf("older row must be the live focused one: %+v", rows[1])
	}
	// A live turn on the focused (older) session bumps it to the top.
	m.lives.sessions[0].lastActive = time.Now()
	rows = m.buildSidebarRows()
	if rows[0].id != "a" {
		t.Fatalf("new output did not move the row: %+v", rows[0])
	}
}

// sidebarRowAt: title/meta lines map to rows; the blank separator line
// maps to the session above it; the ✕ zone is the right edge of a TITLE
// line only; header/footer and out-of-panel positions miss.
func TestSidebarRowAt(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	m.sidebarWidth = 26               // panel spans columns 0..25
	m.lives.Add(&agent.Agent{}, "bg") // second session → a separator renders

	t.Run("title line maps to row 0", func(t *testing.T) {
		row, closeZone, ok := m.sidebarRowAt(5, 4)
		if !ok || row != 0 || closeZone {
			t.Fatalf("row=%d close=%v ok=%v", row, closeZone, ok)
		}
	})
	t.Run("meta line maps to the same row", func(t *testing.T) {
		row, closeZone, ok := m.sidebarRowAt(5, 5)
		if !ok || row != 0 || closeZone {
			t.Fatalf("row=%d close=%v ok=%v", row, closeZone, ok)
		}
	})
	t.Run("separator line maps to the session above", func(t *testing.T) {
		row, closeZone, ok := m.sidebarRowAt(5, 6)
		if !ok || row != 0 || closeZone {
			t.Fatalf("row=%d close=%v ok=%v", row, closeZone, ok)
		}
	})
	t.Run("next session title line maps to row 1", func(t *testing.T) {
		row, closeZone, ok := m.sidebarRowAt(m.sidebarWidth-3, 7)
		if !ok || row != 1 || !closeZone {
			t.Fatalf("row=%d close=%v ok=%v", row, closeZone, ok)
		}
	})
	t.Run("header misses", func(t *testing.T) {
		if _, _, ok := m.sidebarRowAt(5, 2); ok {
			t.Fatal("header row must not map")
		}
	})
	t.Run("top border misses", func(t *testing.T) {
		if _, _, ok := m.sidebarRowAt(5, 0); ok {
			t.Fatal("border row must not map")
		}
	})
	t.Run("close zone on title line", func(t *testing.T) {
		row, closeZone, ok := m.sidebarRowAt(m.sidebarWidth-3, 4)
		if !ok || row != 0 || !closeZone {
			t.Fatalf("row=%d close=%v ok=%v", row, closeZone, ok)
		}
	})
	t.Run("close zone not on meta line", func(t *testing.T) {
		_, closeZone, _ := m.sidebarRowAt(m.sidebarWidth-3, 5)
		if closeZone {
			t.Fatal("meta line must not be a close zone")
		}
	})
	t.Run("out of panel misses", func(t *testing.T) {
		if _, _, ok := m.sidebarRowAt(m.sidebarWidth+3, 3); ok {
			t.Fatal("chat-column position must not map")
		}
	})
}

func TestSidebarMouseClickFocusesAndSwitches(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	// Deterministic ordering: the focused session has no recent output,
	// the background session was just spawned (newest → row 0).
	m.lives.sessions[0].lastActive = time.Now().Add(-time.Minute)
	bg := newSwitchTestAgent(t)
	m.lives.Add(bg, "bg")
	if row, _, ok := m.sidebarRowAt(5, 4); !ok || row != 0 {
		t.Fatal("row 0 must map")
	}
	rows := m.buildSidebarRows()
	if rows[0].liveIdx != 1 {
		t.Fatalf("row 0 must be the background session: %+v", rows[0])
	}
	// Click row 0's title: selects it and switches focus.
	ev := mouseEvent{x: 5, y: 4, button: tea.MouseLeft, kind: mousePress}
	if !m.handleSidebarMouse(ev) {
		t.Fatal("panel click not consumed")
	}
	if m.focus != FocusSidebar {
		t.Fatalf("focus = %v, want FocusSidebar", m.focus)
	}
	if m.lives.active != 1 || m.agent != bg {
		t.Fatal("click did not switch to the background session")
	}
	if m.sidebarCursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.sidebarCursor)
	}
}

func TestSidebarMouseCloseAndDelete(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	// Deterministic ordering: bg (just spawned) is row 0, focused "cur" row 1.
	m.lives.sessions[0].lastActive = time.Now().Add(-time.Minute)
	bg := newSwitchTestAgent(t)
	m.lives.Add(bg, "bg")

	t.Run("close zone on background live row closes the pane", func(t *testing.T) {
		// Row 0 (bg) title line is y=4; ✕ zone at sidebarWidth-3.
		ev := mouseEvent{x: m.sidebarWidth - 3, y: 4, button: tea.MouseLeft, kind: mousePress}
		if !m.handleSidebarMouse(ev) {
			t.Fatal("close click not consumed")
		}
		if len(m.lives.sessions) != 1 {
			t.Fatalf("background session not closed: %d live", len(m.lives.sessions))
		}
		if m.modal != ModalNone {
			t.Fatal("closing a background pane must not confirm")
		}
	})

	t.Run("close zone on the only focused live row refuses", func(t *testing.T) {
		// After the close, the focused session is row 0 again; it is the
		// ONLY open session, so ✕ must refuse (and never delete).
		ev := mouseEvent{x: m.sidebarWidth - 3, y: 4, button: tea.MouseLeft, kind: mousePress}
		m.handleSidebarMouse(ev)
		if m.modal != ModalNone {
			t.Fatalf("modal = %v, want ModalNone (close never confirms)", m.modal)
		}
		if len(m.lives.sessions) != 1 {
			t.Fatalf("the only open session must not close: %d live", len(m.lives.sessions))
		}
		if m.statusMsg == "" {
			t.Fatal("refusal must surface a status message")
		}
	})

	t.Run("close zone on a saved row asks for delete", func(t *testing.T) {
		if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
			WorkingDir: m.agent.WorkingDir,
			Messages:   []llm.Message{{Role: "user", Content: "hello"}},
		}); err != nil {
			t.Fatal(err)
		}
		// Keep the focused session (row 0) ahead of the fresh save.
		m.lives.sessions[0].lastActive = time.Now()
		m.refreshSavedSessions()
		savedRow := -1
		for i, r := range m.buildSidebarRows() {
			if r.id == "abc" && !r.live {
				savedRow = i
			}
		}
		if savedRow < 0 {
			t.Fatal("saved row missing from the list")
		}
		ev := mouseEvent{x: m.sidebarWidth - 3, y: sidebarHeaderLines + 1 + 3*savedRow, button: tea.MouseLeft, kind: mousePress}
		m.handleSidebarMouse(ev)
		if m.modal != ModalConfirm {
			t.Fatalf("modal = %v, want ModalConfirm", m.modal)
		}
		// Confirm the delete.
		m.handleConfirmKey(keyMsg("y"))
		if m.modal != ModalNone {
			t.Fatal("confirm must close the modal")
		}
		for _, si := range m.savedCache {
			if si.ID == "abc" {
				t.Fatal("deleted session still in the list")
			}
		}
		if len(m.lives.sessions) != 1 {
			t.Fatalf("deleting a saved row must not touch live sessions: %d live", len(m.lives.sessions))
		}
	})
}

func TestSidebarKeyNav(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	bg := newSwitchTestAgent(t)
	m.lives.Add(bg, "bg")
	m.focus = FocusSidebar
	m.sidebarCursor = 0

	m.handleSidebarKey(keyMsg("down"))
	if m.sidebarCursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.sidebarCursor)
	}
	m.handleSidebarKey(keyMsg("up"))
	if m.sidebarCursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.sidebarCursor)
	}
	// n without a workspace must surface an error line, not spawn.
	m.handleSidebarKey(keyMsg("n"))
	if len(m.lives.sessions) != 2 {
		t.Fatalf("n without workspace must not spawn: %d live", len(m.lives.sessions))
	}
	found := false
	for _, l := range m.chatLines {
		if strings.Contains(stripANSI(l), "Open:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("n without workspace must report an error line")
	}
	m.handleSidebarKey(keyMsg("i"))
	if m.focus != FocusInput {
		t.Fatalf("focus = %v, want FocusInput", m.focus)
	}
}

func TestViewportTabFocusesSidebar(t *testing.T) {
	m := newSidebarFullModel(t)
	m.focus = FocusViewport
	m.handleViewportKey(tea.KeyPressMsg{Code: '\t'})
	if m.focus != FocusSidebar {
		t.Fatalf("focus = %v, want FocusSidebar", m.focus)
	}
}

func TestConfirmModalKeys(t *testing.T) {
	m := newSidebarFullModel(t)
	m.confirmText = "Delete session \"x\"?"
	ran := false
	m.confirmAction = func() (tea.Model, tea.Cmd) { ran = true; return m, nil }
	m.modal = ModalConfirm

	if out := stripANSI(m.renderModal()); !strings.Contains(out, "Delete session") {
		t.Fatalf("confirm modal missing text:\n%s", out)
	}
	m.handleConfirmKey(keyMsg("y"))
	if !ran || m.modal != ModalNone || m.confirmAction != nil {
		t.Fatal("y must run the action exactly once and clear state")
	}

	ran = false
	m.confirmAction = func() (tea.Model, tea.Cmd) { ran = true; return m, nil }
	m.modal = ModalConfirm
	m.handleConfirmKey(keyMsg("n"))
	if ran || m.modal != ModalNone {
		t.Fatal("n must cancel without running the action")
	}
}

// Resume from a saved row rebinds the focused agent (web openSessionPane).
func TestSidebarResumeSavedRow(t *testing.T) {
	m := newSidebarFullModel(t)
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.resumeSavedRow("abc")
	if m.sessionID != "abc" {
		t.Fatalf("sessionID = %q, want abc", m.sessionID)
	}
	if m.agent.SessionID != "abc" {
		t.Fatal("agent not rebound to the resumed session")
	}
	// The live overlay now maps onto the resumed saved row.
	rows := m.buildSidebarRows()
	for _, r := range rows {
		if r.id == "abc" && (!r.live || !r.focused) {
			t.Fatalf("resumed row must be live+focused: %+v", r)
		}
	}
}

// Selecting a saved session must not reorder the list (web parity:
// activation keeps the earned position). The resumed session is pinned to
// its store timestamp, and the left-behind session keeps its in-process
// output stamp instead of jumping to the top on the resume flush (which
// rewrites its store timestamp to now).
func TestSidebarResumeKeepsPositions(t *testing.T) {
	m := newSidebarFullModel(t)
	now := time.Now()
	for _, c := range []struct {
		id  string
		ago time.Duration
	}{{"abc", 2 * time.Hour}, {"def", 3 * time.Hour}} {
		if err := m.agent.SessionStore.Save(c.id, agent.SessionSnapshot{
			WorkingDir: m.agent.WorkingDir,
			Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.agent.SessionStore.(*session.Store).SetUpdatedAt(m.agent.WorkingDir, c.id, now.Add(-c.ago).UTC()); err != nil {
			t.Fatal(err)
		}
	}
	// The focused session ("cur") had its last output 5 min ago.
	m.lives.sessions[0].lastActive = now.Add(-5 * time.Minute)
	m.touchSessionActivity("cur", now.Add(-5*time.Minute))
	m.refreshSavedSessions()

	ids := func() []string {
		rows := m.buildSidebarRows()
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.id)
		}
		return out
	}
	before := ids()
	if len(before) != 3 || before[0] != "cur" || before[1] != "abc" || before[2] != "def" {
		t.Fatalf("setup order = %v, want [cur abc def]", before)
	}

	// Select the "abc" row: "cur" is dirty, so the resume flushes it
	// (rewriting its store timestamp to now).
	m.agent.Messages = append(m.agent.Messages, llm.Message{Role: "user", Content: "hey"})
	m.agent.SetMode(m.agent.Mode)
	m.resumeSavedRow("abc")
	after := ids()
	if len(after) != 3 {
		t.Fatalf("row count changed: %v -> %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("selection reordered the list: %v -> %v (position %d: %q -> %q)",
				before, after, i, before[i], after[i])
		}
	}
	for _, r := range m.buildSidebarRows() {
		if r.id == "abc" && (!r.live || !r.focused) {
			t.Fatalf("resumed row must be live+focused in place: %+v", r)
		}
	}

	// Switch back: "cur" keeps its in-process stamp (5 min), not the
	// flush timestamp (now).
	m.resumeSavedRow("cur")
	back := ids()
	for i := range before {
		if i < len(back) && before[i] != back[i] {
			t.Fatalf("switch-back reordered the list: %v -> %v", before, back)
		}
	}
}

// Delete from the sidebar removes the store entry (web deleteSession).
func TestSidebarDeleteSavedRow(t *testing.T) {
	m := newSidebarFullModel(t)
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.deleteSavedRow("abc")
	m.refreshSavedSessions()
	for _, si := range m.savedCache {
		if si.ID == "abc" {
			t.Fatal("deleted session still in the list")
		}
	}
}

// Only a COMPLETED turn bumps the row: cancelled/failed turns produced no
// output, so the row must keep its position (web: lastActivity is written
// on output events only).
func TestSidebarTurnEndStamping(t *testing.T) {
	m := newSidebarFullModel(t)
	m.lives.sessions[0].lastActive = time.Now().Add(-time.Hour)
	before := m.lives.sessions[0].lastActive

	m.handleTurnFinishedMsg("s1", context.Canceled)
	if !m.lives.sessions[0].lastActive.Equal(before) {
		t.Fatal("cancelled turn must not bump the row")
	}

	m.handleTurnFinishedMsg("s1", nil)
	if m.lives.sessions[0].lastActive.Equal(before) {
		t.Fatal("completed turn must bump the row")
	}
}

// A session gaining its first store entry moves groups (live fallback →
// saved overlay) but must NOT reshuffle equal-activity rows: the
// persistent first-seen sequence is the tie-break.
func TestSidebarFirstSaveDoesNotReshuffleTies(t *testing.T) {
	m := newSidebarFullModel(t)
	ts := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	m.lives.sessions[0].lastActive = time.Now().Add(-time.Hour)
	m.savedCache = []agent.SessionInfo{
		{ID: "x", UpdatedAt: ts},
		{ID: "y", UpdatedAt: ts},
	}
	// The live "cur" session is NOT in the store yet (fallback group).
	ids := func() []string {
		var out []string
		for _, r := range m.buildSidebarRows() {
			out = append(out, r.id)
		}
		return out
	}
	first := ids()
	if len(first) != 3 {
		t.Fatalf("rows = %v, want 3", first)
	}
	// First save lands: "cur" now overlays a store entry.
	m.savedCache = append(m.savedCache, agent.SessionInfo{ID: "cur", UpdatedAt: ts})
	second := ids()
	if len(first) != len(second) {
		t.Fatalf("row count changed: %v -> %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("equal-activity rows reshuffled on first save: %v -> %v", first, second)
		}
	}
}

// A resumed OLD session keeps its earned position: the root's output time
// is pinned to the store timestamp, not the process start.
func TestSidebarSeedRootLastActive(t *testing.T) {
	m := newSidebarFullModel(t)
	m.lives.sessions[0].lastActive = time.Now() // process start (newLiveSessions)
	old := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	m.savedCache = []agent.SessionInfo{{ID: "cur", UpdatedAt: old}}
	m.seedRootLastActive()
	got, err := time.Parse(time.RFC3339Nano, m.lives.sessions[0].lastActive.UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := time.Parse(time.RFC3339Nano, old)
	if got.Sub(want) > time.Second || want.Sub(got) > time.Second {
		t.Fatalf("root lastActive = %v, want ~%v", got, want)
	}

	// No store entry: the process-start time is kept (genuinely new).
	m2 := newSidebarFullModel(t)
	m2.lives.sessions[0].lastActive = time.Now()
	kept := m2.lives.sessions[0].lastActive
	m2.seedRootLastActive()
	if !m2.lives.sessions[0].lastActive.Equal(kept) {
		t.Fatal("new session must keep its fresh position")
	}
}

// The footer's "n new" / "x close" segments are clickable buttons that act
// like the n / x keys, with a hover highlight on the button under the mouse.
func TestSidebarFooterButtons(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	// Deterministic ordering: bg (just spawned) is row 0, focused "cur" row 1.
	m.lives.sessions[0].lastActive = time.Now().Add(-time.Hour)
	bg := newSwitchTestAgent(t)
	m.lives.Add(bg, "bg")

	footerY := m.sidebarMainLines - 2 // above the bottom border row
	newX := 4                         // inside "n new" (columns 2..6)
	closeX := 12                      // inside "x close" (columns 9..15)
	delX := 20                        // inside "d del" (columns 18..22)

	t.Run("hover tracks the button under the mouse", func(t *testing.T) {
		m.handleSidebarMouse(mouseEvent{x: newX, y: footerY, button: tea.MouseNone, kind: mouseMotion})
		if m.sidebarHover != sidebarFooterNew {
			t.Fatalf("hover = %d, want new", m.sidebarHover)
		}
		m.handleSidebarMouse(mouseEvent{x: closeX, y: footerY, button: tea.MouseNone, kind: mouseMotion})
		if m.sidebarHover != sidebarFooterClose {
			t.Fatalf("hover = %d, want close", m.sidebarHover)
		}
		m.handleSidebarMouse(mouseEvent{x: delX, y: footerY, button: tea.MouseNone, kind: mouseMotion})
		if m.sidebarHover != sidebarFooterDelete {
			t.Fatalf("hover = %d, want delete", m.sidebarHover)
		}
		m.handleSidebarMouse(mouseEvent{x: 25, y: footerY, button: tea.MouseNone, kind: mouseMotion})
		if m.sidebarHover != sidebarFooterNone {
			t.Fatalf("hover = %d, want none off-button", m.sidebarHover)
		}
		m.handleSidebarMouse(mouseEvent{x: 5, y: 3, button: tea.MouseNone, kind: mouseMotion})
		if m.sidebarHover != sidebarFooterNone {
			t.Fatalf("hover = %d, want none off-footer", m.sidebarHover)
		}
	})

	t.Run("non-button footer parts are consumed but inert", func(t *testing.T) {
		if !m.handleSidebarMouse(mouseEvent{x: 25, y: footerY, button: tea.MouseLeft, kind: mousePress}) {
			t.Fatal("footer click must be consumed")
		}
		if len(m.lives.sessions) != 2 || m.modal != ModalNone {
			t.Fatalf("inert footer click acted: %d live, modal %v", len(m.lives.sessions), m.modal)
		}
	})

	t.Run("x close closes the cursor row without deleting", func(t *testing.T) {
		m.sidebarCursor = 0 // background live row
		m.handleSidebarMouse(mouseEvent{x: closeX, y: footerY, button: tea.MouseLeft, kind: mousePress})
		if len(m.lives.sessions) != 1 {
			t.Fatalf("cursor row not closed: %d live", len(m.lives.sessions))
		}
		if m.modal != ModalNone {
			t.Fatal("closing a background pane must not confirm")
		}
		// The cursor clamps to row 0 (the focused session): close refuses
		// the only open session and NEVER deletes.
		m.handleSidebarMouse(mouseEvent{x: closeX, y: footerY, button: tea.MouseLeft, kind: mousePress})
		if m.modal != ModalNone || len(m.lives.sessions) != 1 {
			t.Fatalf("close must refuse the last session without deleting: modal %v, %d live",
				m.modal, len(m.lives.sessions))
		}
	})

	t.Run("d del asks for delete confirm on the cursor row", func(t *testing.T) {
		m.sidebarCursor = 0 // focused live row
		m.handleSidebarMouse(mouseEvent{x: delX, y: footerY, button: tea.MouseLeft, kind: mousePress})
		if m.modal != ModalConfirm {
			t.Fatalf("modal = %v, want ModalConfirm", m.modal)
		}
		m.handleConfirmKey(keyMsg("esc"))
		if m.modal != ModalNone || len(m.lives.sessions) != 1 {
			t.Fatal("cancel must leave everything intact")
		}
	})

	t.Run("d del on a background live row demands close first", func(t *testing.T) {
		m.lives.Add(newSwitchTestAgent(t), "bg2")
		m.sidebarCursor = 0 // bg2 (just spawned, newest → row 0)
		m.handleSidebarMouse(mouseEvent{x: delX, y: footerY, button: tea.MouseLeft, kind: mousePress})
		if m.modal != ModalNone {
			t.Fatalf("modal = %v, want ModalNone", m.modal)
		}
		if m.statusMsg == "" {
			t.Fatal("must surface a status message")
		}
		if len(m.lives.sessions) != 2 {
			t.Fatalf("nothing may be deleted: %d live", len(m.lives.sessions))
		}
	})

	t.Run("n new without a workspace reports an error", func(t *testing.T) {
		before := len(m.lives.sessions)
		m.handleSidebarMouse(mouseEvent{x: newX, y: footerY, button: tea.MouseLeft, kind: mousePress})
		if len(m.lives.sessions) != before {
			t.Fatalf("n new without workspace must not spawn: %d -> %d live", before, len(m.lives.sessions))
		}
		found := false
		for _, l := range m.chatLines {
			if strings.Contains(stripANSI(l), "Open:") {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("n new without workspace must report an error line")
		}
	})
}

func TestSidebarFooterRendersButtons(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	m.sidebarWidth = maxSidebarWidth // wide enough for the full footer

	lines := strings.Split(m.renderSidebar(30), "\n")
	footer := stripANSI(lines[len(lines)-2]) // above the bottom border
	for _, want := range []string{"n new", "x close", "d del", "[/] size", "^b hide"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer missing %q: %q", want, footer)
		}
	}
	// At the DEFAULT width the hint tail truncates but all three buttons
	// still render (and stay hit-testable).
	m.sidebarWidth = defaultSidebarWidth
	lines = strings.Split(m.renderSidebar(30), "\n")
	footer = stripANSI(lines[len(lines)-2])
	for _, want := range []string{"n new", "x close", "d del"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("default-width footer missing %q: %q", want, footer)
		}
	}

	// The hovered button gets the row-highlight style, the other does not.
	// (Style assertions are meaningless under NO_COLOR: prefixes are empty.)
	m.sidebarHover = sidebarFooterNew
	out := m.renderSidebar(30)
	if !noColor {
		if !strings.Contains(out, ansiHighlightOn+sidebarFooterNewLabel) {
			t.Fatal("hovered button must render with the highlight style")
		}
		if strings.Contains(out, ansiHighlightOn+sidebarFooterCloseLabel) {
			t.Fatal("non-hovered button must not be highlighted")
		}
	}

	// A modal overlay clears the stale hover.
	m.modal = ModalConfirm
	m.renderSidebar(30)
	if m.sidebarHover != sidebarFooterNone {
		t.Fatal("modal must clear the footer hover")
	}
}

// The footer (shortcut-hint) row must be EXACTLY sidebarWidth cells wide at
// every panel width — the ANSI-styled line used to be padded with a
// raw-rune measure, which collapsed the padding on wide panels and left
// the right border off the panel edge. The hint must also keep one cell
// of breathing room before the border.
func TestSidebarFooterRowWidth(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	for _, w := range []int{minSidebarWidth, defaultSidebarWidth, 44, maxSidebarWidth} {
		m.sidebarWidth = w
		lines := strings.Split(m.renderSidebar(30), "\n")
		for i, l := range lines {
			if n := len([]rune(stripANSI(l))); n != w {
				t.Fatalf("width %d: row %d is %d cells, want %d: %q",
					w, i, n, w, stripANSI(l))
			}
		}
		footer := stripANSI(lines[len(lines)-2])
		if !strings.HasSuffix(footer, " │") {
			t.Fatalf("width %d: footer must end with a space before the border: %q", w, footer)
		}
	}
}

// Web-parity row structure: the title line is a plain label (no state
// dot) with the ✕ action, and the state dot lives on the META line ahead
// of the state label — live rows carry a dot, saved rows do not.
func TestSidebarRowWebParity(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	m.sidebarWidth = defaultSidebarWidth
	m.savedCache = append(m.savedCache, agent.SessionInfo{
		ID: "old1", Label: "saved session",
		UpdatedAt: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano),
	})
	lines := strings.Split(m.renderSidebar(30), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = stripANSI(l)
	}
	// Session rows start at y=4 (border + 3 header lines), 3 lines each;
	// only the rendered sessions occupy the grid (padding follows).
	rendered := m.sidebarVisibleRows(30)
	if n := len(m.buildSidebarRows()); n < rendered {
		rendered = n
	}
	for i := 0; i < rendered; i++ {
		y := 4 + 3*i
		title, meta := plain[y], plain[y+1]
		if strings.Contains(title, "●") || strings.Contains(title, "○") {
			t.Fatalf("title line carries a state dot: %q", title)
		}
		if !strings.Contains(title, "✕") {
			t.Fatalf("title line missing the ✕ action: %q", title)
		}
		live := strings.Contains(meta, "active") || strings.Contains(meta, "open") ||
			strings.Contains(meta, "responding")
		dotted := strings.Contains(meta, "●")
		if live != dotted {
			t.Fatalf("state dot out of sync with state label (live=%v): %q", live, meta)
		}
	}
}

// Closing the FOCUSED session moves focus to the newest-output remaining
// live session (web closePane) and the closed session stays SAVED — close
// never deletes.
func TestSidebarCloseFocusedRow(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	bg := newSwitchTestAgent(t)
	// Production parity: every hosted session shares one workspace (store
	// + working dir), so the post-close refresh sees the saved session.
	bg.WorkingDir = m.agent.WorkingDir
	bg.SessionStore = m.agent.SessionStore
	m.lives.Add(bg, "bg")
	// Give the focused session content: empty (0-message, unlabeled)
	// sessions are deliberately not persisted (skipEmptySave).
	m.agent.Messages = append(m.agent.Messages, llm.Message{Role: "user", Content: "hi"})
	// bg was just spawned (newest) → row 0; the focused "cur" is row 1.
	m.sidebarCursor = 1
	m.handleSidebarKey(keyMsg("x"))
	if len(m.lives.sessions) != 1 {
		t.Fatalf("focused session not closed: %d live", len(m.lives.sessions))
	}
	if m.lives.active != 0 || m.agent != bg {
		t.Fatal("focus must move to the remaining live session")
	}
	// The closed session stays saved (flushed on close).
	found := false
	for _, si := range m.savedCache {
		if si.ID == "cur" {
			found = true
		}
	}
	if !found {
		t.Fatal("closed session must stay saved")
	}
}

// The d key deletes the cursor row with confirmation (x only closes).
func TestSidebarDeleteKey(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	m.focus = FocusSidebar
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Keep the focused session (row 0) ahead of the fresh save.
	m.lives.sessions[0].lastActive = time.Now()
	m.refreshSavedSessions()
	savedRow := -1
	for i, r := range m.buildSidebarRows() {
		if r.id == "abc" && !r.live {
			savedRow = i
		}
	}
	if savedRow < 0 {
		t.Fatal("saved row missing from the list")
	}
	m.sidebarCursor = savedRow
	m.handleSidebarKey(keyMsg("d"))
	if m.modal != ModalConfirm {
		t.Fatalf("modal = %v, want ModalConfirm", m.modal)
	}
	m.handleConfirmKey(keyMsg("y"))
	if m.modal != ModalNone {
		t.Fatal("confirm must close the modal")
	}
	for _, si := range m.savedCache {
		if si.ID == "abc" {
			t.Fatal("deleted session still in the list")
		}
	}
}

// The panel border renders dim normally and highlighted (cyan) while the
// mouse is over the panel area; it returns to dim when the mouse moves
// into the chat column.
func TestSidebarBorderHoverHighlight(t *testing.T) {
	if noColor {
		t.Skip("style assertions are meaningless under NO_COLOR")
	}
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30

	out := m.renderSidebar(30)
	if m.sidebarHovering {
		t.Fatal("fresh model must not be hovered")
	}
	if !strings.Contains(out, ansiDimOn+"┌") {
		t.Fatal("border must render dim when unhovered")
	}

	// Mouse over the panel area → highlighted border.
	m.handleMouseMsg(tea.MouseMotionMsg{X: 5, Y: 5, Button: tea.MouseNone})
	if !m.sidebarHovering {
		t.Fatal("motion over the panel must set sidebarHovering")
	}
	out = m.renderSidebar(30)
	if !strings.Contains(out, ansiCyanOn+"┌") {
		t.Fatal("border must render highlighted while hovered")
	}
	if !strings.Contains(out, ansiCyanOn+"└") {
		t.Fatal("bottom border must render highlighted while hovered")
	}

	// Mouse into the chat column → dim again.
	m.handleMouseMsg(tea.MouseMotionMsg{X: m.sidebarWidth + 5, Y: 5, Button: tea.MouseNone})
	if m.sidebarHovering {
		t.Fatal("motion over the chat column must clear sidebarHovering")
	}
}

// The 30 s tick keeps rescheduling itself (one loop for the lifetime of
// the program) and refreshes the store index while the panel is visible.
func TestSidebarTickReschedules(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarVisible = true
	_, cmd := m.Update(sidebarTickMsg{})
	if cmd == nil {
		t.Fatal("tick must reschedule itself")
	}
	m.sidebarVisible = false
	_, cmd = m.Update(sidebarTickMsg{})
	if cmd == nil {
		t.Fatal("tick keeps running while hidden (no-op), so it must still reschedule")
	}
}

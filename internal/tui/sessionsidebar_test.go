package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/server"
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
	if consumed, _ := m.handleSidebarMouse(ev); !consumed {
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

// Mouse-driven switching to a streaming session must propagate the
// progress spinner tick cmd (the keyboard path always did; the mouse path
// used to drop it, freezing the spinner on one static frame).
func TestSidebarMouseOpenStreamingReturnsCmd(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	// The background session (row 0) is mid-turn.
	m.lives.sessions[0].lastActive = time.Now().Add(-time.Minute)
	bg := newSwitchTestAgent(t)
	m.lives.Add(bg, "bg")
	m.lives.sessions[1].streaming = true

	ev := mouseEvent{x: 5, y: 4, button: tea.MouseLeft, kind: mousePress}
	consumed, cmd := m.handleSidebarMouse(ev)
	if !consumed {
		t.Fatal("panel click not consumed")
	}
	if m.lives.active != 1 {
		t.Fatalf("focus = %d, want 1", m.lives.active)
	}
	if cmd == nil {
		t.Fatal("switching to a streaming session must return the spinner tick cmd")
	}
	if m.progressPhase != progressThinking {
		t.Fatalf("progress = %v, want progressThinking", m.progressPhase)
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
		if consumed, _ := m.handleSidebarMouse(ev); !consumed {
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
		// The delete's list refresh is async; land it synchronously so
		// the row assertions run against the fresh index.
		m.refreshSavedSessions()
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

// Rebinding the focused agent (resume / delete-current) must be blocked
// while the focused session has an in-flight turn or compaction — from
// every entry point, not just the typed one: handleSubmitKey gates typed
// input, but sidebar actions and the confirm-modal callback bypass it.
func TestRebindBlockedWhileFocusedBusy(t *testing.T) {
	newModel := func(t *testing.T) *Model {
		t.Helper()
		m := newSidebarFullModel(t)
		m.sessionID = "cur" // NewModel mirrors a.SessionID; the harness skips it
		for _, id := range []string{"abc", "cur"} {
			if err := m.agent.SessionStore.Save(id, agent.SessionSnapshot{
				WorkingDir: m.agent.WorkingDir,
				Messages:   []llm.Message{{Role: "user", Content: "hello " + id}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		m.refreshSavedSessions()
		return m
	}
	chatContains := func(t *testing.T, m *Model, needle string) {
		t.Helper()
		for _, l := range m.chatLines {
			if strings.Contains(stripANSI(l), needle) {
				return
			}
		}
		t.Fatalf("chat missing %q:\n%s", needle, strings.Join(m.chatLines, "\n"))
	}
	storeHas := func(t *testing.T, m *Model, id string) bool {
		t.Helper()
		m.refreshSavedSessions()
		for _, si := range m.savedCache {
			if si.ID == id {
				return true
			}
		}
		return false
	}

	t.Run("resume blocked while streaming", func(t *testing.T) {
		m := newModel(t)
		m.streaming = true
		m.resumeSavedRow("abc")
		if m.sessionID != "cur" || m.agent.SessionID != "cur" {
			t.Fatalf("agent must not rebind: sessionID=%q agent=%q", m.sessionID, m.agent.SessionID)
		}
		chatContains(t, m, "Resume: wait for the current turn to finish")
	})

	t.Run("resume blocked while compacting", func(t *testing.T) {
		m := newModel(t)
		m.compacting = true
		m.resumeSavedRow("abc")
		if m.sessionID != "cur" || m.agent.SessionID != "cur" {
			t.Fatalf("agent must not rebind: sessionID=%q agent=%q", m.sessionID, m.agent.SessionID)
		}
		chatContains(t, m, "Resume: wait for compaction to finish")
	})

	t.Run("delete current blocked while streaming", func(t *testing.T) {
		m := newModel(t)
		m.streaming = true
		m.deleteSavedRow("cur")
		if m.agent.SessionID != "cur" {
			t.Fatalf("agent must not rebind: %q", m.agent.SessionID)
		}
		if !storeHas(t, m, "cur") {
			t.Fatal("current session file must survive the blocked delete")
		}
		chatContains(t, m, "Delete: wait for the current turn to finish")
	})

	t.Run("delete current blocked while compacting", func(t *testing.T) {
		m := newModel(t)
		m.compacting = true
		m.deleteSavedRow("cur")
		if m.agent.SessionID != "cur" {
			t.Fatalf("agent must not rebind: %q", m.agent.SessionID)
		}
		if !storeHas(t, m, "cur") {
			t.Fatal("current session file must survive the blocked delete")
		}
		chatContains(t, m, "Delete: wait for compaction to finish")
	})

	t.Run("delete non-current allowed while streaming", func(t *testing.T) {
		m := newModel(t)
		m.streaming = true
		m.deleteSavedRow("abc")
		if storeHas(t, m, "abc") {
			t.Fatal("non-current delete must proceed while the focused session streams")
		}
		if m.agent.SessionID != "cur" {
			t.Fatalf("focused agent must not rebind: %q", m.agent.SessionID)
		}
	})

	t.Run("sidebar delete of focused row refuses before confirm", func(t *testing.T) {
		m := newModel(t)
		m.streaming = true
		focusedRow := -1
		for i, r := range m.buildSidebarRows() {
			if r.focused {
				focusedRow = i
			}
		}
		if focusedRow < 0 {
			t.Fatal("focused row missing from the list")
		}
		m.sidebarDeleteRow(focusedRow)
		if m.modal != ModalNone {
			t.Fatalf("modal = %v, want ModalNone (no confirm while busy)", m.modal)
		}
		if !strings.Contains(m.statusMsg, "Delete: wait for the current turn to finish") {
			t.Fatalf("statusMsg = %q", m.statusMsg)
		}
	})

	t.Run("idle rebind still works", func(t *testing.T) {
		m := newModel(t)
		m.resumeSavedRow("abc")
		if m.sessionID != "abc" || m.agent.SessionID != "abc" {
			t.Fatalf("idle resume must rebind: sessionID=%q agent=%q", m.sessionID, m.agent.SessionID)
		}
	})
}

// Resuming a session that is still hosted in a background slot must FOCUS
// that slot (switchToLive), not rebind the focused agent onto the same
// SessionID: two agents with one SessionID both persist to the same file,
// and the rebound agent's first full save wipes the other's pending delta
// (partial restore on the next load). The web dedupes in loadOrCreateRuntime.
func TestResumeSavedRowLiveTargetSwitches(t *testing.T) {
	m := newSidebarFullModel(t)
	bg := newSwitchTestAgent(t)
	bg.SessionStore = m.agent.SessionStore
	bg.SessionID = "abc"
	bg.Messages = []llm.Message{{Role: "user", Content: "bg hello"}}
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "bg hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.lives.Add(bg, "bg")

	m.resumeSavedRow("abc")
	if m.agent != bg || m.lives.active != 1 {
		t.Fatalf("expected focus switch to the live slot: active=%d", m.lives.active)
	}
	if m.lives.sessions[0].agent.SessionID != "cur" {
		t.Fatal("left-behind slot was rebound")
	}
	if bg.SessionID != "abc" || len(bg.Messages) != 1 {
		t.Fatal("live slot's session state was clobbered")
	}
	if m.sessionID != "abc" {
		t.Fatalf("sessionID = %q, want abc", m.sessionID)
	}
}

// Resuming a SAVED session while the focused turn is in flight must be
// blocked: the rebind would swap Messages out from under the running
// stream goroutine (the left-behind session's in-flight reply would be
// lost and the old turn's tail would land under the resumed session's id).
func TestResumeSavedRowBlockedWhileStreaming(t *testing.T) {
	m := newSidebarFullModel(t)
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.sessionID = "cur"
	m.streaming = true
	m.lives.Active().streaming = true

	m.resumeSavedRow("abc")
	if m.agent.SessionID != "cur" || m.sessionID != "cur" {
		t.Fatal("rebind happened while the focused turn was streaming")
	}
	if len(m.chatLines) == 0 || !strings.Contains(m.chatLines[len(m.chatLines)-1], "wait for the current turn") {
		t.Fatalf("expected the blocked-resume notice, chatLines=%q", m.chatLines)
	}
}

// Opening a saved session while the focused session is busy must NOT be
// blocked (web openSessionPane parity): the saved session gets its OWN live
// agent loaded from the store snapshot — the focused agent is never
// rebound, so its in-flight turn keeps running in the background under its
// own SessionID (a rebind would swap Messages out from under the running
// stream goroutine and misattribute the turn's tail).
func TestResumeSavedRowSpawnsWhileBusy(t *testing.T) {
	newBusyModel := func(t *testing.T, compacting bool) *Model {
		t.Helper()
		m := newSidebarFullModel(t)
		m.sessionID = "cur"
		m.workspace = server.NewWorkspaceForHost(m.agent, nil)
		if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
			WorkingDir: m.agent.WorkingDir,
			Messages:   []llm.Message{{Role: "user", Content: "hello abc"}},
		}); err != nil {
			t.Fatal(err)
		}
		m.refreshSavedSessions()
		m.streaming = !compacting
		m.lives.Active().streaming = !compacting
		m.compacting = compacting
		m.lives.Active().compacting = compacting
		return m
	}
	for _, compacting := range []bool{false, true} {
		kind := "streaming"
		if compacting {
			kind = "compacting"
		}
		t.Run("spawn allowed while "+kind, func(t *testing.T) {
			m := newBusyModel(t, compacting)
			m.resumeSavedRow("abc")
			// The focused agent was NOT rebound: it keeps its session and
			// its in-flight turn.
			if m.lives.sessions[0].agent.SessionID != "cur" {
				t.Fatalf("left-behind agent was rebound: %q", m.lives.sessions[0].agent.SessionID)
			}
			if m.lives.sessions[0].streaming != !compacting || m.lives.sessions[0].compacting != compacting {
				t.Fatalf("left-behind busy flags clobbered: streaming=%v compacting=%v",
					m.lives.sessions[0].streaming, m.lives.sessions[0].compacting)
			}
			if len(m.lives.sessions) != 2 || m.lives.active != 1 {
				t.Fatalf("want a new focused live slot: sessions=%d active=%d", len(m.lives.sessions), m.lives.active)
			}
			focused := m.lives.Active()
			if focused.agent.SessionID != "abc" {
				t.Fatalf("focused agent = %q, want abc", focused.agent.SessionID)
			}
			if len(focused.agent.Messages) != 1 {
				t.Fatalf("restored messages = %d, want 1", len(focused.agent.Messages))
			}
			if m.sessionID != "abc" {
				t.Fatalf("sessionID = %q, want abc", m.sessionID)
			}
			// Joining an idle session unlocks the focused mirrors.
			if m.streaming || m.compacting {
				t.Fatalf("focused mirrors not unlocked: streaming=%v compacting=%v", m.streaming, m.compacting)
			}
			found := false
			for _, l := range m.chatLines {
				if strings.Contains(stripANSI(l), "Resumed session abc") {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("resume notice missing:\n%s", strings.Join(m.chatLines, "\n"))
			}
		})
	}
}

// With a workspace, opening a saved session SPAWNS a live slot even when
// the focused session is idle (web parity: one code path — the focused
// agent is never rebound, so the left-behind session stays live instead of
// becoming a saved row).
func TestResumeSavedRowSpawnsWhenIdle(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sessionID = "cur"
	m.workspace = server.NewWorkspaceForHost(m.agent, nil)
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello abc"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.refreshSavedSessions()

	m.resumeSavedRow("abc")
	if len(m.lives.sessions) != 2 || m.lives.active != 1 {
		t.Fatalf("want a new focused live slot: sessions=%d active=%d", len(m.lives.sessions), m.lives.active)
	}
	if m.lives.sessions[0].agent.SessionID != "cur" {
		t.Fatalf("left-behind agent was rebound: %q", m.lives.sessions[0].agent.SessionID)
	}
	if m.sessionID != "abc" {
		t.Fatalf("sessionID = %q, want abc", m.sessionID)
	}

	// Switching back focuses the original live slot (it was never saved
	// away — it is still hosted).
	i := m.lives.indexBySessionID("cur")
	if i != 0 {
		t.Fatalf("original session lost its live slot: index=%d", i)
	}
	m.switchToLive(i)
	if m.agent.SessionID != "cur" || m.lives.active != 0 {
		t.Fatalf("switch back failed: agent=%q active=%d", m.agent.SessionID, m.lives.active)
	}
	// Re-opening "abc" focuses ITS live slot instead of spawning a second
	// agent for the same SessionID.
	m.resumeSavedRow("abc")
	if len(m.lives.sessions) != 2 || m.lives.active != 1 {
		t.Fatalf("second open must focus the existing slot: sessions=%d active=%d", len(m.lives.sessions), m.lives.active)
	}
}

// Typed /resume shares the resumeSavedRow guards: a live background target
// is focused (not double-hosted), "resume latest" resolving to a live
// background session focuses it, and a saved target still rebinds.
func TestCmdSessionResumeGuards(t *testing.T) {
	m := newSidebarFullModel(t)
	bg := newSwitchTestAgent(t)
	bg.SessionStore = m.agent.SessionStore
	bg.SessionID = "abc"
	if err := m.agent.SessionStore.Save("abc", agent.SessionSnapshot{
		WorkingDir: m.agent.WorkingDir,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	m.lives.Add(bg, "bg")

	t.Run("typed resume of a live background session focuses it", func(t *testing.T) {
		handled, quit, _ := m.dispatchCommand("resume abc")
		if !handled || quit {
			t.Fatalf("handled=%v quit=%v", handled, quit)
		}
		if m.agent != bg || m.lives.active != 1 {
			t.Fatal("must focus the live slot, not rebind")
		}
		if m.lives.sessions[0].agent.SessionID != "cur" {
			t.Fatal("left-behind slot was rebound")
		}
	})

	t.Run("resume latest resolving to a live session focuses it", func(t *testing.T) {
		// Focus is on "abc" now; switch back so "abc" is the background
		// live target again.
		m.switchToLive(0)
		if m.lives.active != 0 {
			t.Fatal("setup: switch back failed")
		}
		handled, _, _ := m.dispatchCommand("resume latest")
		if !handled {
			t.Fatal("not handled")
		}
		if m.agent != bg || m.lives.active != 1 {
			t.Fatal("resume latest must focus the live background session")
		}
	})

	t.Run("typed resume of a saved session still rebinds", func(t *testing.T) {
		m2 := newSidebarFullModel(t)
		if err := m2.agent.SessionStore.Save("def", agent.SessionSnapshot{
			WorkingDir: m2.agent.WorkingDir,
			Messages:   []llm.Message{{Role: "user", Content: "saved"}},
		}); err != nil {
			t.Fatal(err)
		}
		handled, _, _ := m2.dispatchCommand("resume def")
		if !handled {
			t.Fatal("not handled")
		}
		if m2.agent.SessionID != "def" || m2.sessionID != "def" {
			t.Fatalf("rebind failed: agent=%q model=%q", m2.agent.SessionID, m2.sessionID)
		}
	})
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
	// The resume's list refresh is async; land it synchronously so the
	// ordering assertions run against the fresh index.
	m.refreshSavedSessions()
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

	m.handleTurnFinishedMsg("s1", 0, context.Canceled)
	if !m.lives.sessions[0].lastActive.Equal(before) {
		t.Fatal("cancelled turn must not bump the row")
	}

	m.handleTurnFinishedMsg("s1", 0, nil)
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
		if consumed, _ := m.handleSidebarMouse(mouseEvent{x: 25, y: footerY, button: tea.MouseLeft, kind: mousePress}); !consumed {
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
	// The close's list refresh is async; land it synchronously so the
	// saved-row assertion runs against the fresh index.
	m.refreshSavedSessions()
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
	// The delete's list refresh is async; land it synchronously so the
	// row assertion runs against the fresh index.
	m.refreshSavedSessions()
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

// ── Identity-based cursor: the selection follows the SESSION, not the slot ──

// Regression: the list reorders at runtime (a background turn completing
// bumps that session to the top). The cursor used to be a bare list index,
// so after a reorder the highlight jumped to a different session and x/d
// acted on the wrong one. The cursor must keep its session's identity.
func TestSidebarCursorFollowsSessionAcrossReorder(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	m.focus = FocusSidebar
	aB := newSwitchTestAgent(t)
	aC := newSwitchTestAgent(t)
	m.lives.Add(aB, "B")
	m.lives.Add(aC, "C")
	// Deterministic order: C (newest) row 0, B row 1, focused "cur" row 2.
	m.lives.sessions[0].lastActive = time.Now().Add(-3 * time.Minute)
	m.lives.sessions[1].lastActive = time.Now().Add(-2 * time.Minute)
	m.lives.sessions[2].lastActive = time.Now().Add(-time.Minute)

	rows := m.buildSidebarRows()
	cRow, bRow := -1, -1
	for i, r := range rows {
		if r.liveIdx == 2 {
			cRow = i // C
		}
		if r.liveIdx == 1 {
			bRow = i // B
		}
	}
	if cRow != 0 || bRow != 1 {
		t.Fatalf("setup order wrong: C=%d B=%d, want 0/1", cRow, bRow)
	}
	cID := rows[cRow].id
	m.setSidebarCursor(rows, cRow) // cursor on C (row 0)

	// B's turn completes in the background → B jumps to the top, C slides
	// from row 0 to row 1.
	m.lives.sessions[1].lastActive = time.Now()

	// The render resolves the cursor by identity: it must now point at row 1
	// (still C), not stay pinned to row 0 (now B).
	m.renderSidebar(30)
	rows = m.buildSidebarRows()
	if m.sidebarCursor != 1 || rows[1].id != cID {
		t.Fatalf("cursor = %d (%q), want row 1 (%q) — it must follow C across the reorder",
			m.sidebarCursor, rows[m.sidebarCursor].id, cID)
	}

	// x must close C (the selected session), not B, which slid into C's old
	// slot.
	m.handleSidebarKey(keyMsg("x"))
	if m.lives.ByID("s3") != nil {
		t.Fatal("C (the selected session) was not closed")
	}
	if m.lives.ByID("s2") == nil {
		t.Fatal("B (the session that slid into the cursor's old slot) must stay open")
	}
	if len(m.lives.sessions) != 2 {
		t.Fatalf("live sessions = %d, want 2", len(m.lives.sessions))
	}
}

// A reorder that relocates the cursor outside the visible window must move
// the window to it (the resolved position counts as a cursor move), so the
// selection can never be stranded off-screen.
func TestSidebarReorderKeepsCursorInWindow(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	// 9 live sessions → the 8-row window cannot show them all.
	agents := make([]*agent.Agent, 9)
	for i := range agents {
		agents[i] = newSwitchTestAgent(t)
		m.lives.Add(agents[i], fmt.Sprintf("s%d", i))
	}
	// Oldest first: agents[0] newest … agents[8] oldest (row 8, off-window).
	for i := range agents {
		m.lives.sessions[i+1].lastActive = time.Now().Add(time.Duration(i) * time.Minute)
	}
	m.lives.sessions[0].lastActive = time.Now().Add(-9 * time.Minute)
	m.renderSidebar(30)

	// Select the bottom row (row 8) — the window follows to show it.
	rows := m.buildSidebarRows()
	m.setSidebarCursor(rows, 8)
	m.renderSidebar(30)
	if m.sidebarScroll != 1 { // 8 - 8 + 1
		t.Fatalf("scroll = %d, want 1 (window must follow the cursor)", m.sidebarScroll)
	}

	// Now the TOP session completes → everything slides down by one: the
	// selected session moves from row 8 to row 9… it stays the last row,
	// but its identity must survive and the window must keep showing it.
	m.lives.sessions[0].lastActive = time.Now()
	m.renderSidebar(30)
	rows = m.buildSidebarRows()
	selID := m.sidebarCursorID
	if selID == "" {
		t.Fatal("cursor identity lost after the reorder")
	}
	found := -1
	for i, r := range rows {
		if r.id == selID {
			found = i
			break
		}
	}
	if found < 0 {
		t.Fatal("selected session vanished from the list")
	}
	if m.sidebarCursor != found {
		t.Fatalf("cursor = %d, want %d (the selected session's new position)", m.sidebarCursor, found)
	}
	if m.sidebarCursor < m.sidebarScroll || m.sidebarCursor >= m.sidebarScroll+m.sidebarVisibleRows(30) {
		t.Fatalf("cursor %d outside visible window [%d,%d)", m.sidebarCursor, m.sidebarScroll, m.sidebarScroll+m.sidebarVisibleRows(30))
	}
}

// Footer buttons hit EXACT columns: the two-column gaps between them (and
// the "│ " prefix) must be inert — the old ±2 tolerance let a gap click
// trigger the nearest button (e.g. "new session").
func TestSidebarFooterGapClicksInert(t *testing.T) {
	m := newSidebarFullModel(t)
	m.sidebarMainLines = 30
	footerY := m.sidebarMainLines - 2
	// Zones: "n new" cols 2-6, "x close" 9-15, "d del" 18-22.
	// Inert: prefix 0-1, gaps 7-8 and 16-17, tail 23+.
	for _, x := range []int{0, 1, 7, 8, 16, 17, 23, 25} {
		consumed, _ := m.handleSidebarMouse(mouseEvent{x: x, y: footerY, button: tea.MouseLeft, kind: mousePress})
		if !consumed {
			t.Fatalf("x=%d: footer click must be consumed", x)
		}
		if len(m.lives.sessions) != 1 {
			t.Fatalf("x=%d: gap click spawned a session", x)
		}
		if m.modal != ModalNone {
			t.Fatalf("x=%d: gap click opened a modal", x)
		}
		// Hover tracking must agree: a gap is not a button.
		m.handleSidebarMouse(mouseEvent{x: x, y: footerY, button: tea.MouseNone, kind: mouseMotion})
		if m.sidebarHover != sidebarFooterNone {
			t.Fatalf("x=%d: hover = %d, want none over a gap", x, m.sidebarHover)
		}
	}
	// The buttons themselves still work (exact columns).
	m.handleSidebarMouse(mouseEvent{x: 4, y: footerY, button: tea.MouseNone, kind: mouseMotion})
	if m.sidebarHover != sidebarFooterNew {
		t.Fatal("hover over 'n new' must track")
	}
}

// The border drag's press zone must not reach into the chat column: a press
// on the first chat column starts a text selection, not a panel drag.
func TestSidebarDragPressStaysInPanel(t *testing.T) {
	m := dragModel(t)
	press := func(x int) bool {
		return m.handleSidebarResizeMouse(mouseEvent{x: x, y: 5, button: tea.MouseLeft, kind: mousePress})
	}
	release := func() {
		m.handleSidebarResizeMouse(mouseEvent{kind: mouseRelease})
	}

	// First chat column (x == sidebarWidth): must NOT engage the drag.
	if press(m.sidebarWidth) {
		t.Fatal("press in the chat column must not start a border drag")
	}
	if m.sidebarDragging {
		t.Fatal("dragging flag set by a chat-column press")
	}
	// Border column and its one-column left tolerance still engage.
	if !press(m.sidebarWidth - 1) {
		t.Fatal("border press must start the drag")
	}
	release()
	if !press(m.sidebarWidth - 2) {
		t.Fatal("left-tolerance press must start the drag")
	}
	release()
	// Two columns left of the border is row content (the ✕ zone), not the
	// handle: it must fall through to selection.
	if press(m.sidebarWidth - 3) {
		t.Fatal("press on the ✕ column must not start a border drag")
	}
}

// Opening a live row from the sidebar must announce the switch with the
// same system line /switch and the active-sessions modal print, so every
// focus entry point gives the same feedback. The new-session path keeps
// its own "Opened live session" line (it never goes through here).
func TestSidebarOpenRowAnnouncesSwitch(t *testing.T) {
	m := newSidebarFullModel(t)
	a2 := newSwitchTestAgent(t)
	a2.SessionID = "second-id" // NewAgent leaves it empty; the row keys on it
	m.lives.Add(a2, "second")

	rows := m.buildSidebarRows()
	idx := -1
	for i, r := range rows {
		if r.id == a2.SessionID {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("second live session missing from sidebar rows")
	}

	m.sidebarOpenRow(idx)
	if m.agent != a2 {
		t.Fatal("sidebar open did not focus the live session")
	}
	if len(m.chatLines) == 0 {
		t.Fatal("no chat lines after opening a live row")
	}
	last := m.chatLines[len(m.chatLines)-1]
	if !strings.Contains(last, "Switched to session:") {
		t.Fatalf("last chat line = %q, want the switch feedback line", last)
	}
}

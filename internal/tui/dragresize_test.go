package tui

import (
	"os"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
)

func dragModel(t *testing.T) *Model {
	t.Helper()
	a := newSwitchTestAgent(t) // real agent → WorkingDir set for prefs persist
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	ta := textarea.New()
	ta.SetHeight(3)
	m := &Model{
		agent:          a,
		lives:          newLiveSessions(a),
		viewport:       vp,
		textarea:       ta,
		width:          100,
		height:         30,
		sidebarVisible: true,
		sidebarWidth:   26,
	}
	return m
}

func evDrag(kind mouseEventKind, x int) mouseEvent {
	return mouseEvent{x: x, y: 5, button: tea.MouseLeft, kind: kind}
}

func TestSidebarBorderDragResize(t *testing.T) {
	m := dragModel(t)
	border := m.sidebarWidth - 1 // 25

	t.Run("press on border begins drag without selecting", func(t *testing.T) {
		if !m.handleSidebarResizeMouse(evDrag(mousePress, border)) {
			t.Fatal("border press not consumed")
		}
		if !m.sidebarDragging {
			t.Fatal("dragging flag not set")
		}
		if m.selection != nil {
			t.Fatal("border press must not start a text selection")
		}
	})

	t.Run("motion maps width to cursor within clamps", func(t *testing.T) {
		if !m.handleSidebarResizeMouse(evDrag(mouseMotion, 34)) {
			t.Fatal("motion not consumed")
		}
		if m.sidebarWidth != 35 { // panel spans columns 0..x → width = x+1
			t.Fatalf("width = %d, want 35", m.sidebarWidth)
		}

		m.handleSidebarResizeMouse(evDrag(mouseMotion, 200)) // capped by the live max
		if m.sidebarWidth != m.sidebarMaxWidth() {
			t.Fatalf("max clamp failed: %d", m.sidebarWidth)
		}

		m.handleSidebarResizeMouse(evDrag(mouseMotion, 2)) // below min
		if m.sidebarWidth != m.sidebarMinWidth() {
			t.Fatalf("min clamp failed: %d", m.sidebarWidth)
		}
	})

	t.Run("release finalizes and persists prefs", func(t *testing.T) {
		m.handleSidebarResizeMouse(evDrag(mouseMotion, 30))
		if !m.handleSidebarResizeMouse(mouseEvent{button: tea.MouseLeft, kind: mouseRelease}) {
			t.Fatal("release not consumed")
		}
		if m.sidebarDragging {
			t.Fatal("dragging flag stuck")
		}
		if got := m.sidebarWidth; got != 31 {
			t.Fatalf("release width = %d, want 31", got)
		}
		data, err := os.ReadFile(uiPrefsPath(m.agent.WorkingDir))
		if err != nil || !containsInt(string(data), 31) {
			t.Fatalf("prefs not persisted (err=%v data=%q)", err, string(data))
		}
	})

	t.Run("press away from border does not engage drag", func(t *testing.T) {
		if m.handleSidebarResizeMouse(evDrag(mousePress, 5)) {
			t.Fatal("panel-interior press must fall through to selection")
		}
	})
}

func TestSidebarWheelScrollsList(t *testing.T) {
	m := dragModel(t)
	// The drag handler no longer owns the wheel: the list scroll does.
	if m.handleSidebarResizeMouse(mouseEvent{x: 3, y: 5, button: tea.MouseLeft, kind: mouseWheelEvent}) {
		t.Fatal("drag handler must not consume the panel wheel")
	}
	m.sidebarMainLines = 30
	m.sidebarScroll = 5
	// Enough saved rows for the list to actually overflow the panel.
	for i := 0; i < 20; i++ {
		m.savedCache = append(m.savedCache, agent.SessionInfo{ID: "s" + itoa(i)})
	}
	down := mouseEvent{x: 3, y: 5, button: tea.MouseWheelDown, kind: mouseWheelEvent}
	if !m.handleSidebarMouse(down) {
		t.Fatal("wheel over panel must be consumed by the list")
	}
	if m.sidebarScroll != 6 {
		t.Fatalf("scroll = %d, want 6", m.sidebarScroll)
	}
	up := mouseEvent{x: 3, y: 5, button: tea.MouseWheelUp, kind: mouseWheelEvent}
	m.handleSidebarMouse(up)
	if m.sidebarScroll != 5 {
		t.Fatalf("scroll = %d, want 5", m.sidebarScroll)
	}
	// Hidden panel: the wheel passes through to the chat viewport.
	m.sidebarVisible = false
	if m.handleSidebarMouse(down) {
		t.Fatal("wheel must pass through when sidebar hidden")
	}
}

// End-to-end through the REAL message path (tea.MouseWheelMsg → Update →
// handleMsg → handleMouseMsg): a wheel over the panel scrolls the session
// list and must not scroll the chat viewport; a wheel over the chat
// column does the opposite.
func TestSidebarWheelEndToEnd(t *testing.T) {
	m := dragModel(t)
	m.sidebarMainLines = 30
	for i := 0; i < 20; i++ {
		m.savedCache = append(m.savedCache, agent.SessionInfo{ID: "s" + itoa(i)})
	}
	m.renderSidebar(30) // register the row count used by clamping
	// Overflow the chat viewport so its scroll offset can move.
	for i := 0; i < 50; i++ {
		m.appendChatLine("line " + itoa(i))
	}
	m.setViewportContent()
	m.viewport.GotoBottom()
	vpBefore := m.viewport.YOffset
	if vpBefore == 0 {
		t.Fatal("test setup: viewport must start scrolled to the bottom")
	}

	m.Update(tea.MouseWheelMsg{X: 5, Y: 10, Button: tea.MouseWheelDown})
	if m.sidebarScroll != 1 {
		t.Fatalf("sidebarScroll = %d, want 1 (panel wheel must scroll the list)", m.sidebarScroll)
	}
	if m.viewport.YOffset != vpBefore {
		t.Fatal("wheel over the panel must not scroll the chat viewport")
	}
	// Regression: the render's keep-cursor-in-window logic used to snap
	// the wheel scroll back to the (unmoved) cursor on the next frame.
	m.renderSidebar(30)
	if m.sidebarScroll != 1 {
		t.Fatalf("render snapped the wheel scroll back to the cursor: %d", m.sidebarScroll)
	}

	m.Update(tea.MouseWheelMsg{X: m.sidebarWidth + 5, Y: 10, Button: tea.MouseWheelUp})
	if m.sidebarScroll != 1 {
		t.Fatalf("chat-column wheel scrolled the list: %d", m.sidebarScroll)
	}
	if m.viewport.YOffset == vpBefore {
		t.Fatal("chat-column wheel must scroll the viewport")
	}
}

// Keyboard navigation still scrolls the window to follow the cursor: the
// keep-in-window logic must fire when the CURSOR moves (wheel scrolls move
// the window instead, and are left alone — see TestSidebarWheelEndToEnd).
func TestSidebarKeyboardFollowScroll(t *testing.T) {
	m := dragModel(t)
	m.sidebarMainLines = 30
	for i := 0; i < 20; i++ {
		m.savedCache = append(m.savedCache, agent.SessionInfo{ID: "s" + itoa(i)})
	}
	m.renderSidebar(30)
	m.sidebarCursor = 15 // past the visible window (8 sessions at 30 lines)
	m.renderSidebar(30)
	if m.sidebarScroll != 8 { // 15 - 8 + 1
		t.Fatalf("scroll = %d, want 8 (window must follow the cursor)", m.sidebarScroll)
	}
}

// Web parity: the live clamp range scales with the terminal (12%..50%,
// never squeezing the main column below minMainWidth, capped at
// maxSidebarWidth), and keyboard resize can never auto-hide the panel.
func TestSidebarWidthClampsScaleWithTerminal(t *testing.T) {
	m := dragModel(t)

	t.Run("100-col terminal: 16..50", func(t *testing.T) {
		m.width = 100
		if m.sidebarMinWidth() != 16 || m.sidebarMaxWidth() != 50 {
			t.Fatalf("range = %d..%d, want 16..50", m.sidebarMinWidth(), m.sidebarMaxWidth())
		}
	})
	t.Run("80-col terminal: minMainWidth binds the max", func(t *testing.T) {
		m.width = 80
		if m.sidebarMinWidth() != 16 || m.sidebarMaxWidth() != 40 {
			t.Fatalf("range = %d..%d, want 16..40", m.sidebarMinWidth(), m.sidebarMaxWidth())
		}
	})
	t.Run("200-col terminal: 12% floor and absolute cap bind", func(t *testing.T) {
		m.width = 200
		if m.sidebarMinWidth() != 24 || m.sidebarMaxWidth() != maxSidebarWidth {
			t.Fatalf("range = %d..%d, want 24..%d", m.sidebarMinWidth(), m.sidebarMaxWidth(), maxSidebarWidth)
		}
	})
	t.Run("keyboard resize never auto-hides", func(t *testing.T) {
		m.width = 100
		m.sidebarWidth = 26
		m.resizeSidebar(999)
		if m.sidebarWidth != 50 {
			t.Fatalf("width = %d, want 50", m.sidebarWidth)
		}
		if m.sidebarOffsetX() == 0 {
			t.Fatal("resizing to the max must not auto-hide the panel")
		}
		if m.mainWidth() < minMainWidth {
			t.Fatalf("main column squeezed below min: %d", m.mainWidth())
		}
	})
}

func containsInt(s string, n int) bool {
	return strings.Contains(s, itoa(n))
}

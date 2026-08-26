package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// tocTestModel builds a wide, sidebar-free model with a 20-row viewport
// and a 31-line, two-prompt transcript (taller than the viewport so the
// scroll position is meaningful): prompt 1 at line 0, prompt 2 at line 11.
func tocTestModel(t *testing.T) *Model {
	t.Helper()
	m := dragModel(t)
	m.sidebarVisible = false
	m.viewport = NewViewport(m.mainWidth(), 20)
	m.viewport.Style = ViewportStyle
	lines := []string{
		UserStyle.Render(userLabel) + " first prompt",
		AssistantStyle.Render(assistantLabel) + " hello there",
	}
	for i := 0; i < 9; i++ {
		lines = append(lines, strings.Repeat("filler ", 10))
	}
	lines = append(lines, UserStyle.Render(userLabel)+" second prompt")
	for i := 0; i < 19; i++ {
		lines = append(lines, strings.Repeat("tail ", 10))
	}
	m.chatLines = lines
	m.setViewportContent()
	return m
}

func TestTocAnchors(t *testing.T) {
	m := tocTestModel(t)
	if len(m.tocAnchors) != 2 {
		t.Fatalf("anchors = %d, want 2: %+v", len(m.tocAnchors), m.tocAnchors)
	}
	if m.tocAnchors[0].line != 0 || m.tocAnchors[0].text != "first prompt" {
		t.Fatalf("anchor 0 wrong: %+v", m.tocAnchors[0])
	}
	want := 0
	for _, l := range m.chatLines[:11] {
		want += len(m.wrapLine(l))
	}
	if m.tocAnchors[1].line != want || m.tocAnchors[1].text != "second prompt" {
		t.Fatalf("anchor 1 = %+v, want line %d text %q", m.tocAnchors[1], want, "second prompt")
	}

	// Incremental append: a new prompt line extends the index without a
	// full rebuild (the web's appendTocDot path). It lands at the end of
	// the transcript — the total wrapped line count so far.
	want2 := 0
	for _, l := range m.chatLines {
		want2 += len(m.wrapLine(l))
	}
	m.appendChatLine(UserStyle.Render(userLabel) + " third prompt")
	if len(m.tocAnchors) != 3 || m.tocAnchors[2].text != "third prompt" {
		t.Fatalf("append anchor wrong: %+v", m.tocAnchors)
	}
	if m.tocAnchors[2].line != want2 {
		t.Fatalf("append anchor line = %d, want %d", m.tocAnchors[2].line, want2)
	}
}

func TestTocActiveAnchor(t *testing.T) {
	m := tocTestModel(t)
	// At the bottom the LAST prompt is active (web rule).
	m.viewport.GotoBottom()
	if got := m.tocActiveAnchor(); got != 1 {
		t.Fatalf("active at bottom = %d, want 1", got)
	}
	// Scrolled to the top: the probe line (35% down = row 7) sits between
	// the prompts (0 and 11), so the first is active.
	m.viewport.SetYOffset(0)
	if m.viewport.AtBottom() {
		t.Fatal("test setup: expected not at bottom")
	}
	if got := m.tocActiveAnchor(); got != 0 {
		t.Fatalf("active at top = %d, want 0", got)
	}
}

func TestTocRailRenders(t *testing.T) {
	m := tocTestModel(t)
	m.viewport.SetYOffset(0)
	m.tocHover = true
	frame := m.applyTocOverlay(m.renderMainColumn())
	lines := strings.Split(frame, "\n")

	// Every row keeps the frame width (ANSI-aware paste).
	for i, l := range lines {
		if n := len([]rune(stripANSI(l))); n != m.mainWidth() {
			t.Fatalf("row %d is %d cells, want %d", i, n, m.mainWidth())
		}
	}
	// Dots in the last column: active "●" for prompt 0 (above the 35%
	// probe at the top of the scroll), dim "·" for prompt 1.
	dotAt := func(y int) string {
		plain := stripANSI(lines[y])
		r := []rune(plain)
		return string(r[len(r)-1])
	}
	if got := dotAt(0); got != "●" {
		t.Fatalf("row 0 last cell = %q, want ●", got)
	}
	if got := dotAt(m.tocAnchors[1].line); got != "·" {
		t.Fatalf("row %d last cell = %q, want ·", m.tocAnchors[1].line, got)
	}
	// No rail at rest: the frame is untouched.
	m.tocHover = false
	if got := m.applyTocOverlay(m.renderMainColumn()); got != m.renderMainColumn() {
		t.Fatal("rail must be a no-op when not hovered")
	}
}

func TestTocTooltipRenders(t *testing.T) {
	m := tocTestModel(t)
	m.viewport.SetYOffset(0)
	m.tocHover = true
	m.tocPreview = 0
	frame := m.applyTocOverlay(m.renderMainColumn())
	plain := stripANSI(frame)
	if !strings.Contains(plain, "Prompt 1") || !strings.Contains(plain, "first prompt") {
		t.Fatalf("tooltip missing label/body:\n%s", plain)
	}
	for i, l := range strings.Split(plain, "\n") {
		if n := len([]rune(l)); n != m.mainWidth() {
			t.Fatalf("row %d is %d cells, want %d", i, n, m.mainWidth())
		}
	}
}

func TestTocMouse(t *testing.T) {
	m := tocTestModel(t)
	railX := m.mainWidth() - 1

	t.Run("motion in the strip shows the rail and consumes", func(t *testing.T) {
		ev := mouseEvent{x: railX, y: 0, kind: mouseMotion}
		if !m.handleTocMouse(ev) {
			t.Fatal("motion in the strip must be consumed")
		}
		if !m.tocHover {
			t.Fatal("motion in the strip must set tocHover")
		}
	})

	t.Run("motion elsewhere hides the rail", func(t *testing.T) {
		if m.handleTocMouse(mouseEvent{x: 5, y: 5, kind: mouseMotion}) {
			t.Fatal("motion outside the strip must not be consumed")
		}
		if m.tocHover || m.tocPreview != -1 {
			t.Fatal("motion outside the strip must clear the rail state")
		}
	})

	t.Run("click on a dot jumps and consumes", func(t *testing.T) {
		m.tocHover = true
		target := m.tocAnchors[1].line
		ev := mouseEvent{x: railX, y: target, button: tea.MouseLeft, kind: mousePress}
		if !m.handleTocMouse(ev) {
			t.Fatal("dot click must be consumed")
		}
		if m.viewport.YOffset != target {
			t.Fatalf("YOffset = %d, want %d", m.viewport.YOffset, target)
		}
	})

	t.Run("click in the strip off a dot consumes without jumping", func(t *testing.T) {
		m.tocHover = true
		m.viewport.SetYOffset(0)
		if !m.handleTocMouse(mouseEvent{x: railX, y: 10, button: tea.MouseLeft, kind: mousePress}) {
			t.Fatal("strip press must be consumed (no selection)")
		}
		if m.viewport.YOffset != 0 {
			t.Fatalf("YOffset = %d, want 0", m.viewport.YOffset)
		}
	})

	t.Run("wheel falls through to the viewport", func(t *testing.T) {
		if m.handleTocMouse(mouseEvent{x: railX, y: 5, button: tea.MouseWheelDown, kind: mouseWheelEvent}) {
			t.Fatal("wheel must fall through")
		}
	})

	t.Run("no prompts: the rail is inert", func(t *testing.T) {
		m.tocAnchors = nil
		m.tocHover = true
		if m.handleTocMouse(mouseEvent{x: railX, y: 0, kind: mouseMotion}) {
			t.Fatal("no anchors: motion must not be consumed")
		}
		if got := m.applyTocOverlay(m.renderMainColumn()); got != m.renderMainColumn() {
			t.Fatal("no anchors: overlay must be a no-op")
		}
	})
}

func TestTocKeyboardJump(t *testing.T) {
	m := tocTestModel(t)
	m.viewport.SetYOffset(0)

	if !m.tocJumpNext(true) {
		t.Fatal("next prompt must exist")
	}
	if m.viewport.YOffset != m.tocAnchors[1].line {
		t.Fatalf("YOffset = %d, want %d", m.viewport.YOffset, m.tocAnchors[1].line)
	}
	if !m.tocJumpNext(false) {
		t.Fatal("previous prompt must exist")
	}
	if m.viewport.YOffset != m.tocAnchors[0].line {
		t.Fatalf("YOffset = %d, want %d", m.viewport.YOffset, m.tocAnchors[0].line)
	}
	// Nothing above the first prompt → fall through (bracket gets typed).
	if m.tocJumpNext(false) {
		t.Fatal("no prompt above: must report false")
	}
	// No anchors at all → fall through.
	m.tocAnchors = nil
	if m.tocJumpNext(true) {
		t.Fatal("no anchors: must report false")
	}
}

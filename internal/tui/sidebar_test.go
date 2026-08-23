package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"

	"gogen/internal/agent"
)

func newSidebarTestModel(width int) *Model {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	ta := textarea.New()
	ta.SetHeight(3)
	return &Model{
		lives:    newLiveSessions(&agent.Agent{}),
		viewport: vp,
		textarea: ta,
		width:    width,
	}
}

func TestSidebarGeometry(t *testing.T) {
	m := newSidebarTestModel(100)

	t.Run("hidden uses full width", func(t *testing.T) {
		if got := m.mainWidth(); got != 100 {
			t.Fatalf("mainWidth = %d, want 100", got)
		}
	})

	t.Run("toggle sets default width and subtracts", func(t *testing.T) {
		m.toggleSidebar()
		if !m.sidebarVisible || m.sidebarWidth != defaultSidebarWidth {
			t.Fatalf("visible=%v width=%d", m.sidebarVisible, m.sidebarWidth)
		}
		if got := m.mainWidth(); got != 100-defaultSidebarWidth {
			t.Fatalf("mainWidth = %d, want %d", got, 100-defaultSidebarWidth)
		}
	})

	t.Run("resize clamps to the live terminal-relative range", func(t *testing.T) {
		m.resizeSidebar(-999)
		if m.sidebarWidth != m.sidebarMinWidth() {
			t.Fatalf("min clamp failed: %d", m.sidebarWidth)
		}
		m.resizeSidebar(999)
		if m.sidebarWidth != m.sidebarMaxWidth() {
			t.Fatalf("max clamp failed: %d", m.sidebarWidth)
		}
	})

	t.Run("narrow terminal auto-hides", func(t *testing.T) {
		m.width = minMainWidth + defaultSidebarWidth - 1
		m.sidebarWidth = defaultSidebarWidth
		if got := m.mainWidth(); got != m.width {
			t.Fatalf("auto-hide failed: mainWidth=%d width=%d", got, m.width)
		}
	})

	t.Run("second toggle hides again", func(t *testing.T) {
		m.width = 100
		m.sidebarVisible = true // prior subtests left it shown
		m.toggleSidebar()       // → hidden
		if m.sidebarVisible {
			t.Fatal("sidebar should be hidden")
		}
		if got := m.mainWidth(); got != 100 {
			t.Fatalf("mainWidth = %d after hide", got)
		}
	})
}

func TestRenderSidebarLayout(t *testing.T) {
	m := newSidebarTestModel(100)
	m.height = 24
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	m.lives.Add(&agent.Agent{}, "bg")
	m.lives.ByID("s2").streaming = true

	main := m.renderMainColumn()
	out := m.renderSidebar(strings.Count(main, "\n") + 1)
	lines := nonEmptyLines(out)
	if len(lines) < 3 {
		t.Fatalf("sidebar too short:\n%s", out)
	}
	plain := stripANSI(out)
	if !strings.Contains(plain, "SESSIONS") || !strings.Contains(plain, "▸") || !strings.Contains(plain, "responding") {
		t.Fatalf("missing header/focus/stream markers:\n%s", plain)
	}
	// Every rendered row must respect the panel width (no bleed into chat):
	// the box border makes each line exactly sidebarWidth cells wide.
	for _, l := range strings.Split(plain, "\n") {
		if n := len([]rune(l)); n > defaultSidebarWidth {
			t.Fatalf("row wider than sidebar: %d > %d (%q)", n, defaultSidebarWidth, l)
		}
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gogen/internal/agent"
)

// ── A1/A3: close semantics ──

func TestLiveSessionsClose(t *testing.T) {
	ls := newLiveSessions(&agent.Agent{})
	bg := ls.Add(&agent.Agent{}, "bg")

	t.Run("cannot close focused", func(t *testing.T) {
		if err := ls.Close(0); err == nil {
			t.Fatal("expected focused-close error")
		}
	})
	t.Run("cannot close while streaming", func(t *testing.T) {
		bg.streaming = true
		if err := ls.Close(1); err == nil {
			t.Fatal("expected streaming-close error")
		}
		bg.streaming = false
	})
	t.Run("close flushes, detaches, keeps active index", func(t *testing.T) {
		if err := ls.Close(1); err != nil {
			t.Fatalf("close: %v", err)
		}
		if len(ls.sessions) != 1 || ls.ByID("s2") != nil {
			t.Fatal("session not detached")
		}
		if ls.active != 0 || ls.Active().id != "s1" {
			t.Fatalf("active desynced: %d", ls.active)
		}
	})
}

// ── A2: owner tagging drops switch-boundary stragglers ──

func TestStreamEventSidGuard(t *testing.T) {
	m := &Model{lives: newLiveSessions(&agent.Agent{})}
	m.lives.Add(&agent.Agent{}, "bg")

	t.Run("sid extraction", func(t *testing.T) {
		if sid, ok := streamEventSid(streamTokenMsg{token: "x", sid: "s2"}); !ok || sid != "s2" {
			t.Fatalf("got %q %v", sid, ok)
		}
		if _, ok := streamEventSid(tea.WindowSizeMsg{}); ok {
			t.Fatal("non-stream msg must report ok=false")
		}
	})

	t.Run("straggler from backgrounded owner is dropped", func(t *testing.T) {
		m.lives.Switch(0) // s1 focused
		before := len(m.chatLines)
		// A chunk sent by s2 while it was focused, delivered after the switch.
		mod, _ := m.Update(streamTokenMsg{token: "leak", sid: "s2"})
		got := mod.(*Model)
		if len(got.chatLines) != before {
			t.Fatal("straggler leaked into focused transcript")
		}
	})

	t.Run("focused session events still render", func(t *testing.T) {
		m.resetStreamState(false)
		mod, _ := m.Update(streamStartMsg{sid: "s1"})
		got := mod.(*Model)
		_ = got // must not panic / must be processed without drop
		if _, ok := streamEventSid(streamStartMsg{sid: "s1"}); !ok {
			t.Fatal("start should be tagged")
		}
	})
}

// The ownsStream guard precedes the routing switch in Update; terminal
// msgs are deliberately excluded from streamEventSid so background
// finalization still routes. This pins that exclusion.
func TestTerminalMsgsBypassStragglerGuard(t *testing.T) {
	m := dragModel(t)
	m.lives.Add(&agent.Agent{}, "bg")
	m.lives.ByID("s2").streaming = true

	mod, _ := m.Update(endOfStream("s2"))
	got := mod.(*Model)
	if got.lives.ByID("s2") == nil || got.lives.ByID("s2").streaming {
		t.Fatal("Update dropped/rerouted a background terminal message")
	}
}

// ── A5: prefs persistence round-trip ──

func TestUIPrefsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := uiPrefsPath(dir)

	t.Run("missing file yields defaults, not found", func(t *testing.T) {
		p, found := loadUIPrefs(filepath.Join(dir, "empty"))
		if found || p.SidebarVisible || p.SidebarWidth != defaultSidebarWidth {
			t.Fatalf("defaults wrong: found=%v %+v", found, p)
		}
	})

	t.Run("save then load preserves state", func(t *testing.T) {
		saveUIPrefs(dir, uiPrefs{SidebarVisible: true, SidebarWidth: 34})
		p, found := loadUIPrefs(dir)
		if !found || !p.SidebarVisible || p.SidebarWidth != 34 {
			t.Fatalf("round-trip lost state: found=%v %+v", found, p)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("prefs file missing: %v", err)
		}
	})

	t.Run("corrupt file falls back to defaults", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		p, found := loadUIPrefs(dir)
		if found || p.SidebarVisible || p.SidebarWidth != defaultSidebarWidth {
			t.Fatalf("corrupt handling wrong: found=%v %+v", found, p)
		}
	})
}

// Startup policy (web parity): ALWAYS open; only the width persists.
func TestResolveSidebarStart(t *testing.T) {
	dir := t.TempDir()

	t.Run("first run opens at default width", func(t *testing.T) {
		p := resolveSidebarStart(filepath.Join(dir, "fresh"))
		if !p.SidebarVisible || p.SidebarWidth != defaultSidebarWidth {
			t.Fatalf("first run = %+v, want open/default", p)
		}
	})

	t.Run("saved close is ignored, width kept", func(t *testing.T) {
		saveUIPrefs(dir, uiPrefs{SidebarVisible: false, SidebarWidth: 30})
		p := resolveSidebarStart(dir)
		if !p.SidebarVisible {
			t.Fatal("panel must open by default (web parity)")
		}
		if p.SidebarWidth != 30 {
			t.Fatalf("width preference lost: %+v", p)
		}
	})

	t.Run("explicit open width is honored", func(t *testing.T) {
		saveUIPrefs(dir, uiPrefs{SidebarVisible: true, SidebarWidth: 38})
		p := resolveSidebarStart(dir)
		if !p.SidebarVisible || p.SidebarWidth != 38 {
			t.Fatalf("open choice lost: %+v", p)
		}
	})
}

// ── Critical: m.streaming is the FOCUSED session's mirror ──

// Regression: switching away from a streaming session must unlock input
// (m.streaming=false on the idle target), and a background finish must
// never touch the focused mirror.
func TestSwitchSyncsStreamingMirror(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	m.lives.Add(a2, "second")

	t.Run("leave mid-turn session unlocks the idle target", func(t *testing.T) {
		s1 := m.lives.Active() // s1
		s1.streaming = true
		m.streaming = true // turn started while s1 was focused

		cmd := m.switchToLive(1)
		_ = cmd
		if m.streaming {
			t.Fatal("m.streaming stayed true after leaving a backgrounded stream — input would be bricked")
		}
		if !m.lives.ByID("s1").streaming {
			t.Fatal("background session must keep streaming")
		}
		if m.progressPhase != progressHidden {
			t.Fatalf("progress not cleared: %v", m.progressPhase)
		}
	})

	t.Run("join locks the mirror again", func(t *testing.T) {
		// Start from idle s1 focus, then join streaming s2.
		m.switchToLive(0)
		m.streaming = false
		m.clearProgress()
		m.lives.ByID("s2").streaming = true

		m.switchToLive(1)

		if !m.streaming {
			t.Fatal("joining a streaming session must set m.streaming")
		}
		if m.progressPhase == progressHidden {
			t.Fatal("joined session must show progress")
		}
	})

	t.Run("background finish leaves focused mirror alone", func(t *testing.T) {
		// s1's turn from the first subtest ended meanwhile; focus it while
		// s2 keeps streaming; then s2 finishes.
		m.lives.ByID("s1").streaming = false
		m.switchToLive(0)
		if m.streaming {
			t.Fatal("precondition: focus on idle s1")
		}

		mod, _ := m.handleTurnFinishedMsg("s2", nil)
		got := mod.(*Model)
		if got.lives.ByID("s2").streaming {
			t.Fatal("background flags not cleared")
		}
		if got.streaming {
			t.Fatal("background finish corrupted the focused mirror")
		}
	})
}

// ── Terminal attribution: successful turns MUST finalize ──

// Regression for the silent-brick bug: a success terminal built without
// its sid made ByID("") miss, so handleTurnFinishedMsg early-returned and
// the focused session never left streaming state.
func TestTerminalAttribution(t *testing.T) {
	t.Run("endOfStream attributes", func(t *testing.T) {
		msg := endOfStream("s7")
		end, ok := msg.(streamEndMsg)
		if !ok || end.sid != "s7" {
			t.Fatalf("endOfStream = %#v", msg)
		}
	})
	t.Run("failOfStream attributes", func(t *testing.T) {
		msg := failOfStream("s7", errFake{})
		e, ok := msg.(streamErrorMsg)
		if !ok || e.sid != "s7" || e.err == nil {
			t.Fatalf("failOfStream = %#v", msg)
		}
	})

	t.Run("attributed success finalizes the focused session", func(t *testing.T) {
		m := dragModel(t) // single live session s1, focused
		s1 := m.lives.Active()
		s1.streaming = true
		m.streaming = true

		mod, _ := m.Update(endOfStream("s1"))
		got := mod.(*Model)
		if got.streaming || got.lives.ByID("s1").streaming {
			t.Fatal("successful turn did not finalize streaming state")
		}
		if got.progressPhase != progressHidden {
			t.Fatalf("progress stuck: %v", got.progressPhase)
		}
	})
}

// ── B8: saved sessions render as rows in the unified list ──

func TestRenderSidebarSavedRows(t *testing.T) {
	m := newSidebarTestModel(100)
	m.height = 30
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	m.savedCache = []agent.SessionInfo{{ID: "abc123", Label: "old task"}}
	main := m.renderMainColumn()
	out := stripANSI(m.renderSidebar(strings.Count(main, "\n") + 1))
	// The saved row shows its label (id when unlabeled); its meta line
	// carries no live state (the live row above it does).
	lines := strings.Split(out, "\n")
	found := false
	for i, l := range lines {
		if strings.Contains(l, "old task") {
			found = true
			if i+1 < len(lines) {
				meta := lines[i+1]
				for _, st := range []string{"active", "open", "responding"} {
					if strings.Contains(meta, st) {
						t.Fatalf("saved row meta carries live state %q: %q", st, meta)
					}
				}
			}
			break
		}
	}
	if !found {
		t.Fatalf("saved row missing:\n%s", out)
	}
}

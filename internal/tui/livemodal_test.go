package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"gogen/internal/agent"
)

func TestLiveSessionsModalNavigation(t *testing.T) {
	m := &Model{lives: newLiveSessions(&agent.Agent{})}
	m.lives.Add(&agent.Agent{}, "second")

	t.Run("cursor moves within bounds", func(t *testing.T) {
		for _, key := range []string{"j", "j", "down"} {
			m.handleLiveSessionsKey(keyMsg(key))
		}
		if m.liveCursor != 1 {
			t.Fatalf("liveCursor = %d, want 1", m.liveCursor)
		}
		m.handleLiveSessionsKey(keyMsg("up"))
		if m.liveCursor != 0 {
			t.Fatalf("liveCursor = %d, want 0", m.liveCursor)
		}
	})

	t.Run("enter on focused row just closes", func(t *testing.T) {
		m.liveCursor = 0
		mod, _ := m.handleLiveSessionsKey(keyMsg("enter"))
		got := mod.(*Model)
		if got.modal != ModalNone || got.agent != nil && got.agent != m.lives.ByID("s1").agent {
			t.Fatalf("unexpected state after enter: modal=%v", got.modal)
		}
	})

	t.Run("esc closes", func(t *testing.T) {
		m.modal = ModalLiveSessions
		mod, _ := m.handleLiveSessionsKey(keyMsg("esc"))
		if mod.(*Model).modal != ModalNone {
			t.Fatal("esc must close the modal")
		}
	})
}

// "c" cancels the SELECTED session's turn (the focused-only
// cancelActiveStream can't reach a background one). Flags stay set until
// the attributed terminal message lands — that is the contract, pinned here.
func TestLiveSessionsModalCancelKey(t *testing.T) {
	m := &Model{lives: newLiveSessions(&agent.Agent{})}
	bg := m.lives.Add(&agent.Agent{}, "bg")
	bg.streaming = true
	called := false
	bg.cancel = func() { called = true }

	t.Run("cancel fires the selected session's cancel func", func(t *testing.T) {
		m.liveCursor = 1
		m.handleLiveSessionsKey(keyMsg("c"))
		if !called {
			t.Fatal("cancel key did not call the session's cancel func")
		}
		if !bg.streaming {
			t.Fatal("streaming flag must stay set until the terminal message lands")
		}
		if m.statusMsg == "" {
			t.Fatal("expected a status message")
		}
	})

	t.Run("idle selection reports idle, gains no cancel func", func(t *testing.T) {
		idle := m.lives.Add(&agent.Agent{}, "idle")
		m.liveCursor = 2
		m.handleLiveSessionsKey(keyMsg("c"))
		if idle.cancel != nil || idle.streaming {
			t.Fatal("idle session must be untouched")
		}
		if m.statusMsg == "" {
			t.Fatal("expected an idle status message")
		}
	})
}

func keyMsg(s string) tea.KeyPressMsg {
	switch s {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	default:
		return tea.KeyPressMsg{Code: rune(s[0])}
	}
}

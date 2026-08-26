package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
)

// The completion bell (web parity: desktop notifications for turn end,
// turn error, and approval requests) rings ONLY while the terminal window
// is blurred — never while the user is looking at it.
func TestCompletionBellOnlyWhenBlurred(t *testing.T) {
	t.Run("focused window stays quiet", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.terminalBlurred = false
		m.handleStreamEndMsg()
		if m.bellsRung != 0 {
			t.Fatalf("bells = %d, want 0 while focused", m.bellsRung)
		}
	})

	t.Run("blurred window bells on turn end", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.terminalBlurred = true
		m.handleStreamEndMsg()
		if m.bellsRung != 1 {
			t.Fatalf("bells = %d, want 1", m.bellsRung)
		}
	})

	t.Run("blur/focus messages toggle the flag", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.Update(tea.BlurMsg{})
		if !m.terminalBlurred {
			t.Fatal("BlurMsg must set terminalBlurred")
		}
		m.Update(tea.FocusMsg{})
		if m.terminalBlurred {
			t.Fatal("FocusMsg must clear terminalBlurred")
		}
	})

	t.Run("turn error bells, user cancel does not", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.terminalBlurred = true
		// Production cancel flow: the key handler clears m.streaming
		// BEFORE the stream's context-canceled error arrives.
		m.streaming = false
		m.handleStreamError(context.Canceled)
		if m.bellsRung != 0 {
			t.Fatalf("cancel must not bell: %d", m.bellsRung)
		}
		m.handleStreamError(context.DeadlineExceeded)
		if m.bellsRung != 1 {
			t.Fatalf("bells = %d, want 1 after a real error", m.bellsRung)
		}
	})

	t.Run("background session finish bells", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.lives.Add(&agent.Agent{}, "bg")
		m.terminalBlurred = true
		// "s2" is the background session; the focused one stays active.
		m.handleTurnFinishedMsg("s2", 0, nil)
		if m.bellsRung != 1 {
			t.Fatalf("bells = %d, want 1 for a background finish", m.bellsRung)
		}
	})

	t.Run("approval request bells when blurred", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.terminalBlurred = true
		m.handleApprovalRequestMsg(approvalRequestMsg{
			ar: &approvalRequest{
				req:   agent.DeleteRequest{Paths: []string{"f.txt"}, Reason: "test"},
				reply: make(chan bool, 1),
			},
		})
		if m.modal != ModalApproval {
			t.Fatalf("modal = %v, want ModalApproval", m.modal)
		}
		if m.bellsRung != 1 {
			t.Fatalf("bells = %d, want 1", m.bellsRung)
		}
	})

	t.Run("approval request is quiet while focused", func(t *testing.T) {
		m := newSidebarFullModel(t)
		m.terminalBlurred = false
		m.handleApprovalRequestMsg(approvalRequestMsg{
			ar: &approvalRequest{
				req:   agent.DeleteRequest{Paths: []string{"f.txt"}, Reason: "test"},
				reply: make(chan bool, 1),
			},
		})
		if m.bellsRung != 0 {
			t.Fatalf("bells = %d, want 0 while focused", m.bellsRung)
		}
	})
}

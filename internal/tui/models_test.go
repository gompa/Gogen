package tui

import (
	"errors"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// /models and the models-modal selection run off the Update thread
// (provider round trips); these tests pin the async result handling
// without a live endpoint.
func TestModelListMsg(t *testing.T) {
	t.Run("success opens the modal", func(t *testing.T) {
		m := dragModel(t)
		mod, _ := m.Update(modelListMsg{agent: m.agent, list: []llm.ModelInfo{{ID: "m1"}, {ID: "m2"}}})
		got := mod.(*Model)
		if got.modal != ModalModels || len(got.modelList) != 2 || got.modelCursor != 0 {
			t.Fatalf("modal=%v list=%d cursor=%d", got.modal, len(got.modelList), got.modelCursor)
		}
	})

	t.Run("error appends a line and keeps the modal closed", func(t *testing.T) {
		m := dragModel(t)
		mod, _ := m.Update(modelListMsg{agent: m.agent, err: errors.New("no endpoint")})
		got := mod.(*Model)
		if got.modal != ModalNone {
			t.Fatal("modal must stay closed on error")
		}
		if !strings.Contains(strings.Join(got.chatLines, "\n"), "Models: no endpoint") {
			t.Fatalf("error line missing: %s", strings.Join(got.chatLines, "\n"))
		}
	})

	t.Run("stale list from another session is dropped", func(t *testing.T) {
		m := dragModel(t)
		m.statusMsg = "Loading models…"
		other := newSwitchTestAgent(t)
		mod, _ := m.Update(modelListMsg{agent: other, list: []llm.ModelInfo{{ID: "m1"}}})
		got := mod.(*Model)
		if got.modal != ModalNone || len(got.modelList) != 0 {
			t.Fatal("list from a non-focused session must be dropped")
		}
		if got.statusMsg == "" || strings.Contains(got.statusMsg, "Loading") {
			t.Fatalf("drop must surface an explanation instead of the loading hint: %q", got.statusMsg)
		}
	})

	t.Run("no-arg /models starts an async list", func(t *testing.T) {
		m := dragModel(t)
		handled, _, cmd := m.dispatchCommand("/models")
		if !handled || cmd == nil {
			t.Fatalf("handled=%v cmd=%v, want async list start", handled, cmd != nil)
		}
	})
}

func TestModelSwitchMsg(t *testing.T) {
	t.Run("success appends the switch line", func(t *testing.T) {
		m := dragModel(t)
		mod, _ := m.Update(modelSwitchMsg{agent: m.agent, out: "Switched to model: m1"})
		got := mod.(*Model)
		if !strings.Contains(strings.Join(got.chatLines, "\n"), "Switched to model: m1") {
			t.Fatalf("switch line missing: %s", strings.Join(got.chatLines, "\n"))
		}
	})

	t.Run("error appends an error line", func(t *testing.T) {
		m := dragModel(t)
		mod, _ := m.Update(modelSwitchMsg{agent: m.agent, err: errors.New("unknown model")})
		got := mod.(*Model)
		if !strings.Contains(strings.Join(got.chatLines, "\n"), "Models: unknown model") {
			t.Fatalf("error line missing: %s", strings.Join(got.chatLines, "\n"))
		}
	})

	t.Run("switch for a non-focused session is dropped", func(t *testing.T) {
		m := dragModel(t)
		other := newSwitchTestAgent(t)
		mod, _ := m.Update(modelSwitchMsg{agent: other, out: "Switched to model: m1"})
		got := mod.(*Model)
		if strings.Contains(strings.Join(got.chatLines, "\n"), "Switched to model") {
			t.Fatal("switch line leaked into the focused transcript")
		}
		if got.statusMsg != "Switched to model: m1" {
			t.Fatalf("drop must surface the outcome on the status line: %q", got.statusMsg)
		}

		mod, _ = m.Update(modelSwitchMsg{agent: other, err: errors.New("unknown model")})
		got = mod.(*Model)
		if got.statusMsg != "✗ Models: unknown model" {
			t.Fatalf("drop of a failed switch must surface the error: %q", got.statusMsg)
		}
	})
}

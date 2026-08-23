package tui

import (
	"strings"
	"testing"

	"gogen/internal/agent"
)

func TestLiveSessionsRegistry(t *testing.T) {
	root := &agent.Agent{}
	ls := newLiveSessions(root)

	t.Run("startup session is active and focused", func(t *testing.T) {
		if got := ls.Active(); got == nil || got.id != "s1" || !got.focused.Load() {
			t.Fatalf("active = %+v, want focused s1", got)
		}
	})

	t.Run("add registers without switching focus", func(t *testing.T) {
		s2 := ls.Add(&agent.Agent{}, "refactor")
		if s2.id != "s2" || ls.Active() != ls.ByID("s1") {
			t.Fatalf("add switched focus or bad id: %+v", s2)
		}
		if s2.focused.Load() {
			t.Fatal("background session must not be focused")
		}
	})

	t.Run("switch flips focus gates both ways", func(t *testing.T) {
		ls.Switch(1)
		if ls.Active().id != "s2" || !ls.ByID("s2").focused.Load() || ls.ByID("s1").focused.Load() {
			t.Fatalf("focus gates wrong after switch: s1=%v s2=%v",
				ls.ByID("s1").focused.Load(), ls.ByID("s2").focused.Load())
		}
		ls.Switch(0)
		if ls.Active().id != "s1" || !ls.ByID("s1").focused.Load() {
			t.Fatal("switch back failed")
		}
	})

	t.Run("switch ignores out-of-range and no-op", func(t *testing.T) {
		ls.Switch(9)
		ls.Switch(0)
		if ls.Active().id != "s1" {
			t.Fatalf("active changed on invalid switch: %s", ls.Active().id)
		}
	})

	t.Run("by id resolves and misses cleanly", func(t *testing.T) {
		if ls.ByID("s2") == nil {
			t.Fatal("s2 should resolve")
		}
		if ls.ByID("nope") != nil {
			t.Fatal("unknown id must return nil")
		}
	})
}

// Background finalization must never touch transcript/viewport state — it
// only clears runtime flags and reports via the status line.
func TestHandleTurnFinishedBackgroundSession(t *testing.T) {
	newModel := func() *Model {
		ls := newLiveSessions(&agent.Agent{})
		ls.Add(&agent.Agent{}, "bg")
		return &Model{lives: ls}
	}

	tests := []struct {
		name    string
		sid     string
		err     error
		wantSub string
	}{
		{name: "background success reports finished", sid: "s2", wantSub: "✓ bg"},
		{name: "background failure reports error", sid: "s2", err: errFake{}, wantSub: "✗ bg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel()
			m.lives.ByID("s2").streaming = true
			model, _ := m.handleTurnFinishedMsg(tt.sid, tt.err)
			got := model.(*Model)
			if got.lives.ByID("s2").streaming {
				t.Fatal("background session still marked streaming")
			}
			if got.lives.ByID("s2").cancel != nil {
				t.Fatal("cancel not cleared")
			}
			if !strings.Contains(got.statusMsg, tt.wantSub) {
				t.Fatalf("statusMsg = %q, want substring %q", got.statusMsg, tt.wantSub)
			}
			if len(got.chatLines) != 0 {
				t.Fatal("background finalize must not mutate transcript")
			}
		})
	}

	t.Run("unknown session id is a no-op", func(t *testing.T) {
		m := newModel()
		model, _ := m.handleTurnFinishedMsg("ghost", nil)
		if model.(*Model).statusMsg != "" {
			t.Fatalf("unexpected status %q", model.(*Model).statusMsg)
		}
	})
}

type errFake struct{}

func (errFake) Error() string { return "boom" }

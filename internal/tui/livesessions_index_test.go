package tui

import (
	"testing"

	"gogen/internal/agent"
)

func TestLiveSessionsByIndexAndGates(t *testing.T) {
	ls := newLiveSessions(&agent.Agent{})
	s2 := ls.Add(&agent.Agent{}, "second")

	if ls.ByIndex(1) != s2 || ls.ByIndex(-1) != nil || ls.ByIndex(2) != nil {
		t.Fatal("ByIndex range handling wrong")
	}

	ls.Switch(1)
	if !s2.focused.Load() || ls.ByIndex(0).focused.Load() {
		t.Fatal("focus gates not flipped by Switch")
	}
	ls.Switch(0)
	if ls.Active() != ls.ByIndex(0) {
		t.Fatal("switch back failed")
	}
}

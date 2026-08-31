package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
)

// newJobNoticeTestAgent builds an agent with a real executor so
// StartBackgroundCommand can run (the notice fires from the job monitor
// goroutine when the command exits naturally).
func newJobNoticeTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	a := agent.NewAgent(nil, agent.NewExecutor(t.TempDir()), nil)
	t.Cleanup(a.Close)
	return a
}

// TestJobNoticeHookFor pins the focus-aware contract: a focused session
// gets the normal delivery turn; a backgrounded one buffers an attributed
// condensed note (no delivery — a global delivery turn would run on
// whichever session is focused, not this one).
func TestJobNoticeHookFor(t *testing.T) {
	ls := newLiveSessions(&agent.Agent{})
	bg := ls.Add(&agent.Agent{}, "bg")
	ls.Switch(1) // "bg" focused, root backgrounded

	t.Run("focused session delivers", func(t *testing.T) {
		var got string
		hook := jobNoticeHookFor(bg, func(summary string) { got = summary })
		hook("job done")
		if got != "job done" {
			t.Fatalf("deliver = %q, want %q", got, "job done")
		}
		if n := len(bg.pending); n != 0 {
			t.Fatalf("pending = %d, want 0", n)
		}
	})

	t.Run("backgrounded session buffers a note", func(t *testing.T) {
		root := ls.sessions[0]
		delivered := false
		hook := jobNoticeHookFor(root, func(string) { delivered = true })
		hook("[job] job-123 (echo hi) exited with code 0")
		if delivered {
			t.Fatal("deliver must not be called for a backgrounded session")
		}
		out := root.popAll()
		if len(out) != 1 {
			t.Fatalf("pending = %d, want 1", len(out))
		}
		note, ok := out[0].(condensedNoteMsg)
		if !ok {
			t.Fatalf("msg = %T, want condensedNoteMsg", out[0])
		}
		if note.sid != root.id || !strings.Contains(note.note, "job-123") {
			t.Fatalf("note = %+v, want sid %q carrying the summary", note, root.id)
		}
	})
}

// TestInstallJobNoticeHook pins the TUI wiring end-to-end with a real
// background job: an enabled feature installs a focus-aware hook on the
// root agent (focused → deliver, backgrounded → buffered note), and a
// disabled feature installs nothing.
func TestInstallJobNoticeHook(t *testing.T) {
	t.Run("focused root delivers", func(t *testing.T) {
		a := newJobNoticeTestAgent(t)
		tui := &TUI{agent: a, cfg: &config.Config{JobNotices: "on"}}
		m := &Model{lives: newLiveSessions(a)}

		got := make(chan string, 1)
		tui.installJobNoticeHook(m, func(summary string) { got <- summary })
		if _, err := a.StartBackgroundCommand(context.Background(), "echo tui-notice"); err != nil {
			t.Fatal(err)
		}
		select {
		case s := <-got:
			if !strings.Contains(s, "exited with code 0") {
				t.Fatalf("summary = %q", s)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("delivery hook was not fired")
		}
	})

	t.Run("backgrounded root buffers a note", func(t *testing.T) {
		a := newJobNoticeTestAgent(t)
		tui := &TUI{agent: a, cfg: &config.Config{JobNotices: "on"}}
		m := &Model{lives: newLiveSessions(a)}
		m.lives.Add(&agent.Agent{}, "bg")
		// Install while the root is active (as Run does): the hook gates on
		// the slot that is active at install time — the root's.
		tui.installJobNoticeHook(m, func(string) {
			t.Error("deliver must not run for a backgrounded root")
		})
		m.lives.Switch(1) // root backgrounded
		if _, err := a.StartBackgroundCommand(context.Background(), "echo tui-bg-notice"); err != nil {
			t.Fatal(err)
		}
		root := m.lives.sessions[0]
		deadline := time.Now().Add(10 * time.Second)
		for {
			root.mu.Lock()
			n := len(root.pending)
			root.mu.Unlock()
			if n > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		out := root.popAll()
		if len(out) != 1 {
			t.Fatalf("pending = %d, want 1", len(out))
		}
		note, ok := out[0].(condensedNoteMsg)
		if !ok || note.sid != "s1" {
			t.Fatalf("msg = %#v, want condensedNoteMsg with sid s1", out[0])
		}
	})

	t.Run("feature off installs nothing", func(t *testing.T) {
		a := newJobNoticeTestAgent(t)
		tui := &TUI{agent: a, cfg: &config.Config{}}
		m := &Model{lives: newLiveSessions(a)}

		tui.installJobNoticeHook(m, func(string) {
			t.Error("deliver must not run when job_notices is off")
		})
		if _, err := a.StartBackgroundCommand(context.Background(), "echo tui-off"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		if n := len(m.lives.sessions[0].pending); n != 0 {
			t.Fatalf("pending = %d, want 0", n)
		}
	})
}

// TestSwitchToLiveIdleSurfacesNotes pins the idle-switch buffer policy:
// display-only notes buffered while the session was in the background are
// surfaced on focus, while stale stream events (already reflected in
// Messages by the transcript rebuild) are dropped — replaying them would
// duplicate the finished turn.
func TestSwitchToLiveIdleSurfacesNotes(t *testing.T) {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle

	a1 := newSwitchTestAgent(t)
	a2 := newSwitchTestAgent(t)
	m := &Model{agent: a1, lives: newLiveSessions(a1), viewport: vp, sessionID: a1.SessionID}
	bg := m.lives.Add(a2, "second")

	// The background session finished a turn while unfocused: its buffer
	// holds the (now stale) stream events plus a job-notice note.
	bg.enqueue(streamTokenMsg{token: "stale-token", sid: bg.id})
	bg.enqueue(condensedNoteMsg{
		note: noticeLabel + " job finished: [job] job-1 (echo hi) exited with code 0",
		sid:  bg.id,
	})

	m.switchToLive(1)

	if n := len(bg.pending); n != 0 {
		t.Fatalf("pending = %d, want drained", n)
	}
	joined := strings.Join(m.chatLines, "\n")
	if !strings.Contains(joined, "job finished") {
		t.Fatalf("note not surfaced: %q", joined)
	}
	if strings.Contains(joined, "stale-token") {
		t.Fatalf("stale stream event was replayed: %q", joined)
	}
}

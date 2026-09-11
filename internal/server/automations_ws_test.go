package server

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/automation"
)

// frameReader wraps a WS connection with a deadline-free reader goroutine
// feeding a channel. The automations tests use it instead of readUntil's
// per-read SetReadDeadline pattern: gorilla requires closing a connection
// once a read deadline fires, and the deadline'd client can wedge on this
// server's interleaved broadcast frames — real browsers never arm read
// deadlines, so the channel reader matches production semantics.
type frameReader struct {
	frames chan WSMessage
	conn   *websocket.Conn
}

func newFrameReader(t *testing.T, conn *websocket.Conn) *frameReader {
	t.Helper()
	fr := &frameReader{frames: make(chan WSMessage, 128), conn: conn}
	go func() {
		for {
			var m WSMessage
			if err := conn.ReadJSON(&m); err != nil {
				close(fr.frames)
				return
			}
			select {
			case fr.frames <- m:
			default:
			}
		}
	}()
	return fr
}

// until drains frames until one matches (dropping the others — e.g.
// user-term and unrelated config pushes), failing the test on timeout or
// connection close.
func (fr *frameReader) until(t *testing.T, timeout time.Duration, what string, match func(WSMessage) bool) WSMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-fr.frames:
			if !ok {
				t.Fatalf("connection closed while waiting for %s", what)
			}
			if match(m) {
				return m
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// newAutomationsTest wires a continuation server with the automations store
// in a temp XDG dir, a stub fire, a fast sweep, and a frame-reading client
// that already toggled the feature flag on through the settings channel.
func newAutomationsTest(t *testing.T) (*Server, *frameReader) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	storeDir := automation.DefaultDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, t.TempDir())
	s.setAutomationFire(nil, storeDir)
	s.setAutomationInterval(10 * time.Millisecond)
	srv := startWSServer(t, s)
	t.Cleanup(srv.Close)

	conn := dialWS(t, srv, "/ws")
	fr := newFrameReader(t, conn)

	// Handshake: session_state + config (+ config echo).
	fr.until(t, 5*time.Second, "session_state", func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := fr.until(t, 5*time.Second, "config", func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	fr.until(t, 5*time.Second, "config echo", func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid
	})

	// Toggle the feature on through the settings channel (the same path
	// the settings modal uses) and wait for the push to reflect it.
	if err := conn.WriteJSON(WSMessage{Type: "config", Automations: "on", SessionID: sid}); err != nil {
		t.Fatalf("send automations on: %v", err)
	}
	fr.until(t, 5*time.Second, "automations on push", func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid && m.Automations == "on"
	})
	return s, fr
}

// TestAutomationsOpsViaWS drives the automations-tab WS contract end to
// end: list → create → runs → disable → enable → delete, the state
// broadcasts after each mutation, and the validation/unknown-op error
// notices.
func TestAutomationsOpsViaWS(t *testing.T) {
	_, fr := newAutomationsTest(t)

	// list → snapshot (empty).
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{Action: "list"}}); err != nil {
		t.Fatalf("send list: %v", err)
	}
	snap := fr.until(t, 5*time.Second, "list snapshot", func(m WSMessage) bool { return m.Type == "automations_state" })
	if snap.AutomationsState == nil || !snap.AutomationsState.StoreOK {
		t.Fatalf("initial snapshot = %+v, want StoreOK", snap.AutomationsState)
	}
	if !snap.AutomationsState.Enabled || len(snap.AutomationsState.Automations) != 0 {
		t.Fatalf("initial snapshot = enabled %v, %d rows; want enabled, 0 rows",
			snap.AutomationsState.Enabled, len(snap.AutomationsState.Automations))
	}

	// create (daily 09:00, empty dir = the workspace default).
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "create",
		Title:  "Nightly triage",
		Prompt: "triage the board",
		Kind:   "daily",
		Time:   "09:00",
	}}); err != nil {
		t.Fatalf("send create: %v", err)
	}
	// The broadcast is written BEFORE the notice on the same connection, so
	// wait for the state first, then the toast (the until() helper drops
	// non-matching frames — reading them out of order would lose one).
	created := fr.until(t, 5*time.Second, "created state", func(m WSMessage) bool {
		return m.Type == "automations_state" && m.AutomationsState != nil && len(m.AutomationsState.Automations) == 1
	}).AutomationsState
	notice := fr.until(t, 5*time.Second, "create notice", func(m WSMessage) bool {
		return m.Type == "notice" && m.Kind == "automations"
	})
	if !notice.Success {
		t.Fatalf("create notice = %+v", notice)
	}
	a := created.Automations[0]
	if a.Title != "Nightly triage" || a.Schedule.Kind != "daily" || a.Schedule.Time != "09:00" || !a.Enabled {
		t.Fatalf("created row drifted: %+v", a)
	}
	if a.NextRunAt == nil {
		t.Fatal("created row has no next_run_at")
	}
	if a.WorkingDir == "" {
		t.Fatal("create did not default the working dir to the workspace")
	}

	// runs → history payload (empty, but the row must exist).
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "runs", ID: a.ID,
	}}); err != nil {
		t.Fatalf("send runs: %v", err)
	}
	runsMsg := fr.until(t, 5*time.Second, "runs payload", func(m WSMessage) bool {
		return m.Type == "automations_runs"
	})
	if runsMsg.AutomationsRuns == nil || runsMsg.AutomationsRuns.AutomationID != a.ID || len(runsMsg.AutomationsRuns.Runs) != 0 {
		t.Fatalf("runs payload = %+v", runsMsg.AutomationsRuns)
	}

	// disable → paused in the next broadcast (state first, then notice).
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "disable", ID: a.ID,
	}}); err != nil {
		t.Fatalf("send disable: %v", err)
	}
	fr.until(t, 5*time.Second, "paused state", func(m WSMessage) bool {
		return m.Type == "automations_state" && m.AutomationsState != nil &&
			len(m.AutomationsState.Automations) == 1 && !m.AutomationsState.Automations[0].Enabled
	})
	fr.until(t, 5*time.Second, "disable notice", func(m WSMessage) bool {
		return m.Type == "notice" && m.Kind == "automations" && m.Success
	})

	// enable again → active with a recomputed next run.
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "enable", ID: a.ID,
	}}); err != nil {
		t.Fatalf("send enable: %v", err)
	}
	fr.until(t, 5*time.Second, "enabled state", func(m WSMessage) bool {
		return m.Type == "automations_state" && m.AutomationsState != nil &&
			len(m.AutomationsState.Automations) == 1 && m.AutomationsState.Automations[0].Enabled &&
			m.AutomationsState.Automations[0].NextRunAt != nil
	})
	fr.until(t, 5*time.Second, "enable notice", func(m WSMessage) bool {
		return m.Type == "notice" && m.Kind == "automations" && m.Success
	})

	// delete → the row is gone in the broadcast.
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "delete", ID: a.ID,
	}}); err != nil {
		t.Fatalf("send delete: %v", err)
	}
	fr.until(t, 5*time.Second, "deleted state", func(m WSMessage) bool {
		return m.Type == "automations_state" && m.AutomationsState != nil && len(m.AutomationsState.Automations) == 0
	})
	fr.until(t, 5*time.Second, "delete notice", func(m WSMessage) bool {
		return m.Type == "notice" && m.Kind == "automations" && m.Success
	})

	// Unknown op → error notice.
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "explode",
	}}); err != nil {
		t.Fatalf("send unknown op: %v", err)
	}
	errNotice := fr.until(t, 5*time.Second, "unknown-op error", func(m WSMessage) bool {
		return m.Type == "notice" && m.Kind == "automations" && !m.Success
	})
	if errNotice.Content == "" {
		t.Fatal("unknown op produced an empty error notice")
	}

	// Invalid create (missing prompt) → error notice, nothing stored.
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "create", Title: "no prompt", Kind: "daily", Time: "09:00",
	}}); err != nil {
		t.Fatalf("send invalid create: %v", err)
	}
	fr.until(t, 5*time.Second, "invalid-create error", func(m WSMessage) bool {
		return m.Type == "notice" && m.Kind == "automations" && !m.Success
	})
}

// TestAutomationsUpdateOpViaWS covers the tab's edit flow: the update op
// applies a full-record PATCH (the client sends the complete schedule), a
// schedule change re-times the next run, and a prompt-only change keeps it.
func TestAutomationsUpdateOpViaWS(t *testing.T) {
	_, fr := newAutomationsTest(t)

	// Seed a daily job through the tab.
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "create", Title: "Nightly", Prompt: "p", Kind: "daily", Time: "09:00", Timezone: "UTC",
	}}); err != nil {
		t.Fatalf("send create: %v", err)
	}
	created := fr.until(t, 5*time.Second, "created state", func(m WSMessage) bool {
		return m.Type == "automations_state" && m.AutomationsState != nil && len(m.AutomationsState.Automations) == 1
	}).AutomationsState
	a := created.Automations[0]
	if a.NextRunAt == nil {
		t.Fatal("created row has no next_run_at")
	}
	originalNext := *a.NextRunAt

	// Prompt-only edit: the next run must NOT move.
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "update", ID: a.ID, Title: a.Title, Prompt: "edited prompt",
		Kind: a.Schedule.Kind, Time: a.Schedule.Time, Timezone: a.Schedule.Timezone,
	}}); err != nil {
		t.Fatalf("send prompt-only update: %v", err)
	}
	fr.until(t, 5*time.Second, "prompt-edit state", func(m WSMessage) bool {
		if m.Type != "automations_state" || m.AutomationsState == nil || len(m.AutomationsState.Automations) != 1 {
			return false
		}
		got := m.AutomationsState.Automations[0]
		return got.Prompt == "edited prompt" && got.NextRunAt != nil && got.NextRunAt.Equal(originalNext)
	})

	// Schedule edit: re-timed to the new wall-clock time.
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "update", ID: a.ID, Title: a.Title, Prompt: "edited prompt",
		Kind: "daily", Time: "18:30", Timezone: "UTC",
	}}); err != nil {
		t.Fatalf("send schedule update: %v", err)
	}
	retimed := fr.until(t, 5*time.Second, "re-timed state", func(m WSMessage) bool {
		if m.Type != "automations_state" || m.AutomationsState == nil || len(m.AutomationsState.Automations) != 1 {
			return false
		}
		got := m.AutomationsState.Automations[0]
		return got.Schedule.Time == "18:30" && got.NextRunAt != nil && !got.NextRunAt.Equal(originalNext)
	}).AutomationsState.Automations[0]
	if retimed.NextRunAt == nil {
		t.Fatal("re-timed row lost its next run")
	}

	// Update an unknown id → error notice, nothing stored.
	if err := fr.conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "update", ID: "auto-nope", Title: "x", Prompt: "p", Kind: "daily", Time: "09:00",
	}}); err != nil {
		t.Fatalf("send unknown-id update: %v", err)
	}
	fr.until(t, 5*time.Second, "unknown-id update error", func(m WSMessage) bool {
		return m.Type == "notice" && m.Kind == "automations" && !m.Success
	})
}

// TestAutomationsOpFlagGate pins the gate: with the flag off the op is
// rejected with an error notice and no automations_state reply is sent.
func TestAutomationsOpFlagGate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, t.TempDir())
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	fr := newFrameReader(t, conn)
	fr.until(t, 5*time.Second, "session_state", func(m WSMessage) bool { return m.Type == "session_state" })
	fr.until(t, 5*time.Second, "config", func(m WSMessage) bool { return m.Type == "config" })

	if err := conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{Action: "list"}}); err != nil {
		t.Fatalf("send list: %v", err)
	}
	notice := fr.until(t, 5*time.Second, "flag-gate notice", func(m WSMessage) bool {
		return m.Type == "notice"
	})
	if notice.Kind != "automations" || notice.Success {
		t.Fatalf("flag-gated op notice = %+v", notice)
	}
	if s.ws.GetAutomationsEnabled() {
		t.Fatal("the flag must stay off in this test")
	}
}

// TestAutomationsWebCreateVisibleToScheduler pins the shared-store
// contract: an automation created through the WS op is claimed and fired by
// the host scheduler without a restart (the ops and the sweep share one
// store instance).
func TestAutomationsWebCreateVisibleToScheduler(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	storeDir := automation.DefaultDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, t.TempDir())
	fire := newFakeAutomationFire()
	s.setAutomationFire(fire.Fire, storeDir)
	s.setAutomationInterval(10 * time.Millisecond)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	fr := newFrameReader(t, conn)
	fr.until(t, 5*time.Second, "session_state", func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := fr.until(t, 5*time.Second, "config", func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	fr.until(t, 5*time.Second, "config echo", func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid
	})
	if err := conn.WriteJSON(WSMessage{Type: "config", Automations: "on", SessionID: sid}); err != nil {
		t.Fatalf("send automations on: %v", err)
	}
	fr.until(t, 5*time.Second, "automations on push", func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid && m.Automations == "on"
	})

	// Create a due automation through the tab (once, 10 minutes ago —
	// inside the catch-up window, so the very next sweep fires it).
	if err := conn.WriteJSON(WSMessage{Type: "automations_op", AutomationsOp: &AutomationsOpRequest{
		Action: "create",
		Title:  "Web-created",
		Prompt: "say hi",
		Kind:   "once",
		At:     time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
	}}); err != nil {
		t.Fatalf("send create: %v", err)
	}
	created := fr.until(t, 5*time.Second, "created state", func(m WSMessage) bool {
		return m.Type == "automations_state" && m.AutomationsState != nil && len(m.AutomationsState.Automations) == 1
	}).AutomationsState

	// The scheduler (started by the toggle) must claim + fire it.
	select {
	case fired := <-fire.fired:
		if fired.ID != created.Automations[0].ID {
			t.Fatalf("scheduler fired %s, want the web-created %s", fired.ID, created.Automations[0].ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler never fired the web-created automation (ops and sweep must share one store)")
	}

	// The fire goroutine records the run asynchronously (RecordFire → … →
	// RecordFinish), each an atomic store write under the temp XDG dir. The
	// handle's result is already buffered, so the fired signal above can
	// precede RecordFinish; wait for the terminal row so that last write
	// lands before t.TempDir's RemoveAll (otherwise the cleanup races the
	// writer and fails with "directory not empty", the flake class the
	// ShutdownSessions cleanup above already covers for session writers).
	// RecordFinish holds the store lock across the save, so observing a
	// terminal row means the write is on disk.
	st, err := s.automationStore()
	if err != nil {
		t.Fatalf("automation store: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		for _, r := range st.Runs(created.Automations[0].ID) {
			if r.Status != automation.RunRunning {
				return true
			}
		}
		return false
	})
}

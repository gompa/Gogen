package server

import (
	"strings"
	"testing"
	"time"

	"gogen/internal/automation"
)

// fakeAutomationFire records fired automations; each fire completes
// immediately with a stub session id.
type fakeAutomationFire struct {
	fired chan automation.Automation
}

func newFakeAutomationFire() *fakeAutomationFire {
	return &fakeAutomationFire{fired: make(chan automation.Automation, 16)}
}

func (f *fakeAutomationFire) Fire(a *automation.Automation) (*automation.FireHandle, error) {
	f.fired <- *a
	ch := make(chan automation.FireResult, 1)
	ch <- automation.FireResult{SessionID: "sess-scheduled"}
	return &automation.FireHandle{Result: ch}, nil
}

// seedDueAutomation stores a once-automation due 10 minutes ago — inside the
// catch-up window, so the very first sweep after the toggle fires it.
func seedDueAutomation(t *testing.T, dir string) automation.Automation {
	t.Helper()
	store, err := automation.OpenStore(dir)
	if err != nil {
		t.Fatalf("open automation store: %v", err)
	}
	a := automation.Automation{
		Title:      "Nightly triage",
		Prompt:     "triage the board",
		WorkingDir: t.TempDir(),
		Schedule:   automation.Schedule{Kind: automation.KindOnce, At: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)},
		Enabled:    true,
	}
	if err := store.Create(&a); err != nil {
		t.Fatalf("create automation: %v", err)
	}
	return a
}

// TestAutomationsToggleViaConfigWS drives the settings toggle end to end:
// config WS "automations: on" starts the scheduler (the due automation
// fires through the stubbed fire and lands in the run history with its
// session id), "automations: off" stops it, and the effective config is
// persisted so the toggle survives a restart.
func TestAutomationsToggleViaConfigWS(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	storeDir := automation.DefaultDir()

	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, t.TempDir())
	fire := newFakeAutomationFire()
	s.setAutomationFire(fire.Fire, storeDir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && m.SessionID == sid })

	if cfg.Automations != "off" {
		t.Fatalf("initial config push automations = %q, want off", cfg.Automations)
	}
	due := seedDueAutomation(t, storeDir)

	// Toggle on: the config push reflects it and the scheduler starts.
	if err := conn.WriteJSON(WSMessage{Type: "config", Automations: "on", SessionID: sid}); err != nil {
		t.Fatalf("send automations on: %v", err)
	}
	onCfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid && m.Automations == "on"
	})
	if onCfg.Automations != "on" {
		t.Fatalf("toggle-on push automations = %q", onCfg.Automations)
	}

	// The sweep fires the due automation through the stub.
	select {
	case fired := <-fire.fired:
		if fired.ID != due.ID {
			t.Fatalf("fired automation id = %s, want %s", fired.ID, due.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler never fired the due automation")
	}

	// The run history links the stub session id (wait for the terminal
	// record — the fire itself precedes the final status write).
	deadline := time.Now().Add(5 * time.Second)
	var store *automation.Store
	var runs []automation.Run
	var auto automation.Automation
	var ok bool
	var err error
	for time.Now().Before(deadline) {
		store, err = automation.OpenStore(storeDir)
		if err != nil {
			t.Fatalf("reopen automation store: %v", err)
		}
		runs = store.Runs(due.ID)
		if len(runs) > 0 && runs[0].Status == automation.RunCompleted {
			// The parent's last-run mirror lands in a second write after
			// the run row; wait for both.
			auto, ok = store.Get(due.ID)
			if ok && auto.LastRunStatus == automation.RunCompleted {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(runs) == 0 {
		t.Fatal("no run history recorded")
	}
	last := runs[0]
	if last.Status != automation.RunCompleted || last.SessionID != "sess-scheduled" {
		t.Fatalf("run record = %+v, want completed with the stub session id", last)
	}
	if !ok || auto.LastRunStatus != automation.RunCompleted {
		t.Fatalf("parent last_run_status = %+v (ok=%v), want completed", auto, ok)
	}

	// Toggle off: the scheduler stops — a newly created due automation is
	// left pending (it fires on the next toggle-on; nothing runs now).
	if err := conn.WriteJSON(WSMessage{Type: "config", Automations: "off", SessionID: sid}); err != nil {
		t.Fatalf("send automations off: %v", err)
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "config" && m.SessionID == sid && m.Automations == "off"
	})
	_ = seedDueAutomation(t, storeDir)
	select {
	case fired := <-fire.fired:
		t.Fatalf("scheduler fired after toggle-off: %+v", fired)
	case <-time.After(300 * time.Millisecond):
	}

	// Persisted for the next start: the effective config (the startup
	// config + live flag overlay) carries the toggle.
	if eff := s.effectiveConfig(); eff != nil && eff.Automations != "off" {
		t.Fatalf("effective config automations = %q, want off after the toggle-off", eff.Automations)
	}
}

// TestAutomationsToggleInvalidValueRejected pins the on/off validation for
// the automations toggle.
func TestAutomationsToggleInvalidValueRejected(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, t.TempDir())
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
	sid := cfg.SessionID

	if err := conn.WriteJSON(WSMessage{Type: "config", Automations: "maybe", SessionID: sid}); err != nil {
		t.Fatalf("send invalid automations: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "settings" || resp.Success || !contains(resp.Content, "invalid automations value") {
		t.Fatalf("invalid automations notice = %+v", resp)
	}
	if s.ws.GetAutomationsEnabled() {
		t.Fatal("invalid value must not enable the flag")
	}
}

// contains is the tiny substring helper (mirrors the project test style).
func contains(haystack, needle string) bool {
	return len(needle) == 0 || strings.Contains(haystack, needle)
}

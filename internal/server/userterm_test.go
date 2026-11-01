package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"

	"github.com/gorilla/websocket"
)

// collectOutput is a thread-safe sink for UserTerminal output.
type collectOutput struct {
	mu  sync.Mutex
	buf strings.Builder
	ch  chan struct{} // signaled on every write
}

func newCollectOutput() *collectOutput {
	return &collectOutput{ch: make(chan struct{}, 64)}
}

func (c *collectOutput) write(s string) {
	c.mu.Lock()
	c.buf.WriteString(s)
	c.mu.Unlock()
	select {
	case c.ch <- struct{}{}:
	default:
	}
}

func (c *collectOutput) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *collectOutput) waitContains(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if strings.Contains(c.String(), substr) {
			return
		}
		select {
		case <-c.ch:
		case <-deadline:
			t.Fatalf("timed out waiting for %q in output; got %q", substr, c.String())
		}
	}
}

// TestUserTerminalEchoAndExit spawns a real shell on a pty and verifies the
// full lifecycle: output streams, resize is accepted, exit is reported.
func TestUserTerminalEchoAndExit(t *testing.T) {
	// Pin the shell: $SHELL on dev machines (fish etc.) can be slow or
	// interactive-config heavy; /bin/sh is deterministic everywhere.
	t.Setenv("SHELL", "/bin/sh")
	out := newCollectOutput()
	ut, err := startUserTerminal(t.TempDir(), out.write)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer ut.Close()

	if ut.Title() == "" {
		t.Fatal("expected non-empty shell title")
	}
	if err := ut.Write([]byte("echo hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	out.waitContains(t, "hello", 10*time.Second)

	if err := ut.Resize(120, 40); err != nil {
		t.Fatalf("resize: %v", err)
	}

	if err := ut.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	select {
	case <-ut.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("shell did not exit after 'exit'")
	}
	if code := ut.ExitCode(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// Writes after exit must fail cleanly, not panic.
	if err := ut.Write([]byte("x")); err == nil {
		t.Fatal("expected error writing to exited shell")
	}
}

// TestWSUserTerminal drives the interactive user shell over a real WebSocket:
// the shell is spawned lazily on the first user_term_request, which answers
// user_term_opened → typing a command streams user_term_output → 'exit'
// produces user_term_exit with code 0.
func TestWSUserTerminal(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	exec := agent.NewExecutor(t.TempDir())
	a := agent.NewAgent(&termStreamStubProvider{}, exec, nil)
	s := newTestServer(t, a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:]+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 0) The server no longer spawns the shell at connect: ask for it.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
		t.Fatalf("send user_term_request: %v", err)
	}

	// 1) Expect user_term_opened in response to that request.
	deadline := time.Now().Add(15 * time.Second)
	opened := false
	for !opened {
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read (opened): %v", err)
		}
		switch msg.Type {
		case "user_term_opened":
			opened = true
			if msg.Content == "" {
				t.Fatal("user_term_opened without shell title")
			}
			if msg.WorkingDir == "" {
				t.Fatal("user_term_opened without working dir")
			}
		case "user_term_output", "config", "history", "response", "sessions":
			// handshake/banner noise — ignore
		case "user_term_exit":
			t.Fatalf("user shell failed to start: %q", msg.Content)
		}
	}

	// 2) Type a command; expect its output to stream back.
	sendShellInput(t, conn, "echo hello")
	var outputs strings.Builder
	deadline = time.Now().Add(15 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read (output): %v", err)
		}
		switch msg.Type {
		case "user_term_output":
			outputs.WriteString(msg.Content)
			if strings.Contains(outputs.String(), "hello") {
				goto gotOutput
			}
		case "user_term_exit":
			t.Fatalf("shell exited early: %q", msg.Content)
		}
	}
gotOutput:

	// 3) Exit the shell; expect user_term_exit with code 0.
	sendShellInput(t, conn, "exit")
	deadline = time.Now().Add(15 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read (exit): %v", err)
		}
		if msg.Type == "user_term_exit" {
			if msg.Code != 0 {
				t.Fatalf("user_term_exit code = %d, want 0 (%q)", msg.Code, msg.Content)
			}
			return
		}
		// Trailing output (e.g. the echoed "exit") may arrive before the event.
		if msg.Type != "user_term_output" {
			t.Fatalf("unexpected message %q while waiting for user_term_exit", msg.Type)
		}
	}
}

// TestWSUserTerminalRespawnAfterExit pins the ↻ flow the web UI offers on a
// dead shell: after user_term_exit the connection may ask again and must get a
// FRESH shell (the exited one was cleared from the holder), answered with its
// own user_term_opened. A silent request here is the "stuck on starting…" bug.
func TestWSUserTerminalRespawnAfterExit(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	seedShellProbe(t)
	exec := agent.NewExecutor(t.TempDir())
	a := agent.NewAgent(&termStreamStubProvider{}, exec, nil)
	s := newTestServer(t, a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:]+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	pump := startWSFramePump(conn)

	if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	if _, ok := pump.waitFor("user_term_opened", 15*time.Second); !ok {
		t.Fatalf("no user_term_opened for the first request; frames: %v", pump.types())
	}
	// Mark the first shell, so a reuse of the dead one is distinguishable from
	// a real respawn below.
	shellStateSet(t, conn)
	if got := shellStateProbe(t, conn, pump, "first"); got != "set" {
		t.Fatalf("first shell does not answer its own state marker (%q): the probe cannot tell shells apart", got)
	}

	// Kill the first shell; the exit report must arrive (the client keys its
	// ↻ affordance off it).
	sendShellInput(t, conn, "exit")
	if _, ok := pump.waitFor("user_term_exit", 15*time.Second); !ok {
		t.Fatalf("no user_term_exit after 'exit'; frames: %v", pump.types())
	}

	// Asking again (the ↻ button) must produce a second, new shell.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
		t.Fatalf("send respawn request: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for pump.count("user_term_opened") < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := pump.count("user_term_opened"); n != 2 {
		t.Fatalf("user_term_opened frames = %d, want 2 (spawn + respawn); frames: %v", n, pump.types())
	}
	// The respawned shell is a NEW process: the marker the first one held is
	// gone (a reuse of the exited shell would still answer "set").
	if got := shellStateProbe(t, conn, pump, "second"); got != "unset" {
		t.Fatalf("respawned shell still holds the first shell's marker (%q): no new shell was started", got)
	}
}

// wsFramePump buffers every frame a test WebSocket receives in the background.
// Reading in a goroutine is what makes an ABSENCE assertion possible: a read
// deadline that expires poisons a gorilla connection ("repeated read on failed
// websocket connection"), so a test cannot simply time out a read and continue.
type wsFramePump struct {
	mu     sync.Mutex
	frames []WSMessage
}

func startWSFramePump(conn *websocket.Conn) *wsFramePump {
	p := &wsFramePump{}
	go func() {
		for {
			var m WSMessage
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			p.mu.Lock()
			p.frames = append(p.frames, m)
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *wsFramePump) snapshot() []WSMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]WSMessage(nil), p.frames...)
}

// types lists the frame types received so far (failure diagnostics).
func (p *wsFramePump) types() []string {
	var out []string
	for _, m := range p.snapshot() {
		out = append(out, m.Type)
	}
	return out
}

// terminalFrameTypes lists the user_term_* frames received so far.
func (p *wsFramePump) terminalFrameTypes() []string {
	var out []string
	for _, m := range p.snapshot() {
		if strings.HasPrefix(m.Type, "user_term_") {
			out = append(out, m.Type)
		}
	}
	return out
}

func (p *wsFramePump) count(typ string) int {
	n := 0
	for _, m := range p.snapshot() {
		if m.Type == typ {
			n++
		}
	}
	return n
}

// waitFor waits up to timeout for the first frame of type typ.
func (p *wsFramePump) waitFor(typ string, timeout time.Duration) (WSMessage, bool) {
	deadline := time.Now().Add(timeout)
	for {
		for _, m := range p.snapshot() {
			if m.Type == typ {
				return m, true
			}
		}
		if time.Now().After(deadline) {
			return WSMessage{}, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// output concatenates every user_term_output frame received so far, in arrival
// order — the shell's own record of what it printed.
func (p *wsFramePump) output() string {
	var b strings.Builder
	for _, m := range p.snapshot() {
		if m.Type == "user_term_output" {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

// waitForOutput waits until the accumulated user_term_output frames contain
// substr, returning everything received so far.
func (p *wsFramePump) waitForOutput(substr string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		out := p.output()
		if strings.Contains(out, substr) {
			return out, true
		}
		if time.Now().After(deadline) {
			return out, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWSUserShellLazySpawn pins the lazy spawn (M4b): the shell is created on
// the first user_term_request, not at connect. A connection that never asks must
// create no pty — observable as "no user_term_* frame at all", since a /bin/sh
// started on a pty writes its prompt immediately — and must tolerate input and
// resize frames (empty holder). The first request spawns exactly one shell; two
// further requests reuse it instead of spawning another.
func TestWSUserShellLazySpawn(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	seedShellProbe(t)
	exec := agent.NewExecutor(t.TempDir())
	a := agent.NewAgent(&termStreamStubProvider{}, exec, nil)
	s := newTestServer(t, a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:]+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	pump := startWSFramePump(conn)

	// The handshake completes without a shell.
	if _, ok := pump.waitFor("config", 15*time.Second); !ok {
		t.Fatalf("no config frame; frames: %v", pump.types())
	}
	// Input/resize before any request are no-ops (empty holder) and must not
	// spawn a shell behind our back.
	sendShellInput(t, conn, "echo should-not-run")
	if err := conn.WriteJSON(WSMessage{Type: "user_term_resize", Cols: 100, Rows: 30}); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	time.Sleep(750 * time.Millisecond)
	if got := pump.terminalFrameTypes(); len(got) != 0 {
		t.Fatalf("terminal frames without a user_term_request: %v (all: %v)", got, pump.types())
	}

	// First request: exactly one shell.
	if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	opened, ok := pump.waitFor("user_term_opened", 15*time.Second)
	if !ok {
		t.Fatalf("no user_term_opened after the request; frames: %v", pump.types())
	}
	if opened.Content == "" || opened.WorkingDir == "" {
		t.Fatalf("user_term_opened = %+v, want shell title and working dir", opened)
	}
	// The lazily created shell is live: it answers input.
	sendShellInput(t, conn, "echo lazy-shell-ok")
	if out, ok := pump.waitForOutput("lazy-shell-ok", 15*time.Second); !ok {
		t.Fatalf("lazily spawned shell produced no output, got %q", out)
	}
	// Mark the shell, so a respawn masquerading as a reuse is caught below.
	shellStateSet(t, conn)
	if got := shellStateProbe(t, conn, pump, "before"); got != "set" {
		t.Fatalf("the live shell does not answer its own state marker (%q): the probe cannot tell shells apart", got)
	}

	// Two rapid requests must reuse the live shell (holder claim), not spawn a
	// second one — and BOTH must be answered. A repeat request that got no
	// reply left the client's tab on "starting…" for good: the client re-asks
	// whenever it does not believe a shell is up, so silence is not
	// recoverable (see spawnUserTerminal / terminalUserEnsure).
	for i := 0; i < 2; i++ {
		if err := conn.WriteJSON(WSMessage{Type: "user_term_request"}); err != nil {
			t.Fatalf("send request %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for pump.count("user_term_opened") < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	openedFrames := 0
	for _, m := range pump.snapshot() {
		if m.Type != "user_term_opened" {
			continue
		}
		openedFrames++
		if m.Content == "" || m.WorkingDir == "" {
			t.Fatalf("user_term_opened #%d = %+v, want shell title and working dir", openedFrames, m)
		}
	}
	if openedFrames != 3 {
		t.Fatalf("user_term_opened frames = %d, want 3 (one per request); frames: %v", openedFrames, pump.types())
	}
	// The same shell answered all three requests: it still holds the marker a
	// replacement process would have lost.
	if got := shellStateProbe(t, conn, pump, "after"); got != "set" {
		t.Fatalf("shell state marker = %q after two repeat requests, want set: a second shell was spawned", got)
	}
	if n := pump.count("user_term_exit"); n != 0 {
		t.Fatalf("unexpected user_term_exit; frames: %v", pump.types())
	}
}

// sendShellInput types one command line into the live shell. The terminator is
// platform-specific: the Windows user shell is cmd.exe (COMSPEC), which wants
// CRLF, while a POSIX shell accepts a bare LF.
func sendShellInput(t *testing.T, conn *websocket.Conn, line string) {
	t.Helper()
	eol := "\n"
	if runtime.GOOS == "windows" {
		eol = "\r\n"
	}
	if err := conn.WriteJSON(WSMessage{Type: "user_term_input", Content: line + eol}); err != nil {
		t.Fatalf("send input %q: %v", line, err)
	}
}

// shellProbeVar is the shell variable shellStateSet re-assigns. seedShellProbe
// pre-seeds it to "unset" in the server's environment, so a FRESHLY spawned
// shell (which inherits os.Environ() through the pty) reports "unset" while the
// live one, whose own assignment shadows the inherited value, reports "set".
const shellProbeVar = "GOGEN_SHELL_PROBE"

// seedShellProbe seeds the probe variable. It must run before the shell is
// spawned: the pty inherits the test's environment.
func seedShellProbe(t *testing.T) {
	t.Helper()
	t.Setenv(shellProbeVar, "unset")
}

// shellStateSet plants the probe value in the live shell (it prints nothing;
// the pty echo of the command is ignored).
func shellStateSet(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// cmd.exe takes the whole rest of the line as the value.
		sendShellInput(t, conn, "set "+shellProbeVar+"=set")
		return
	}
	sendShellInput(t, conn, shellProbeVar+"=set")
}

// shellStateProbe asks the live shell for the value of the probe variable and
// returns "set" or "unset". A pid probe (echo $$) is POSIX-only — cmd.exe
// prints the literal "$$" — so shell identity comes from state instead: only
// the SAME shell process holds the value shellStateSet assigned.
//
// The marker is EXPANDED by the shell, never literal in the command line: the
// pty echoes the command back, so a literal marker there would be
// indistinguishable from the answer.
func shellStateProbe(t *testing.T, conn *websocket.Conn, pump *wsFramePump, tag string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		// cmd.exe expands %VAR% at parse time; an unset variable expands to
		// the literal "%VAR%", which keeps the marker from being contiguous.
		sendShellInput(t, conn, "echo GGPROBE%"+shellProbeVar+"%"+tag)
	} else {
		// Braces end the variable name, so the tag can follow it directly.
		sendShellInput(t, conn, "echo GGPROBE${"+shellProbeVar+"}"+tag)
	}
	re := regexp.MustCompile("GGPROBE(set|unset)" + tag)
	deadline := time.Now().Add(15 * time.Second)
	for {
		out := pump.output()
		if sub := re.FindAllStringSubmatch(out, -1); len(sub) > 0 {
			return sub[len(sub)-1][1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no answer to the shell-state probe %s; output: %q", tag, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUserTermHolderClaimIsAtomic pins the claim spawnUserTerminal relies on to
// keep two concurrent user_term_requests from spawning two shells: of N racers
// exactly one wins trySet, the rest must close what they started.
func TestUserTermHolderClaimIsAtomic(t *testing.T) {
	t.Parallel()
	h := &userTermHolder{}
	const racers = 8
	var wg sync.WaitGroup
	won := make(chan *UserTerminal, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ut := &UserTerminal{}
			if h.trySet(ut) {
				won <- ut
			}
		}()
	}
	wg.Wait()
	close(won)
	if got := len(won); got != 1 {
		t.Fatalf("holders claimed = %d, want 1", got)
	}
	if h.get() == nil {
		t.Fatal("holder empty after a successful claim")
	}
}

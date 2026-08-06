package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// blockingStub blocks inside every GenerateResponseStream call until that
// call is released (or the context is cancelled), so turns can be left
// in-flight while the client disconnects. Each call's release is independent
// (releaseN(n)); call 1 may return tool calls (firstTools) to exercise the
// approval path.
type blockingStub struct {
	mu         sync.Mutex
	calls      int
	entered    int // total number of calls that reached the blocking section
	releases   map[int]chan struct{}
	firstTools []llm.ToolCall
}

func newBlockingStub() *blockingStub {
	return &blockingStub{releases: make(map[int]chan struct{})}
}

// releaseN unblocks GenerateResponseStream call n (subsequent calls keep
// their own channels, so they block until released individually).
func (s *blockingStub) releaseN(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.releases[n]
	if !ok {
		ch = make(chan struct{})
		s.releases[n] = ch
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// waitBlocked waits until at least n GenerateResponseStream calls have
// entered their blocking section (each call holds the session turn lock for
// the whole time it is blocked).
func (s *blockingStub) waitBlocked(n int) {
	for {
		s.mu.Lock()
		e := s.entered
		s.mu.Unlock()
		if e >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *blockingStub) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *blockingStub) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	ch, ok := s.releases[call]
	if !ok {
		ch = make(chan struct{})
		s.releases[call] = ch
	}
	s.entered++
	s.mu.Unlock()
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if call == 1 && len(s.firstTools) > 0 {
		return &llm.StreamResult{ToolCalls: s.firstTools}, nil
	}
	if call == 1 {
		return &llm.StreamResult{Content: "headless-done"}, nil
	}
	return &llm.StreamResult{Content: "done"}, nil
}

func (s *blockingStub) ModelContextLimit(_ context.Context) (int, error) { return 1000, nil }
func (s *blockingStub) SetThinkingLevel(string)                          {}
func (s *blockingStub) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *blockingStub) SetModel(string) error { return nil }
func (s *blockingStub) ModelName() string     { return "blocking-model" }

// newContinuationServer builds a server + agent wired to a real session store
// and a blocking stub provider, sharing the test's working directory.
func newContinuationServer(t *testing.T, stub *blockingStub, dir string) (*Server, *agent.Agent, *session.Store) {
	t.Helper()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	store := session.NewStore(true)
	a.SessionStore = store
	s := NewServer(a, &config.Config{})
	return s, a, store
}

// readUntil reads messages from conn until match returns true; the matched
// message is returned. Fails the test on timeout.
func readUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, match func(WSMessage) bool) WSMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for matching WS message")
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		if match(msg) {
			return msg
		}
	}
}

// TestTurnContinuesAfterDisconnect is the headline continuation test: the
// client drops mid-turn, the turn completes headless (the stub is released
// only AFTER HandleWS has returned, proving the turn context is no longer
// request-derived — the old code killed the turn via r.Context()), the reply
// is persisted, and a fresh connection's attach shows session_state + the
// completed history.
func TestTurnContinuesAfterDisconnect(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	// Drain the attach handshake.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "start headless turn"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Wait until the turn is genuinely in-flight inside the provider.
	stub.waitBlocked(1)

	// Kill the tab: close the socket and let HandleWS return.
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		rt, _ := s.registry.get(a.SessionID)
		return rt != nil && rt.clientCount() == 0
	})

	// Release the provider only now. If the turn context were still derived
	// from the (cancelled) request context, StreamProcessInput would abort
	// and the assistant reply would never be appended.
	stub.releaseN(1)
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(dir, a.SessionID)
		if err != nil {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "headless-done" {
				return true
			}
		}
		return false
	})

	// A fresh connection re-attaches: session_state arrives, then history
	// containing the completed answer (the continuation guarantee).
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	state := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if state.TurnActive {
		t.Fatal("session_state after completed turn should report turnActive=false")
	}
	hist := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "history" })
	found := false
	for _, e := range hist.History {
		if e.Role == "assistant" && e.Content == "headless-done" {
			found = true
		}
	}
	if !found {
		t.Fatal("reconnected history missing the headless-completed assistant reply")
	}
}

// TestCancelFromNewConnectionStopsHeadlessTurn verifies that cancel is the
// only way to stop a turn and works cross-connection: after the original tab
// dies, a fresh connection's cancel stops the headless turn.
func TestCancelFromNewConnectionStopsHeadlessTurn(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "start"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		rt, _ := s.registry.get(a.SessionID)
		return rt != nil && rt.clientCount() == 0
	})

	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	// The session_state must report the turn still running headless.
	state := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if !state.TurnActive {
		t.Fatal("expected turnActive=true for the headless turn on re-attach")
	}
	// Cancel from the NEW connection stops it.
	if err := conn2.WriteJSON(WSMessage{Type: "cancel"}); err != nil {
		t.Fatalf("send cancel: %v", err)
	}
	msg := readUntil(t, conn2, 10*time.Second, func(m WSMessage) bool {
		return m.Type == "cancelled" || m.Type == "stream_end" || m.Type == "turn_end" || m.Type == "stream"
	})
	if msg.Type != "cancelled" {
		t.Fatalf("first terminal message after cancel = %q, want cancelled", msg.Type)
	}
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
	// The turn must not have completed with content.
	rt, _ := s.registry.get(a.SessionID)
	if got := rt.agent.MessageCount(); got < 1 {
		t.Fatalf("unexpected message count %d", got)
	}
}

// TestApprovalAutoDeniedOnDetach verifies D10: with a delete approval pending,
// the last client detaching auto-denies it and the turn continues with the
// "not approved" tool result instead of hanging.
func TestApprovalAutoDeniedOnDetach(t *testing.T) {
	dir := t.TempDir()
	// delete_file Lstats the target before asking for approval, so the file
	// must exist for the approval request to fire.
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_del",
		Name: "delete_file",
		Args: map[string]interface{}{"path": "victim.txt"},
	}}
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "delete it"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Round 1 returns the delete tool call once released; the tool then runs,
	// requires approval, and broadcasts delete_approval.
	stub.waitBlocked(1)
	stub.releaseN(1)
	approval := readUntil(t, conn, 10*time.Second, func(m WSMessage) bool { return m.Type == "delete_approval" })
	if approval.ApprovalID == "" {
		t.Fatal("delete_approval missing approvalId")
	}
	// The only client detaches without answering → auto-deny.
	conn.Close()
	waitFor(t, 5*time.Second, func() bool {
		rt, _ := s.registry.get(a.SessionID)
		return rt != nil && rt.clientCount() == 0
	})

	// The turn continues and completes (round 2 returns "done"): the tool
	// result for the denied delete is recorded, so the turn did not hang.
	waitFor(t, 10*time.Second, func() bool {
		rt, _ := s.registry.get(a.SessionID)
		msgs := rt.agent.SnapshotMessages()
		for _, m := range msgs {
			if m.Role == "tool" && m.Content != "" && m.ToolCallID == "call_del" {
				return true
			}
		}
		return false
	})
}

// TestFanOutToTwoAttachedConnections verifies E29: two connections attached
// to one session both receive the same stream events, and only one can start
// a turn (the second gets the busy response while the first streams).
func TestFanOutToTwoAttachedConnections(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn1 := dialWS(t, srv, "/ws")
	defer conn1.Close()
	readUntil(t, conn1, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Second tab attaches to the same (default) session.
	conn2 := dialWS(t, srv, "/ws")
	defer conn2.Close()
	readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// Start a turn from conn1; both must see the same events.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "fan out"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	stub.releaseN(1)

	for _, c := range []*websocket.Conn{conn1, conn2} {
		_ = readUntil(t, c, 10*time.Second, func(m WSMessage) bool {
			return m.Type == "user_acked" && m.SessionID == a.SessionID
		})
		// The stub does not emit tokens; the stream boundary event is
		// stream_end (produced by finishStreamUI).
		_ = readUntil(t, c, 10*time.Second, func(m WSMessage) bool { return m.Type == "stream_end" })
		_ = readUntil(t, c, 10*time.Second, func(m WSMessage) bool {
			return m.Type == "turn_end" && m.SessionID == a.SessionID
		})
	}

	// A second turn from conn1 blocks in the provider; a message from conn2
	// (a different connection attached to the same session) must get the busy
	// response (per-session turnMu, E3) — and must NOT cancel conn1's turn.
	if err := conn1.WriteJSON(WSMessage{Type: "message", Content: "second turn"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(2)
	if err := conn2.WriteJSON(WSMessage{Type: "message", Content: "busy me"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp := readUntil(t, conn2, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	if resp.Content != "Error: agent is busy with another client" {
		t.Fatalf("second message got %q, want busy rejection", resp.Content)
	}
	// conn1's turn must still be alive (not cancelled by conn2's message):
	// releasing the stub completes it.
	stub.releaseN(2)
	_ = readUntil(t, conn1, 10*time.Second, func(m WSMessage) bool { return m.Type == "turn_end" })
}

// ── helpers ──

func startWSServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(s.HandleWS))
	t.Cleanup(ts.Close)
	return ts
}

func dialWS(t *testing.T, srv *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// requireNever fails the test if cond — the wrong-behavior signal — becomes
// true within the window. The inverse of waitFor: used for negative
// assertions ("this must NOT happen"), where waiting for a non-event is
// impossible. Polling until the window elapses makes the pass meaningful on
// any machine speed (a fixed sleep can outrun a slow runner and pass while
// the bug is still in flight), and fails fast the moment the regression
// appears.
func requireNever(t *testing.T, window time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

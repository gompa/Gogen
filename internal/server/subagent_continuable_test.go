package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// newContinuableServer builds a server with a quick mock for the parent and
// a configurable child provider factory.
func newContinuableServer(t *testing.T, childProvider func() llm.LLMProvider) (*Server, *agent.Agent) {
	t.Helper()
	dir := t.TempDir()
	stub := newBlockingStub()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(stub, exec, ctxMgr)
	store := session.NewStore(true)
	a.SessionStore = store
	// The parent's delivery turns (completion notices, replies) need a
	// provider that answers.
	p := llm.NewMockProvider()
	p.StreamResults = []*llm.StreamResult{{Content: "ack"}, {Content: "ack2"}}
	a.Provider = p
	s := NewServer(a, &config.Config{})
	s.ws.ProviderFactory = childProvider
	a.SetSubagentsEnabled(true)
	// A delivered message is appended to the parent transcript BEFORE the
	// delivery turn's final FlushSession lands (the write that persists
	// the reply). Tests return as soon as they observe the message, so
	// that write would race the test's TempDir removal ("directory not
	// empty"); wait for the delivery machinery to go quiet first.
	t.Cleanup(func() { waitForParentDeliveriesSettled(t, s, a) })
	// The SpawnBackground goroutine's post-turn orphan sweep (Store.Delete
	// → index rewrite) runs AFTER the child is unregistered, so a test can
	// return — and its TempDir start being removed — while that write is
	// still in flight (macOS CI: "TempDir RemoveAll cleanup: ... directory
	// not empty"). Wait for every spawn goroutine to exit first.
	t.Cleanup(func() { waitForChildSpawnsSettled(t, s) })
	return s, a
}

// waitForChildSpawnsSettled waits (bounded) until every SpawnBackground
// goroutine has fully exited — including its post-turn orphan sweep, the
// last disk write the lifecycle performs. A plain WaitGroup wait would
// hang the whole package if a test ever leaves a child blocked in a
// provider without cancelling it, so poll with a deadline instead.
func waitForChildSpawnsSettled(t *testing.T, s *Server) {
	t.Helper()
	sp := continuableSpawner(t, s)
	waitFor(t, 5*time.Second, func() bool {
		done := make(chan struct{})
		go func() {
			sp.spawnWg.Wait()
			close(done)
		}()
		select {
		case <-done:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	})
}

func continuableSpawner(t *testing.T, s *Server) *subagentSpawner {
	t.Helper()
	sp, ok := s.ws.SubagentSpawner.(*subagentSpawner)
	if !ok {
		t.Fatal("workspace spawner is not a *subagentSpawner")
	}
	return sp
}

// waitForParentDeliveriesSettled waits until the parent session's delivery
// machinery is quiescent: no turn running, nothing queued, and the delivery
// worker idle. The delivery turn appends its message to the transcript
// before its final flush completes, so observing the message is not enough —
// the disk write must land before the test's TempDir is removed, or the
// cleanup sporadically fails with "directory not empty".
func waitForParentDeliveriesSettled(t *testing.T, s *Server, a *agent.Agent) {
	t.Helper()
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		// The runtime was evicted, which only happens after its turn ended
		// (and flushed): nothing can be in flight.
		return
	}
	waitFor(t, 5*time.Second, func() bool {
		if active, _ := rt.turnState(); active {
			return false
		}
		if rt.hasPendingDeliveries() {
			return false
		}
		return !rt.deliverWorker.Load()
	})
}

// TestBackgroundSpawnCompletionNotice drives the full background path: the
// child runs to completion, the parent receives the completion notice as a
// user message, and the child stays registered (held) until release.
func TestBackgroundSpawnCompletionNotice(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "background report"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour // keep registered for the assertions

	id, err := sp.SpawnBackground(context.Background(), a, "do the thing", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected a child id")
	}
	// The child is registered and held (orphan eviction exempt).
	child := sp.children.get(id)
	if child == nil {
		t.Fatal("child not registered")
	}
	if !child.rt.held.Load() {
		t.Fatal("child must be held")
	}
	// The parent is notified when the child finishes.
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "[subagent") && deliveredMessages(a, "background report")
	})
	if child.statusOf() != "finished" {
		t.Fatalf("child status = %q, want finished", child.statusOf())
	}
	// The completion notice turn on the parent also completed.
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range a.SnapshotMessages() {
			if m.Role == "assistant" && strings.Contains(m.Content, "ack") {
				return true
			}
		}
		return false
	})
}

// TestSendMessageReplyCapture delivers a message to a finished child and
// verifies the child's reply is injected into the parent. The parent pane
// stays attached (a real user watching the parent), so the parent runtime
// is not orphan-evicted between the notice and the reply.
func TestSendMessageReplyCapture(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		// First call: the main job. Second call: the send_message turn.
		p.StreamResults = []*llm.StreamResult{
			{Content: "main job report"},
			{Content: "reply to message"},
		}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	id, err := sp.SpawnBackground(context.Background(), a, "main job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.children.get(id)
	waitFor(t, 5*time.Second, func() bool { return child.statusOf() == "finished" })

	if err := sp.SendMessage(a, id, "please elaborate"); err != nil {
		t.Fatal(err)
	}
	// The child's reply lands in the parent conversation.
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "reply to message")
	})
}

// TestInterruptAgent cancels the in-flight turn only: the child stays
// registered and continuable, and no completion notice is delivered.
func TestInterruptAgent(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{} // blocks until cancelled
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	id, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.children.get(id)
	waitFor(t, 5*time.Second, func() bool { return child.statusOf() == "running" })
	time.Sleep(100 * time.Millisecond) // let the turn enter the provider

	if err := sp.InterruptAgent(a, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return child.statusOf() == "idle" })
	if deliveredMessages(a, "[subagent") {
		t.Fatal("an interrupted child must not deliver a completion notice")
	}
	// Still live: messaging it works.
	if err := sp.SendMessage(a, id, "wake up"); err != nil {
		t.Fatalf("child should remain continuable after interrupt: %v", err)
	}
	// Authorization: a foreign caller cannot message/interrupt the child.
	if err := sp.SendMessage(&agent.Agent{SessionID: "someone-else"}, id, "hi"); err == nil {
		t.Fatal("foreign caller must not message the child")
	}
	// Cleanup: the wake-up reply turn blocks on the provider — cancel and
	// release the child so no goroutine outlives the test's temp dir.
	sp.cancelAll(a.SessionID)
	waitFor(t, 5*time.Second, func() bool { return sp.children.get(id) == nil })
}

// TestSubagentForkCopiesMessages pins the fork semantics: the child starts
// with a copy of the parent's history and the parent transcript is
// untouched.
func TestSubagentForkCopiesMessages(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "fork reply"}}
		return p
	})
	sp := continuableSpawner(t, s)

	a.Messages = []llm.Message{
		{Role: "user", Content: "original question"},
		{Role: "assistant", Content: "original answer"},
	}
	before := a.MessageCount()

	report, err := sp.Fork(context.Background(), a, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "fork reply") {
		t.Fatalf("fork report = %q", report)
	}
	if a.MessageCount() != before {
		t.Fatalf("parent message count changed: %d → %d", before, a.MessageCount())
	}
	// The fork's session carries the copied history (2) + job (1) + reply (1).
	forkID := sp.children.get("") // forks are foreground: no child record
	_ = forkID
	// Find the fork via the saved sessions: it persisted with ParentID.
	list, err := s.ws.Store.List(a.WorkingDir)
	if err != nil {
		t.Fatal(err)
	}
	var fork *agent.SessionInfo
	for i := range list {
		if list[i].ParentID == a.SessionID {
			fork = &list[i]
			break
		}
	}
	if fork == nil {
		t.Fatal("fork session not persisted under the parent")
	}
}

// TestForkPersistsFailedOutcome verifies a cancelled fork child is saved
// with the failed outcome, so the sidebar renders the true result after a
// reload/restart (same contract as Spawn/SpawnBackground).
func TestForkPersistsFailedOutcome(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.ProviderFactory = func() llm.LLMProvider {
		return &blockingProvider{}
	}
	sp := continuableSpawner(t, s)
	// Fork requires a non-empty transcript (with an assistant message to
	// fork from) to copy.
	a.Messages = []llm.Message{
		{Role: "user", Content: "original question"},
		{Role: "assistant", Content: "original answer"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sp.Fork(ctx, a, "", 0)
		done <- err
	}()
	// Wait for the fork child to register, then cancel mid-turn.
	var forkID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID {
				forkID = id
				return true
			}
		}
		return false
	})
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not return after cancel")
	}
	// The fork's session is saved with the failed outcome.
	info := store.Info(dir, forkID)
	if info == nil {
		t.Fatal("fork session not persisted")
	}
	if info.SubagentStatus != "failed" {
		t.Fatalf("fork outcome = %q, want failed", info.SubagentStatus)
	}
}

// TestChildRetentionRelease pins the retention window: a finished child is
// released (runtime evicted, child record dropped) after the window; the
// saved session stays.
func TestChildRetentionRelease(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = 150 * time.Millisecond

	id, err := sp.SpawnBackground(context.Background(), a, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.children.get(id)
	waitFor(t, 5*time.Second, func() bool { return child.statusOf() == "finished" })
	waitFor(t, 5*time.Second, func() bool {
		if _, ok := s.registry.get(id); ok {
			return false
		}
		if sp.children.get(id) != nil {
			return false
		}
		return !child.rt.held.Load()
	})
}

// TestParentEvictionCancelsChildren pins the parent-close policy: evicting
// the parent runtime cancels and releases its background children.
func TestParentEvictionCancelsChildren(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{} // keep the child running
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	id, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.children.get(id)
	waitFor(t, 5*time.Second, func() bool { return child.statusOf() == "running" })

	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("parent runtime missing")
	}
	// session_close is the explicit teardown path the policy covers.
	s.registry.closeRuntime(parentRt)

	waitFor(t, 5*time.Second, func() bool {
		if _, ok := s.registry.get(id); ok {
			return false
		}
		return sp.children.get(id) == nil
	})
}

// TestChildReportDeliversToParent drives the child-scoped report tool end
// to end: the child's model calls report during its main turn and the
// progress message lands in the parent conversation.
func TestChildReportDeliversToParent(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{
			{ToolCalls: []llm.ToolCall{{
				ID: "c1", Name: "report", Args: map[string]any{"message": "progress from child"},
			}}},
			{Content: "done"},
		}
		return p
	})
	// Children inherit the subagent feature flag from the workspace.
	s.ws.SetSubagentEnabled(true)
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour

	if _, err := sp.SpawnBackground(context.Background(), a, "report as you go", "", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "progress from child")
	})
	// The report lands mid-turn, so the child's turn (and its final
	// flush) is still in flight when the parent sees it. The completion
	// notice is delivered only after that flush lands — wait for it so no
	// session write outlives the test's temp dir.
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "finished: done")
	})
}

// TestChildCapReleasesOldest pins the per-parent finished cap: spawning
// beyond the cap releases the oldest finished children.
func TestChildCapReleasesOldest(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	sp.maxFinished = 2

	for i := 0; i < 4; i++ {
		if _, err := sp.SpawnBackground(context.Background(), a, "job", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	// Children finish CONCURRENTLY (spawn order is not finish order), so
	// the per-id release order is not deterministic: the deterministic
	// contract is that the cap leaves exactly maxFinished registered
	// children and every survivor is finished. (Which two are released
	// depends on finish timing — the implementation releases the oldest
	// FINISHED children.)
	waitFor(t, 5*time.Second, func() bool {
		return len(sp.children.childrenOf(a.SessionID)) == sp.maxFinished
	})
	for _, c := range sp.children.childrenOf(a.SessionID) {
		if !c.isFinished() {
			t.Fatalf("surviving child %s is not finished", c.id)
		}
	}
	// Settle: each child's completion notice is enqueued/delivered only
	// after its final flush lands, so waiting for all four notices
	// (delivered into the parent transcript, or queued at the registry
	// once the parent runtime was orphan-evicted) guarantees every child's
	// session write has completed. The parent's own delivery-turn flush is
	// covered by waitForParentDeliveriesSettled.
	waitFor(t, 5*time.Second, func() bool {
		delivered := deliveredCount(a, "finished: done")
		s.registry.parentDeliverMu.Lock()
		queued := len(s.registry.parentDeliveries[a.SessionID])
		s.registry.parentDeliverMu.Unlock()
		return delivered+queued == 4
	})
	waitForParentDeliveriesSettled(t, s, a)
}

// TestLiveChildCapRefuses pins the live-child bound: spawning beyond
// maxLive is refused with an error (never silently released mid-task) and
// the refused child's runtime is released while earlier children stay
// registered.
func TestLiveChildCapRefuses(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{} // keeps the first child running
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	sp.maxLive = 1

	id1, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return sp.children.get(id1) != nil })

	// A second live child would exceed the cap: refused.
	if _, err := sp.SpawnBackground(context.Background(), a, "second job", "", 0); err == nil {
		t.Fatal("spawning beyond the live cap must be refused")
	}
	// The refused child was released; the first stays registered.
	if sp.children.get(id1) == nil {
		t.Fatal("the first child must stay registered")
	}
	// Cleanup: cancel the running child so its turn goroutine exits.
	sp.cancelAll(a.SessionID)
	waitFor(t, 5*time.Second, func() bool { return sp.children.get(id1) == nil })
}

// TestConcurrentLimitFromConfig pins the user-facing concurrent-subagent
// limit (subagent_max_concurrent): with the workspace value set, background
// spawning beyond it is refused (the refused child's runtime is released),
// and foreground Spawn / Fork are refused while the cap is full.
func TestConcurrentLimitFromConfig(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &gateProvider{release: gate}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(2)

	id1, err := sp.SpawnBackground(context.Background(), a, "job one", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := sp.SpawnBackground(context.Background(), a, "job two", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return sp.children.get(id1) != nil && sp.children.get(id2) != nil
	})

	// A third background child exceeds the limit: refused and released.
	if _, err := sp.SpawnBackground(context.Background(), a, "job three", "", 0); err == nil ||
		!strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("background spawn beyond the concurrent limit must be refused with a limit error, got %v", err)
	}
	// Foreground spawn and fork are refused while the cap is full.
	if _, err := sp.Spawn(context.Background(), a, "foreground job", "", 0); err == nil ||
		!strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("foreground spawn at a full cap must be refused with a limit error, got %v", err)
	}
	if _, err := sp.Fork(context.Background(), a, "", 0); err == nil ||
		!strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("fork at a full cap must be refused with a limit error, got %v", err)
	}
	// The two accepted children stay registered.
	if sp.children.get(id1) == nil || sp.children.get(id2) == nil {
		t.Fatal("accepted children must stay registered")
	}
	// Cleanup: cancel the running children so their turn goroutines exit.
	sp.cancelAll(a.SessionID)
}

// TestConcurrentLimitCountsOnlyLiveChildren pins that FINISHED children do
// not count toward the concurrent limit: with a limit of 1, a second spawn
// is allowed once the first child has finished.
func TestConcurrentLimitCountsOnlyLiveChildren(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(1)
	// Held: the parent has no attached clients, so the orphan re-check after
	// the completion-notice turn would evict it while we wait for the first
	// child to finish.
	if parentRt, ok := s.registry.get(a.SessionID); ok {
		parentRt.held.Store(true)
	}

	id1, err := sp.SpawnBackground(context.Background(), a, "first job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		c := sp.children.get(id1)
		return c != nil && c.isFinished()
	})
	// The finished child no longer counts: the second spawn is allowed.
	id2, err := sp.SpawnBackground(context.Background(), a, "second job", "", 0)
	if err != nil {
		t.Fatalf("spawn after the first child finished must be allowed: %v", err)
	}
	if id2 == id1 {
		t.Fatal("expected a fresh child id")
	}
	// Both completion pipelines must be absorbed before the test returns.
	// The notice delivery happens AFTER the child's outcome FlushSession,
	// so observing BOTH notices in the parent transcript proves both child
	// sessions are persisted; the waitForParentDeliveriesSettled cleanup
	// then bounds the parent ack turns' final flushes. Without this, a
	// child goroutine starved until after cleanup (Windows CI) writes its
	// session file while t.TempDir() is being removed ("TempDir RemoveAll
	// cleanup: ... The directory is not empty").
	waitFor(t, 5*time.Second, func() bool {
		return deliveredCount(a, "[subagent") >= 2
	})
}

// TestInterruptFreesConcurrentSlot pins that the concurrent limit counts
// ACTIVE children: interrupting a child frees its slot immediately (the
// refusal error promises interrupt_agent makes room) while the interrupted
// child stays registered and continuable until its retention window.
func TestInterruptFreesConcurrentSlot(t *testing.T) {
	n := 0
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		n++
		if n == 2 {
			p := llm.NewMockProvider()
			p.StreamResults = []*llm.StreamResult{{Content: "done"}}
			return p
		}
		return &blockingProvider{}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(1)
	// Held: the parent has no attached clients, so the orphan re-check
	// after id2's completion-notice turn would evict it before the
	// foreground spawn below.
	if parentRt, ok := s.registry.get(a.SessionID); ok {
		parentRt.held.Store(true)
	}

	id1, err := sp.SpawnBackground(context.Background(), a, "long job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.children.get(id1)
	waitFor(t, 5*time.Second, func() bool { return child.statusOf() == "running" })
	// Wait until the turn is actually in flight: cancelInFlight is a
	// no-op before the turn registers its cancel handle, so an interrupt
	// fired at mere "running" status (set at registration) could be lost.
	waitFor(t, 5*time.Second, func() bool {
		active, _ := child.rt.turnState()
		return active
	})

	if err := sp.InterruptAgent(a, id1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return child.statusOf() == "idle" })

	// The slot is free: a new background spawn is allowed while the
	// interrupted child is still registered.
	id2, err := sp.SpawnBackground(context.Background(), a, "second job", "", 0)
	if err != nil {
		t.Fatalf("spawn after interrupt must be allowed (an idle child holds no slot): %v", err)
	}
	if sp.children.get(id2) == nil {
		t.Fatal("the second child must stay registered")
	}
	// Let it finish so the foreground admission below is deterministic
	// (with the cap at 1 a still-running id2 would refuse it).
	waitFor(t, 5*time.Second, func() bool {
		c := sp.children.get(id2)
		return c != nil && c.statusOf() == "finished"
	})

	// A foreground spawn is admitted at the free slot (its slot is
	// released when the turn is cancelled).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sp.Spawn(ctx, a, "foreground job", "", 0)
		done <- err
	}()
	var fgID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID && id != id1 && id != id2 {
				fgID = id
				return true
			}
		}
		return false
	})
	if fgID == "" {
		t.Fatal("foreground spawn at a free slot must be admitted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("foreground spawn did not return after cancel")
	}
	sp.cancelAll(a.SessionID)
	waitFor(t, 5*time.Second, func() bool {
		return sp.children.get(id1) == nil && sp.children.get(id2) == nil
	})
}

// TestForegroundCountsTowardConcurrentLimit pins that an in-flight
// foreground child holds a slot of the concurrent limit: with the limit at
// 2 and one background + one foreground child running, a second background
// spawn is refused — and allowed again once the foreground child is gone.
func TestForegroundCountsTowardConcurrentLimit(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(2)

	id1, err := sp.SpawnBackground(context.Background(), a, "background job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		c := sp.children.get(id1)
		return c != nil && c.statusOf() == "running"
	})

	// A foreground child in flight (blocking provider, cancelled below).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := sp.Spawn(ctx, a, "foreground job", "", 0)
		done <- err
	}()
	var fgID string
	waitFor(t, 5*time.Second, func() bool {
		for _, id := range s.registry.activeIDs() {
			if id != a.SessionID && id != id1 {
				fgID = id
				return true
			}
		}
		return false
	})
	if fgID == "" {
		t.Fatal("foreground child not registered")
	}

	// Active = 2 (background + foreground): the cap is full.
	if _, err := sp.SpawnBackground(context.Background(), a, "second background", "", 0); err == nil ||
		!strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("background spawn with an in-flight foreground child at the cap must be refused, got %v", err)
	}

	// Cancel the foreground child: its slot is released when Spawn returns.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("foreground spawn did not return after cancel")
	}
	// Active = 1 again: the background spawn is allowed.
	id3, err := sp.SpawnBackground(context.Background(), a, "second background", "", 0)
	if err != nil {
		t.Fatalf("background spawn after the foreground child finished must be allowed: %v", err)
	}
	if sp.children.get(id3) == nil {
		t.Fatal("the second background child must stay registered")
	}
	sp.cancelAll(a.SessionID)
}

// TestForegroundSlotReleasedOnFailure pins the deferred foreground release:
// a foreground spawn that fails BEFORE its turn starts (model selection)
// must free its slot, or the parent would lose a slot of the concurrent
// limit for nothing.
func TestForegroundSlotReleasedOnFailure(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "done"}}
		return p
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(1)

	// Fails in newChildRuntime (unknown model), after the slot was taken.
	if _, err := sp.Spawn(context.Background(), a, "job", "no/such-model", 0); err == nil ||
		!strings.Contains(err.Error(), "subagent model") {
		t.Fatalf("foreground spawn with an unknown model must fail with a model error, got %v", err)
	}
	// The slot is free: a background spawn at the cap is allowed.
	id, err := sp.SpawnBackground(context.Background(), a, "job", "", 0)
	if err != nil {
		t.Fatalf("background spawn after a failed foreground spawn must be allowed: %v", err)
	}
	if sp.children.get(id) == nil {
		t.Fatal("the background child must stay registered")
	}
	sp.cancelAll(a.SessionID)
}

// TestLiveGuardBoundsIdleChildren pins the internal non-finished guard:
// interrupted (idle) children hold no slot of the user-facing concurrent
// limit, but they still count against the memory guard, so a
// spawn+interrupt loop cannot accumulate unbounded runtimes.
func TestLiveGuardBoundsIdleChildren(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &blockingProvider{}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour
	s.ws.SetSubagentMaxConcurrent(4)
	sp.maxLiveGuard = 2

	var ids []string
	for _, job := range []string{"job one", "job two"} {
		id, err := sp.SpawnBackground(context.Background(), a, job, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		c := sp.children.get(id)
		waitFor(t, 5*time.Second, func() bool { return c.statusOf() == "running" })
		// The turn must be in flight before the interrupt: cancelInFlight
		// is a no-op before the turn registers its cancel handle.
		waitFor(t, 5*time.Second, func() bool {
			active, _ := c.rt.turnState()
			return active
		})
		if err := sp.InterruptAgent(a, id); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 5*time.Second, func() bool { return c.statusOf() == "idle" })
	}
	// Active = 0 (well under the limit of 4), but non-finished = 2 = the
	// guard: the next spawn is refused by the guard, not the user limit.
	if _, err := sp.SpawnBackground(context.Background(), a, "third job", "", 0); err == nil ||
		!strings.Contains(err.Error(), "too many live subagents") {
		t.Fatalf("spawn at the live guard must be refused by the guard, got %v", err)
	}
	sp.cancelAll(a.SessionID)
}

// TestOnTurnEndReplyBaseline pins the reply-capture baseline: only
// messages the delivered turn actually produced count as the reply — an
// older assistant message (e.g. the main job report) must never be
// re-delivered to the parent as the reply to an interrupted send_message.
func TestOnTurnEndReplyBaseline(t *testing.T) {
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		p := llm.NewMockProvider()
		p.StreamResults = []*llm.StreamResult{{Content: "ack"}}
		return p
	})
	sp := continuableSpawner(t, s)

	newChild := func(msgs []llm.Message) *backgroundChild {
		stub := newBlockingStub()
		dir := t.TempDir()
		childAgent := agent.NewAgent(stub, agent.NewExecutor(dir),
			contextmgr.NewManager(stub, contextmgr.Settings{ContextLimit: 1000}))
		childAgent.Messages = msgs
		return &backgroundChild{
			id:       "child-1",
			parentID: a.SessionID,
			label:    "subagent: test",
			depth:    1,
			rt:       newSessionRuntime(childAgent),
			sp:       sp,
			status:   "idle",
		}
	}

	// Completed reply turn: the new assistant message is delivered.
	c := newChild([]llm.Message{
		{Role: "user", Content: "job"},
		{Role: "assistant", Content: "old report"},
	})
	c.pendingReply = true
	c.replyBase = 2 // after "old report"
	c.rt.agent.Messages = append(c.rt.agent.Messages,
		llm.Message{Role: "user", Content: "steer"},
		llm.Message{Role: "assistant", Content: "new reply"},
	)
	c.onTurnEnd()
	waitFor(t, 5*time.Second, func() bool { return deliveredMessages(a, "new reply") })

	// Interrupted reply turn (no completed round): the delivered user
	// message is appended but no assistant message is — the older report
	// must NOT be re-delivered as the reply.
	c = newChild([]llm.Message{
		{Role: "user", Content: "job"},
		{Role: "assistant", Content: "old report"},
	})
	c.pendingReply = true
	c.replyBase = 2
	c.rt.agent.Messages = append(c.rt.agent.Messages, llm.Message{Role: "user", Content: "steer"})
	c.onTurnEnd()
	time.Sleep(200 * time.Millisecond)
	if deliveredMessages(a, "old report") {
		t.Fatal("an interrupted reply turn must not re-deliver an older assistant message")
	}
}

// gateProvider streams one result only after release is closed, so a test
// can deterministically finish a child turn AFTER a parent eviction.
type gateProvider struct {
	release chan struct{}
}

func (p *gateProvider) Name() string { return "gate" }
func (p *gateProvider) ModelName() string {
	return "gate-model"
}
func (p *gateProvider) SetModel(string) error { return nil }
func (p *gateProvider) SetThinkingLevel(string) {
}
func (p *gateProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (p *gateProvider) GenerateResponse(context.Context, []llm.Message, map[string]struct{}, []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}
func (p *gateProvider) GenerateResponseStream(ctx context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	if h != nil && h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if h != nil && h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
	return &llm.StreamResult{Content: "gated report"}, nil
}
func (p *gateProvider) ModelContextLimit(context.Context) (int, error) { return 1000, nil }

// TestBackgroundSpawnCompletionQueuedUntilParentLive pins the deferred
// completion delivery: a parent that is orphan-evicted (idle, no viewers)
// while its background child runs receives the child's completion notice
// when the session is next registered — the notice is queued, not dropped.
func TestBackgroundSpawnCompletionQueuedUntilParentLive(t *testing.T) {
	gate := make(chan struct{})
	s, a := newContinuableServer(t, func() llm.LLMProvider {
		return &gateProvider{release: gate}
	})
	sp := continuableSpawner(t, s)
	sp.retain = time.Hour // keep the child registered for the assertions

	id, err := sp.SpawnBackground(context.Background(), a, "gated job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := sp.children.get(id)
	if child == nil {
		t.Fatal("child not registered")
	}
	// Orphan-evict the parent while the child is mid-turn: the evict hook
	// deliberately does NOT fire for orphan eviction, so the child keeps
	// running (its completion is queued instead).
	parentRt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("parent runtime missing")
	}
	s.registry.evictRuntime(parentRt)
	close(gate) // let the child finish

	// The child finishes; the parent is not live, so the completion notice
	// is queued in the registry rather than delivered.
	waitFor(t, 5*time.Second, func() bool {
		s.registry.parentDeliverMu.Lock()
		defer s.registry.parentDeliverMu.Unlock()
		return len(s.registry.parentDeliveries[a.SessionID]) == 1
	})
	time.Sleep(200 * time.Millisecond)
	if deliveredMessages(a, "[subagent") {
		t.Fatal("completion must not be delivered while the parent is not live")
	}

	// The parent session is reopened: registering a fresh runtime flushes
	// the queued notice into it.
	s.registry.register(a.SessionID, newSessionRuntime(a))
	waitFor(t, 5*time.Second, func() bool {
		return deliveredMessages(a, "[subagent") && deliveredMessages(a, "gated report")
	})
}

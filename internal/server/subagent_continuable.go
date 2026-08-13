package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	sesspkg "gogen/internal/session"
)

// Continuable (background) subagents: children that keep running after the
// parent's tool call returns, can be messaged (send_message), interrupted
// (interrupt_agent), listed (list_agents), and report progress to the
// parent. Web host only in v1 (the TUI spawner stays foreground-only, so
// its agents never see the continuable tools or the run_in_background
// parameter).
//
// Lifecycle:
//   - SpawnBackground creates and registers the child exactly like the
//     foreground Spawn, then runs the turn in a goroutine instead of
//     blocking. The runtime is marked held, so orphan eviction (no clients,
//     no turn) never collects it mid-lifecycle.
//   - On natural completion the final report is delivered to the parent
//     conversation (delivery service; queued for the session's next
//     registration when the parent runtime is not live — an
//     orphan-evicted/closed parent must not lose the outcome) and a
//     retention timer is armed.
//   - Finished children stay registered for a retention window (bounded
//     memory), then are released: held cleared, runtime evicted (the saved
//     session stays attachable via the sidebar).
//   - A per-parent cap bounds how many finished children accumulate.
//   - When the PARENT session is evicted (closed/cap/orphan), all of its
//     children are cancelled and released (fail-safe: a child whose parent
//     is gone cannot deliver anyway).
//   - interrupt_agent cancels only the in-flight turn; the child stays
//     alive and continuable (no completion notice for an interruption).

const (
	// defaultSubagentRetain is how long a finished continuable child stays
	// registered after its main job completes before its runtime is
	// released (the saved session remains attachable). Mirrors the
	// background-jobs retention philosophy.
	defaultSubagentRetain = 10 * time.Minute
	// defaultMaxFinishedSubagents caps finished continuable children per
	// parent; beyond it the oldest finished children are released.
	defaultMaxFinishedSubagents = 32
	// defaultMaxLiveSubagents caps NON-finished continuable children per
	// parent (running + idle). A child whose turn never ends arms no
	// retention timer, so without this a stuck parent could accumulate
	// unbounded runtimes. Spawning beyond it is refused with an error —
	// never silently released mid-task.
	defaultMaxLiveSubagents = 64
)

// backgroundChild is one live continuable subagent.
type backgroundChild struct {
	id       string
	parentID string
	label    string
	depth    int
	// createdAt stamps registration order; childrenOf sorts by it so the
	// listing shows oldest first (newest last).
	createdAt time.Time
	rt        *sessionRuntime
	sp        *subagentSpawner // owning spawner (for parent lookup in onTurnEnd)

	mu           sync.Mutex
	status       string // "running" | "idle" | "finished"
	result       string // final report (status finished)
	err          error  // final error (status finished)
	finishedAt   time.Time
	released     bool
	pendingReply bool // a send_message delivery is in flight; capture the next turn's reply
	replyBase    int  // len(SnapshotMessages()) when the delivery turn started
}

// childRegistry owns the live continuable children of a server.
type childRegistry struct {
	mu       sync.Mutex
	children map[string]*backgroundChild
}

func newChildRegistry() *childRegistry {
	return &childRegistry{children: make(map[string]*backgroundChild)}
}

func (cr *childRegistry) register(c *backgroundChild) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.children[c.id] = c
}

func (cr *childRegistry) get(id string) *backgroundChild {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.children[id]
}

// childrenOf returns the live children of parentID (id → child), newest
// last (registration order, by createdAt).
func (cr *childRegistry) childrenOf(parentID string) []*backgroundChild {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	var out []*backgroundChild
	for _, c := range cr.children {
		if c.parentID == parentID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.Before(out[j].createdAt) })
	return out
}

func (c *backgroundChild) setStatus(s string) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

// finish records the main-job outcome and closes done.
func (c *backgroundChild) finish(report string, err error) {
	c.mu.Lock()
	c.status = "finished"
	c.result = report
	c.err = err
	c.finishedAt = time.Now()
	c.mu.Unlock()
}

func (c *backgroundChild) isFinished() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status == "finished"
}

// onTurnEnd is installed as the child runtime's turn-end hook: when a
// send_message delivery's turn has ended, the child's last assistant reply
// is injected into the parent conversation (delivery service; dropped when
// the parent is gone).
func (c *backgroundChild) onTurnEnd() {
	c.mu.Lock()
	if !c.pendingReply {
		c.mu.Unlock()
		return
	}
	c.pendingReply = false
	base := c.replyBase
	c.mu.Unlock()

	msgs := c.rt.agent.SnapshotMessages()
	var reply string
	// Only messages the delivered turn actually produced count as the
	// reply: an interrupted turn (no completed round) appends nothing, so
	// the scan finds nothing and no stale assistant message is re-sent.
	for i := len(msgs) - 1; i >= base; i-- {
		if msgs[i].Role == "assistant" && msgs[i].Content != "" {
			reply = msgs[i].Content
			break
		}
	}
	if reply == "" {
		return
	}
	// Deliver to the live parent, or queue for its next registration when
	// the parent runtime is not live (orphan-evicted/closed mid-reply).
	c.sp.s.registry.deliverToParent(c.parentID, fmt.Sprintf("[subagent %s] %s", c.label, reply))
}

// --- spawner implementation ---

func (sp *subagentSpawner) retainWindow() time.Duration {
	if sp.retain > 0 {
		return sp.retain
	}
	return defaultSubagentRetain
}

func (sp *subagentSpawner) maxFinishedLimit() int {
	if sp.maxFinished > 0 {
		return sp.maxFinished
	}
	return defaultMaxFinishedSubagents
}

func (sp *subagentSpawner) maxLiveLimit() int {
	if sp.maxLive > 0 {
		return sp.maxLive
	}
	return defaultMaxLiveSubagents
}

// liveChildCount returns the number of non-finished (running + idle)
// children of parentID. Lock order: children.mu → c.mu (matches
// enforceChildCap).
func (sp *subagentSpawner) liveChildCount(parentID string) int {
	sp.children.mu.Lock()
	defer sp.children.mu.Unlock()
	n := 0
	for _, c := range sp.children.children {
		if c.parentID == parentID && !c.isFinished() {
			n++
		}
	}
	return n
}

// SpawnBackground implements agent.ContinuableSubagentSpawner: the child is
// created exactly like the foreground Spawn, then its turn runs in a
// goroutine. The parent tool call returns the child id immediately; the
// parent is notified when the child finishes.
func (sp *subagentSpawner) SpawnBackground(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	s := sp.s
	childRt, label, rawJob, err := sp.newChildRuntime(ctx, parent, job, model, depth)
	if err != nil {
		return "", err
	}
	child := &backgroundChild{
		id:        childRt.agent.SessionID,
		parentID:  parent.SessionID,
		label:     label,
		depth:     depth + 1,
		createdAt: time.Now(),
		rt:        childRt,
		sp:        sp,
		status:    "running",
	}
	sp.children.register(child)
	// Held: orphan eviction must never collect a continuable child while
	// it is alive or awaiting release.
	childRt.held.Store(true)
	// Reply capture: pendingReply is armed by the delivery-start hook (the
	// send_message turn ACTUALLY starting — not when it was queued) and
	// consumed by the turn-end hook, which injects the reply into the
	// parent.
	childRt.turnEndHook = child.onTurnEnd
	childRt.deliverStartHook = func() {
		child.mu.Lock()
		child.pendingReply = true
		// Baseline the child's history at delivery start so reply capture
		// can only pick up messages the delivered turn actually produced —
		// an interrupted turn (no completed round) must not re-deliver an
		// older assistant message as the reply.
		child.replyBase = len(child.rt.agent.SnapshotMessages())
		child.mu.Unlock()
	}
	sp.enforceChildCap(parent.SessionID)
	// Live-child bound (checked after registration so concurrent spawns
	// cannot slip past it): the just-registered child counts toward the
	// cap, so strictly-greater means we are one over.
	if sp.liveChildCount(parent.SessionID) > sp.maxLiveLimit() {
		sp.releaseChild(child)
		return "", fmt.Errorf("live subagent limit reached (%d): wait for running subagents to finish or interrupt_agent them", sp.maxLiveLimit())
	}

	go func() {
		report, runErr := sp.runChildTurn(context.Background(), childRt, agent.FormatSubagentJob(rawJob))
		if childRt.evicted.Load() {
			return // released while running (parent close / retention) — nothing to notify
		}
		if runErr != nil && errors.Is(runErr, context.Canceled) {
			// Interrupted (interrupt_agent / child-pane cancel): the child
			// stays alive and continuable; no completion notice. Arm the
			// retention timer so an idle interrupted child cannot linger in
			// memory forever (the parent's lifetime no longer bounds it:
			// orphan eviction does not cancel children).
			child.setStatus("idle")
			time.AfterFunc(sp.retainWindow(), func() { sp.maybeReleaseChild(child) })
			return
		}
		child.finish(report, runErr)
		// Enforce the per-parent finished cap NOW (spawn-time enforcement
		// misses fast jobs that finish after their siblings spawned):
		// release the oldest finished children beyond the cap.
		sp.enforceChildCap(parent.SessionID)
		success := runErr == nil
		summary := truncateReport(report, runErr)
		verb := "finished"
		if !success {
			verb = "failed"
		}
		if parentRt, ok := s.registry.get(parent.SessionID); ok {
			parentRt.broadcast(WSMessage{
				Type:            "subagent_finished",
				SessionID:       parent.SessionID,
				SubagentID:      child.id,
				SubagentLabel:   label,
				SubagentParent:  parent.SessionID,
				SubagentSuccess: success,
				SubagentSummary: summary,
			})
		}
		// Deliver the completion notice — immediately when the parent is
		// live, queued for its next registration otherwise (deliverToParent
		// re-resolves the runtime, so this also covers the eviction race
		// between the lookup above and the queue append).
		s.registry.deliverToParent(parent.SessionID, fmt.Sprintf("[subagent %s] %s: %s", label, verb, summary))
		// Retention: release the runtime after the window so finished
		// children cannot accumulate in memory. The saved session stays
		// attachable; release is idempotent (child.released).
		time.AfterFunc(sp.retainWindow(), func() { sp.maybeReleaseChild(child) })
	}()
	return child.id, nil
}

// enforceChildCap releases the oldest finished children of parentID beyond
// the per-parent cap.
func (sp *subagentSpawner) enforceChildCap(parentID string) {
	sp.children.mu.Lock()
	var finished []*backgroundChild
	for _, c := range sp.children.children {
		if c.parentID == parentID && c.isFinished() {
			finished = append(finished, c)
		}
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].finishedAt.Before(finished[j].finishedAt) })
	overflow := len(finished) - sp.maxFinishedLimit()
	var victims []*backgroundChild
	if overflow > 0 {
		victims = append(victims, finished[:overflow]...)
	}
	sp.children.mu.Unlock()
	for _, c := range victims {
		sp.releaseChild(c)
	}
}

// maybeReleaseChild runs when a child's retention window elapses. A child
// with a turn currently running (e.g. a send_message reply) is NOT released
// mid-turn — the timer re-arms so the reply completes and the child is
// released after the next quiet window. Idle children are released.
func (sp *subagentSpawner) maybeReleaseChild(c *backgroundChild) {
	if active, _ := c.rt.turnState(); active {
		time.AfterFunc(sp.retainWindow(), func() { sp.maybeReleaseChild(c) })
		return
	}
	sp.releaseChild(c)
}

// releaseChild unregisters and evicts a child's runtime. Idempotent. The
// saved session stays on disk and reopens via the sidebar. An in-flight
// turn (e.g. a send_message reply) is cancelled first so it cannot keep
// running on an evicted runtime.
func (sp *subagentSpawner) releaseChild(c *backgroundChild) {
	sp.children.mu.Lock()
	if c.released {
		sp.children.mu.Unlock()
		return
	}
	c.released = true
	delete(sp.children.children, c.id)
	sp.children.mu.Unlock()
	c.rt.stream.cancelInFlight() // no-op when idle
	c.rt.held.Store(false)
	sp.s.registry.evictRuntime(c.rt)
	// Releasing a child is an explicit teardown of its session: its own
	// children (grandchildren) are cancelled and released in turn.
	sp.s.registry.fireEvictHook(c.id)
}

// cancelAll cancels and releases every live child of parentID. Wired to the
// registry eviction hook so a parent session leaving the registry (close,
// cap eviction, delete, shutdown) takes its children with it — a child
// whose parent is gone cannot deliver report/completion anyway.
func (sp *subagentSpawner) cancelAll(parentID string) {
	sp.children.mu.Lock()
	var victims []*backgroundChild
	for _, c := range sp.children.children {
		if c.parentID == parentID {
			victims = append(victims, c)
		}
	}
	sp.children.mu.Unlock()
	for _, c := range victims {
		c.rt.stream.cancelInFlight() // stop an in-flight turn
		sp.releaseChild(c)
	}
}

// Fork implements agent.ContinuableSubagentSpawner: a foreground child
// seeded with a deep copy of the parent's messages (via ForkMessages, the
// same orphaned-tool-call stripping as the web fork UI) plus the parent's
// model/mode/thinking. The parent transcript is untouched.
func (sp *subagentSpawner) Fork(ctx context.Context, parent *agent.Agent, job string, depth int) (string, error) {
	s := sp.s
	if parent == nil {
		return "", fmt.Errorf("subagent_fork: parent agent is nil")
	}
	parentRt, ok := s.registry.get(parent.SessionID)
	if !ok {
		return "", fmt.Errorf("subagent_fork: parent session is not live")
	}
	forkedMsgs, err := agent.ForkMessages(parent.SnapshotMessages(), "last")
	if err != nil {
		return "", fmt.Errorf("subagent_fork: %w", err)
	}
	mode, thinking := parent.ModeAndThinkingLevel()
	label := "fork: " + parent.SessionLabelSnapshot()
	if job == "" {
		job = "Continue this session from the fork point."
	}
	newID := sesspkg.NewID()
	snap := &agent.SessionSnapshot{
		WorkingDir:    parent.WorkingDir,
		Model:         parent.CurrentModel(),
		Mode:          mode.String(),
		ThinkingLevel: string(thinking),
		Label:         label,
		LabelRenamed:  true,
		Messages:      forkedMsgs,
		ParentID:      parent.SessionID,
	}
	child := s.ws.NewSessionAgent(snap, newID)
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	childRt := newSessionRuntimeWithHold(child, sp.approvalHold())
	childRt.parentID = parent.SessionID
	childRt.nested = true
	childRt.approverOverride = func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		if childRt.clientCount() == 0 {
			return parentRt.deleteApprover()(ctx, req)
		}
		return childRt.deleteApprover()(ctx, req)
	}
	s.registry.register(newID, childRt)
	parentRt.broadcast(WSMessage{
		Type:           "subagent_started",
		SessionID:      parent.SessionID,
		SubagentID:     newID,
		SubagentLabel:  label,
		SubagentJob:    truncateJob(job),
		SubagentParent: parent.SessionID,
	})

	report, err := sp.runChildTurn(ctx, childRt, job)
	success := err == nil
	s.registry.remove(newID)
	childRt.broadcast(WSMessage{Type: "session_detached", SessionID: newID})
	parentRt.broadcast(WSMessage{
		Type:            "subagent_finished",
		SessionID:       parent.SessionID,
		SubagentID:      newID,
		SubagentLabel:   label,
		SubagentParent:  parent.SessionID,
		SubagentSuccess: success,
		SubagentSummary: truncateReport(report, err),
	})
	if err != nil {
		return "", fmt.Errorf("subagent_fork %s: %w", newID, err)
	}
	return report, nil
}

// ListAgents implements agent.ContinuableSubagentSpawner.
func (sp *subagentSpawner) ListAgents(caller *agent.Agent) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("list_agents: caller agent is nil")
	}
	children := sp.children.childrenOf(caller.SessionID)
	if len(children) == 0 {
		return "No live subagents.", nil
	}
	var b strings.Builder
	for _, c := range children {
		fmt.Fprintf(&b, "%s — %s — %s (depth %d)\n", c.id, c.label, c.statusOf(), c.depth)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (c *backgroundChild) statusOf() string {
	c.mu.Lock()
	status := c.status
	c.mu.Unlock()
	if active, _ := c.rt.turnState(); active {
		return "running"
	}
	return status
}

// SendMessage implements agent.ContinuableSubagentSpawner: the message is
// delivered through the child's own delivery service (queued when the child
// is mid-turn); the child's reply is captured by the turn-end hook and
// injected into the parent.
func (sp *subagentSpawner) SendMessage(caller *agent.Agent, agentID, text string) error {
	if caller == nil {
		return fmt.Errorf("send_message: caller agent is nil")
	}
	child := sp.children.get(agentID)
	if child == nil {
		return fmt.Errorf("send_message: subagent %s is not live (finished children are released after their retention window)", agentID)
	}
	if child.parentID != caller.SessionID {
		return fmt.Errorf("send_message: subagent %s is not a child of this session", agentID)
	}
	// The message itself is delivered through the child's delivery service
	// (queued when the child is mid-turn); reply capture is armed by the
	// delivery-start hook when the delivered turn actually begins.
	child.rt.deliverToSession(text)
	return nil
}

// InterruptAgent implements agent.ContinuableSubagentSpawner: cancels the
// child's in-flight turn (no-op when idle); the child stays alive.
func (sp *subagentSpawner) InterruptAgent(caller *agent.Agent, agentID string) error {
	if caller == nil {
		return fmt.Errorf("interrupt_agent: caller agent is nil")
	}
	child := sp.children.get(agentID)
	if child == nil {
		return fmt.Errorf("interrupt_agent: subagent %s is not live", agentID)
	}
	if child.parentID != caller.SessionID {
		return fmt.Errorf("interrupt_agent: subagent %s is not a child of this session", agentID)
	}
	child.rt.stream.cancelInFlight()
	return nil
}

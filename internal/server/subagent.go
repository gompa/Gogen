package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	sesspkg "gogen/internal/session"
)

// subagentSpawner implements agent.SubagentSpawner for the web server:
// children are FULL session runtimes — attachable panes with live streaming
// (D2), registered under their parent, exempt from cap eviction, with delete
// approvals routed to the parent's clients when no child client is attached
// (D6). The child runs with its own turn lock and context window; the parent
// tool call blocks until the child finishes (or the parent turn is
// cancelled, which propagates).
//
// When the spawner also implements agent.ContinuableSubagentSpawner (it
// does — see subagent_continuable.go), the subagent tool gains
// run_in_background and the continuable tools; children then stay
// registered past their main turn (held, retention-bounded) for messaging.
type subagentSpawner struct {
	s *Server

	// children is the continuable-child registry (see
	// subagent_continuable.go). Non-nil for every server spawner.
	children *childRegistry
	// retain bounds how long a finished continuable child stays registered
	// (0 = defaultSubagentRetain). Tests shorten it.
	retain time.Duration
	// maxFinished caps finished continuable children per parent
	// (0 = defaultMaxFinishedSubagents).
	maxFinished int
	// maxLive overrides the user-facing per-parent cap on ACTIVE subagents
	// (0 = the configured subagent_max_concurrent limit — see
	// concurrentLimit).
	maxLive int
	// maxLiveGuard overrides the internal per-parent cap on non-finished
	// children (0 = defaultMaxLiveSubagents — see liveGuardLimit).
	maxLiveGuard int
}

// truncateReport bounds the tool result returned to the parent agent.
func truncateReport(report string, err error) string {
	if err != nil {
		return err.Error()
	}
	return agent.TruncateSubagentReport(report)
}

// newChildRuntime creates and registers a nested child runtime for parent:
// fresh session agent (model selection, label, depth, parent link), D6
// approval forwarding, registry registration, and the subagent_started
// broadcast. Returns the child runtime, its label, and the RAW (unwrapped)
// job. Shared by the foreground Spawn and the background SpawnBackground so
// the two cannot drift.
func (sp *subagentSpawner) newChildRuntime(ctx context.Context, parent *agent.Agent, job, model string, depth int) (*sessionRuntime, string, string, error) {
	s := sp.s
	if parent == nil {
		return nil, "", "", fmt.Errorf("subagent: parent agent is nil")
	}
	parentRt, ok := s.registry.get(parent.SessionID)
	if !ok {
		return nil, "", "", fmt.Errorf("subagent: parent session is not live")
	}

	childID := sesspkg.NewID()
	child := s.ws.NewSessionAgent(nil, childID)
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	runtimeCfg := s.ws.GetRuntimeConfig()
	if model != "" {
		if err := child.SelectModel(ctx, model); err != nil {
			return nil, "", "", fmt.Errorf("subagent model: %w", err)
		}
	} else if m := runtimeCfg.SubagentModel; m != "" {
		// The configured default subagent model (settings modal) beats
		// parent-model inheritance: the user explicitly chose a model for
		// subagents. Selection failures fall back to the workspace default
		// rather than failing the spawn — the default is already on the
		// provider (same fail-open pattern as the inheritance branch).
		if err := child.SelectModel(ctx, m); err != nil {
			log.Printf("subagent: configured subagent model %q not selectable on child (%v); using workspace default", m, err)
		}
	} else if m := parent.CurrentModel(); m != "" && m != s.ws.DefaultModel() {
		// The child's provider was seeded with the workspace DEFAULT model;
		// a parent that switched models per-session (toolbar) should pass
		// its model on. Selection failures fall back to the default rather
		// than failing the spawn — the default is already on the provider.
		if err := child.SelectModel(ctx, m); err != nil {
			log.Printf("subagent: parent model %q not selectable on child (%v); using workspace default", m, err)
		}
	}
	// The reasoning effort follows the same cascade as the model: the
	// configured subagent level (settings) wins; empty = inherit the
	// parent's live level. Applied AFTER the model cascade so validity is
	// resolved against the child's final model (a level it does not accept
	// is omitted — see ApplySubagentThinkingLevel).
	agent.ApplySubagentThinkingLevel(child, parent, runtimeCfg.SubagentThinkingLevel)
	label := agent.SubagentLabel(job)
	child.RenameSession(label)
	// The job wrapper applies AFTER label derivation: labels and the
	// subagent_started event keep the original job the parent wrote; only
	// the child's first message carries the wrapped job.
	rawJob := job

	childRt := newSessionRuntimeWithHold(child, sp.approvalHold())
	childRt.parentID = parent.SessionID
	childRt.nested = true
	// D6: delete approvals go to the child's own attached clients (the
	// child pane shows the modal); with none attached they route to the
	// parent's clients so a headless child can never hang an approval.
	childRt.approverOverride = func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		if childRt.clientCount() == 0 {
			return parentRt.deleteApprover()(ctx, req)
		}
		return childRt.deleteApprover()(ctx, req)
	}

	// Register BEFORE the turn so attach/cancel/approvals resolve. Nested
	// runtimes are cap-exempt, so register never evicts.
	s.registry.register(childID, childRt)

	// The child-scoped report tool delivers progress messages into the live
	// parent session via the delivery service (queued when the parent is
	// busy). Installed on every child — foreground and background — so the
	// tool is exposed for the child's whole lifetime (llmTools gates it on
	// ParentID + hook).
	child.SetReportHook(func(text string) error {
		// Delivered immediately when the parent is live; queued for its
		// next registration otherwise — a progress report must not be lost
		// to a parent that merely went idle with no viewers.
		s.registry.deliverToParent(parent.SessionID, fmt.Sprintf("[subagent %s] %s", label, text))
		return nil
	})

	parentRt.broadcast(WSMessage{
		Type:           "subagent_started",
		SessionID:      parent.SessionID,
		SubagentID:     childID,
		SubagentLabel:  label,
		SubagentJob:    truncateJob(rawJob),
		SubagentParent: parent.SessionID,
	})
	return childRt, label, rawJob, nil
}

func (sp *subagentSpawner) Spawn(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	s := sp.s
	// The foreground child holds a slot of the concurrent limit for the
	// whole turn, exactly like a background child: acquireForeground
	// reserves the slot atomically with the cap check (active children
	// against the user-facing limit, non-finished against the internal
	// guard), and the deferred release frees it on every exit path.
	ok, guardHit := sp.children.acquireForeground(parent.SessionID, sp.concurrentLimit(), sp.liveGuardLimit())
	if !ok {
		return "", spawnCapError(sp.concurrentLimit(), guardHit)
	}
	defer sp.children.releaseForeground(parent.SessionID)
	childRt, label, rawJob, err := sp.newChildRuntime(ctx, parent, job, model, depth)
	if err != nil {
		return "", err
	}
	childID := childRt.agent.SessionID
	parentRt, _ := s.registry.get(parent.SessionID) // still live: registered above

	report, err := sp.runChildTurn(ctx, childRt, agent.FormatSubagentJob(rawJob))
	success := err == nil

	// Persist the final outcome on the child's snapshot so the sidebar can
	// render the true result after a reload/restart (the subagent_started/
	// finished events are not replayed to connecting clients). The turn's
	// own flush already wrote the transcript; this full flush adds the
	// outcome fields (persistMu serializes against any tail of the turn's
	// write).
	status := "success"
	if !success {
		status = "failed"
	}
	childRt.agent.SetSubagentOutcome(status, truncateReport(report, err))
	childRt.agent.FlushSession()
	// A concurrent DELETE of the parent cascades the child's file away;
	// if that delete won the race against the flush above, the write
	// resurrected the child. Detect it (runtime evicted as part of the
	// cascade + parent's file gone) and remove the orphan so it cannot
	// linger invisibly on disk (excluded from the flat list/prune/latest).
	if childRt.evicted.Load() && s.ws.Store.Info(s.ws.GetWorkingDir(), parent.SessionID) == nil {
		_ = s.ws.Store.Delete(s.ws.GetWorkingDir(), childID)
	}

	// The child is persisted by its turn (doPersist); unregistering keeps
	// the registry clean — the saved session stays attachable via the
	// sidebar row (ensureSessionRuntime reloads it).
	s.registry.remove(childID)
	// Attached child panes must close: the runtime is gone, and a message
	// sent to it would be silently dropped (the evicted guard). Same
	// notification as the eviction path — the session stays in the saved
	// list and reopens from the store.
	childRt.broadcast(WSMessage{Type: "session_detached", SessionID: childID})
	parentRt.broadcast(WSMessage{
		Type:            "subagent_finished",
		SessionID:       parent.SessionID,
		SubagentID:      childID,
		SubagentLabel:   label,
		SubagentParent:  parent.SessionID,
		SubagentSuccess: success,
		SubagentSummary: truncateReport(report, err),
	})
	if err != nil {
		return "", fmt.Errorf("subagent %s: %w", childID, err)
	}
	return report, nil
}

// approvalHold returns the configured delete-approval hold window. Reads
// the live runtime overlay (like newSessionRuntimeFor), so a settings-modal
// web_approval_hold_secs change applies to children spawned afterwards.
func (sp *subagentSpawner) approvalHold() time.Duration {
	if sp.s != nil {
		return sp.s.ws.ApprovalHold()
	}
	return 0
}

// runChildTurn runs one full agent turn on the child runtime: the child's
// stream handles are registered so the Cancel button on an attached child
// pane works, events broadcast to the child's clients (live transcript when
// attached), and the parent context propagates cancellation. The turn
// skeleton itself (evicted check, turn-active lifecycle, stream handlers,
// error tail) is shared with startTurn via runTurnBody. The child's own
// turnMu serializes against attach/approval handlers.
func (sp *subagentSpawner) runChildTurn(parentCtx context.Context, rt *sessionRuntime, job string) (string, error) {
	streamCtx, streamCancel := context.WithCancel(context.Background())
	// Parent cancellation kills the child: the parent turn owns this tool
	// call, so if it dies (user Cancel on the parent pane, shutdown) the
	// child must unwind too.
	go func() {
		select {
		case <-parentCtx.Done():
			streamCancel()
		case <-streamCtx.Done():
		}
	}()
	errCh := rt.stream.begin(streamCancel)

	type turnResult struct {
		out string
		err error
	}
	resCh := make(chan turnResult, 1)
	go func() {
		defer rt.turnMu.Unlock()
		defer rt.stream.end()
		defer func() { errCh <- nil }()
		rt.turnMu.Lock()
		// Child turn options: no owner connection, no positioned token
		// frames, no error log/hook (the parent sees the error as the tool
		// result), no persist warning/usage frame (the spawner flushes the
		// outcome).
		out, err := rt.runTurnBody(streamCtx, job, nil, turnOpts{})
		if errors.Is(err, errTurnEvicted) {
			// The runtime was released while we waited for the lock
			// (close/delete/release evict without holding turnMu): nothing
			// to stream to. The defers above still run, so the stream's
			// cancel handle is cleared (begin already ran) and
			// cancelInFlight's errCh wait is released instead of blocking
			// until the drain timeout. The caller's evicted check handles
			// the rest.
			err = fmt.Errorf("child runtime was released while starting")
		}
		resCh <- turnResult{out: out, err: err}
	}()

	select {
	case res := <-resCh:
		// The turn is over (natural completion or child error): cancel the
		// stream context so the parent-cancel watcher goroutine above
		// terminates. With a background child parentCtx is
		// context.Background() and never fires, so without this the
		// watcher leaks for every child that finishes naturally.
		streamCancel()
		// A cancelled parent turn surfaces as a child error; translate it
		// so the parent agent sees a clear reason.
		if parentCtx.Err() != nil && res.err == nil {
			return res.out, parentCtx.Err()
		}
		return res.out, res.err
	case <-parentCtx.Done():
		streamCancel()
		<-resCh // let the child unwind (bounded by its own turn)
		return "", parentCtx.Err()
	}
}

func truncateJob(job string) string {
	return contextmgr.TruncateMarked(job, 200, "…")
}

package server

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"gogen/internal/agent"
	sesspkg "gogen/internal/session"
	"gogen/internal/streamutil"
)

// subagentSpawner implements agent.SubagentSpawner for the web server:
// children are FULL session runtimes — attachable panes with live streaming
// (D2), registered under their parent, exempt from cap eviction, with delete
// approvals routed to the parent's clients when no child client is attached
// (D6). The child runs with its own turn lock and context window; the parent
// tool call blocks until the child finishes (or the parent turn is
// cancelled, which propagates).
type subagentSpawner struct {
	s *Server
}

// truncateReport bounds the tool result returned to the parent agent.
func truncateReport(report string, err error) string {
	if err != nil {
		return err.Error()
	}
	return agent.TruncateSubagentReport(report)
}

func (sp *subagentSpawner) Spawn(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	s := sp.s
	if parent == nil {
		return "", fmt.Errorf("subagent: parent agent is nil")
	}
	parentRt, ok := s.registry.get(parent.SessionID)
	if !ok {
		return "", fmt.Errorf("subagent: parent session is not live")
	}

	childID := sesspkg.NewID()
	child := s.ws.NewSessionAgent(nil, childID)
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	if model != "" {
		if err := child.SelectModel(ctx, model); err != nil {
			return "", fmt.Errorf("subagent model: %w", err)
		}
	} else if m := s.ws.GetRuntimeConfig().SubagentModel; m != "" {
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
	label := agent.SubagentLabel(job)
	child.RenameSession(label)
	// The job wrapper applies AFTER label derivation: labels and the
	// subagent_started event keep the original job the parent wrote; only
	// the child's first message carries the wrapped job.
	rawJob := job
	job = agent.FormatSubagentJob(job)

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

	parentRt.broadcast(WSMessage{
		Type:           "subagent_started",
		SessionID:      parent.SessionID,
		SubagentID:     childID,
		SubagentLabel:  label,
		SubagentJob:    truncateJob(rawJob),
		SubagentParent: parent.SessionID,
	})

	report, err := sp.runChildTurn(ctx, childRt, job)
	success := err == nil

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
// attached), and the parent context propagates cancellation. The child's
// own turnMu serializes against attach/approval handlers.
func (sp *subagentSpawner) runChildTurn(parentCtx context.Context, rt *sessionRuntime, job string) (string, error) {
	child := rt.agent
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
	rt.setTurnActive(true, time.Now(), nil)

	write := func(v WSMessage) {
		if streamCtx.Err() != nil {
			return
		}
		if v.SessionID == "" {
			v.SessionID = child.SessionID
		}
		rt.broadcast(v)
	}
	tokens := streamutil.NewTokenBatcher(func(think bool, text string) {
		if think {
			write(WSMessage{Type: "thinking_token", Content: text})
		} else {
			write(WSMessage{Type: "stream", Content: text})
		}
	}, wsTokenFlushInterval)
	var termMu sync.Mutex
	termBatches := map[string]*streamutil.TokenBatcher{}
	termOpened := map[string]struct{}{}
	handlers := rt.buildStreamHandlers(streamCtx, write, tokens, &termMu, termBatches, termOpened)

	type turnResult struct {
		out string
		err error
	}
	resCh := make(chan turnResult, 1)
	go func() {
		defer rt.turnMu.Unlock()
		rt.turnMu.Lock()
		defer rt.stream.end()
		defer rt.setTurnActive(false, time.Time{}, nil)
		defer func() { errCh <- nil }()
		// Terminal frame for the child's OWN attached clients (a pane
		// opened to watch the subagent): without turn_end the pane stays
		// stuck in the "responding"/busy state forever — the client only
		// clears it on cancelled/turn_end/session_state, and the spawner
		// unregisters the runtime right after this returns. Mirrors the
		// normal startTurn tail (broadcast while the lock is still held).
		defer rt.broadcast(WSMessage{Type: "turn_end", SessionID: child.SessionID})
		appCtx := agent.ContextWithDeleteApprover(streamCtx, rt.approverOverride)
		out, err := child.StreamProcessInputWithImages(appCtx, job, nil, handlers)
		if err != nil {
			if streamCtx.Err() != nil {
				// The child was cancelled (child-pane Cancel or parent turn
				// cancel): broadcast directly (write early-returns on a
				// cancelled ctx), like the normal turn's cancel tail.
				tokens.Flush()
				rt.broadcast(WSMessage{Type: "cancelled", Content: "Cancelled.", SessionID: child.SessionID})
			} else {
				// Real child error: report it in the child's transcript so
				// a watching pane sees why the turn ended, like startTurn.
				tokens.Flush()
				write(WSMessage{Type: "stream_end"})
				write(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
			}
		}
		resCh <- turnResult{out: out, err: err}
	}()

	select {
	case res := <-resCh:
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
	if len(job) > 200 {
		return job[:200] + "…"
	}
	return job
}

package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/streamutil"
)

// handleWSCompact runs the manual compaction command (/compact) for a web
// client. Unlike a regular message it never reaches the LLM as a prompt: it
// cancels any in-flight turn, acquires the session turn lock, emits a
// "compacting" event so the client can show a persistent progress indicator,
// runs CompactHistory (which may take a while — it summarizes the middle via
// the provider), then reports the result and refreshed context stats. Runs in
// a goroutine so the slow summarization does not block the WS read loop.
func (s *Server) handleWSCompact(ws *wsConn, r *http.Request, rt *sessionRuntime) {
	if !rt.acquireTurnForHandler(ws) {
		// /compact is a typed chat command: the busy rejection is its
		// reply on the conversation channel.
		_ = ws.writeJSON(WSMessage{Type: "response", Content: errAgentBusy})
		return
	}
	// Mark the session as "busy" for the compaction duration so a
	// reconnecting client's session_state shows "resuming…" instead of
	// "idle" while the summarization runs, and so session listings/other
	// connections see an in-flight turn (mirrors startTurn). The owner is
	// this connection; its cancel can interrupt nothing here (no LLM stream
	// is running), but the state must be cleared BEFORE the lock is released
	// so the next turn never sees a stale turnActive/turnOwner.
	rt.setTurnActive(true, time.Now(), ws)
	go func() {
		// Orphan check runs LAST (after turnMu.Unlock): if the only client
		// left mid-compact, the idle runtime goes back to the saved list.
		defer rt.evictOrphanedIfPossible()
		defer rt.setTurnActive(false, time.Time{}, nil)
		defer rt.turnMu.Unlock()
		_ = ws.writeJSON(WSMessage{Type: "compacting", SessionID: rt.agent.SessionID})
		if err := rt.agent.CompactHistory(r.Context()); err != nil {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: " + err.Error(), SessionID: rt.agent.SessionID})
		} else {
			rt.agent.FlushSession()
			_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("History compacted (%d messages remaining).", rt.agent.MessageCount()), SessionID: rt.agent.SessionID})
		}
		_ = ws.writeJSON(contextMsg(r.Context(), rt.agent))
	}()
}

func (s *Server) handleWSUserMessage(ws *wsConn, r *http.Request, pane **sessionRuntime, msg WSMessage) {
	rt := *pane
	images, handled := s.preprocessWSUserMessage(ws, r, rt, msg)
	if handled {
		return
	}

	// The turn lock is held across the whole command dispatch below, exactly
	// like the old global turnMu: tryAcquireTurn acquires it, each handled
	// branch releases it before returning, and the unhandled fall-through
	// hands it to rt.startTurn's goroutine, which defers the unlock.
	//
	// Selecting a model resolves the selector against the provider catalog,
	// which performs network I/O on first use. Pre-fetch it before taking the
	// turn lock so the /models <sel> branch below (HandleModelsCommand →
	// SelectModel) only touches the in-memory cache.
	if sel, isModels := agent.ParseModelsCommand(msg.Content); isModels && sel != "" {
		_, _ = rt.agent.ListModels(r.Context())
	}
	if !rt.acquireTurnForHandler(ws) {
		// Busy rejection on the CONVERSATION channel: the user typed a chat
		// message (or a chat command) and the error is its reply.
		_ = ws.writeJSON(WSMessage{Type: "response", Content: errAgentBusy})
		return
	}

	a := rt.agent
	modeOut, modeHandled := a.HandleModeCommand(msg.Content)
	if modeHandled {
		modeCfg := agentConfigMsgBasic(a)
		s.decorateConfig(&modeCfg)
		rt.turnMu.Unlock()
		// Tokenization + echo off the read loop (large uncached sessions
		// take seconds; the read loop serializes every message).
		go func(cfg WSMessage, out string) {
			accum := a.SnapshotUsageAccum()
			applyContextStats(&cfg, a.ContextStats(r.Context()), &accum)
			_ = ws.writeJSON(cfg)
			_ = ws.writeJSON(WSMessage{Type: "response", Content: out})
		}(modeCfg, modeOut)
		return
	}

	thinkOut, thinkHandled := a.HandleThinkingCommand(msg.Content)
	if thinkHandled {
		rt.turnMu.Unlock()
		go func(out string) {
			cfg := agentConfigMsg(r.Context(), rt)
			_, thinking := a.ModeAndThinkingLevel()
			cfg.ThinkingLevel = string(thinking)
			s.decorateConfig(&cfg)
			_ = ws.writeJSON(cfg)
			_ = ws.writeJSON(WSMessage{Type: "response", Content: out})
		}(thinkOut)
		return
	}

	ctxOut, ctxHandled := a.HandleContextCommand(r.Context(), msg.Content)
	if ctxHandled {
		rt.turnMu.Unlock()
		go func(out string) {
			ctxMsg := contextMsg(r.Context(), a)
			_ = ws.writeJSON(ctxMsg)
			_ = ws.writeJSON(WSMessage{Type: "response", Content: out})
		}(ctxOut)
		return
	}

	// Session slash commands (/new, /resume, /sessions, /fork, resume del)
	// route through the registry instead of mutating the agent.
	sessResult, sessHandled, sessErr := s.runSessionCommand(r.Context(), ws, pane, msg.Content)
	if sessHandled {
		rt.turnMu.Unlock()
		s.writeSessionCommandResult(ws, r.Context(), *pane, sessResult, sessErr)
		return
	}

	if sel, isModels := agent.ParseModelsCommand(msg.Content); isModels && sel != "" {
		out, _, modelErr := a.HandleModelsCommand(r.Context(), msg.Content)
		cfg := agentConfigMsgBasic(a)
		s.decorateConfig(&cfg)
		rt.turnMu.Unlock()
		// Echo off the read loop (tokenization can take seconds on a large
		// uncached session; the read loop serializes every message). Both
		// cfg (success only) and resp are written from the goroutine so
		// their relative order is preserved via the send-queue FIFO.
		go func(out string, modelErr error, cfg WSMessage) {
			fillModelPricing(a, &cfg)
			accum := a.SnapshotUsageAccum()
			applyContextStats(&cfg, a.ContextStats(r.Context()), &accum)
			resp := WSMessage{Type: "response", Content: out}
			if modelErr != nil {
				resp.Content = fmt.Sprintf("Error: %v", modelErr)
			} else {
				resp.Model = cfg.Model
				resp.ContextLimit = cfg.ContextLimit
				resp.UsedTokens = cfg.UsedTokens
				resp.UsedSource = cfg.UsedSource
				resp.UsedPercent = cfg.UsedPercent
				_ = ws.writeJSON(cfg)
			}
			_ = ws.writeJSON(resp)
		}(out, modelErr, cfg)
		return
	}

	rt.startTurn(ws, msg.Content, images)
}

// preprocessWSUserMessage validates user-attached images, applies the
// interrupt semantics, and routes the commands that never need the turn
// lock (a literal /compact, /help, and a bare /models list). Returns the
// validated images (for the turn fall-through) and whether the message was
// fully handled.
func (s *Server) preprocessWSUserMessage(ws *wsConn, r *http.Request, rt *sessionRuntime, msg WSMessage) ([]llm.ImageInput, bool) {
	// Validate user-attached images first: a malformed image frame must be
	// rejected without cancelling an in-flight turn or taking the turn lock.
	images, err := validateImageInputs(msg.Images)
	if err != nil {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: " + err.Error()})
		return nil, true
	}
	// Interrupt semantics apply only to the connection that owns the current
	// turn; a second connection's message must not cancel a turn it does not
	// own — it gets the busy rejection below.
	if rt.ownsTurn(ws) {
		rt.stream.cancelInFlight()
	}

	// A literal /compact typed into the composer (or sent by older clients)
	// routes to the real compact command instead of reaching the LLM as a
	// prompt. /compact is registered TUI-only, but the web banner and command
	// palette rely on this path.
	if strings.TrimSpace(msg.Content) == "/compact" {
		s.handleWSCompact(ws, r, rt)
		return nil, true
	}

	if out, handled := agent.HandleHelpCommand(msg.Content, true, false); handled {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: out})
		return nil, true
	}

	if sel, isModels := agent.ParseModelsCommand(msg.Content); isModels && sel == "" {
		go func(content string) {
			a := rt.agent
			out, _, err := a.HandleModelsCommand(r.Context(), content)
			resp := WSMessage{Type: "response", Content: out}
			if err != nil {
				resp.Content = fmt.Sprintf("Error: %v", err)
				_ = ws.writeJSON(resp)
				return
			}
			if models, listErr := a.ListModels(r.Context()); listErr == nil && len(models) > 1 {
				resp.Type = "models"
				resp.Models = s.modelEntries(models)
			}
			cfg := agentConfigMsg(r.Context(), rt)
			resp.Model = cfg.Model
			resp.ContextLimit = cfg.ContextLimit
			resp.UsedTokens = cfg.UsedTokens
			resp.UsedSource = cfg.UsedSource
			resp.UsedPercent = cfg.UsedPercent
			_ = ws.writeJSON(resp)
		}(msg.Content)
		return nil, true
	}
	return images, false
}

// errAgentBusy is the rejection sent when a handler cannot acquire the
// session turn lock because another client's turn is still running. Each
// caller emits it on its OWN channel: the chat path (handleWSUserMessage,
// handleWSCompact) writes it as a "response" (conversation channel — the
// error is the reply to a typed message), while UI-channel handlers
// (set_model / set_mode / set_thinking_level / board start) write it as a
// notice — per the message-type contract, UI errors must never render into
// the chat transcript.
const errAgentBusy = "Error: agent is busy with another client"

// startTurn begins a streaming turn owned by the session runtime. The caller
// (the connection read loop) must already hold rt.turnMu; the goroutine
// defers the unlock. The turn context derives from context.Background() plus
// the runtime's own cancel handles — NOT the HTTP request context, which is
// cancelled the moment HandleWS returns and would silently abort the headless
// turn (§4, third kill path). owner is the connection that started the turn
// (the only one allowed to interrupt it via the cancel-then-lock path, E29).
func (rt *sessionRuntime) startTurn(owner *wsConn, content string, images []llm.ImageInput) {
	streamCtx, streamCancel := context.WithCancel(context.Background())
	errCh := rt.stream.begin(streamCancel)
	go func(content string, images []llm.ImageInput, turnCtx context.Context, done chan error) {
		// Defers run LIFO, so the cleanup executes in this order: turn_end
		// broadcast → setTurnActive(false) (both inside runTurnBody, while
		// the lock is still held) → done → stream.end() → turnMu.Unlock().
		// The turn state must be cleared BEFORE the lock is released: a new
		// turn can only start once the lock is free, and it must never see
		// (or clobber) a stale turnActive/turnOwner from the turn it
		// replaced. turn_end is broadcast while the lock is still held so a
		// new turn's first events can never interleave ahead of it.
		// stream.end() must also run before the lock is released: it clears
		// the runtime's shared stream handles, and the next turn's begin()
		// runs as soon as the lock is free. If end() ran after unlock, a
		// fast consecutive turn would have its freshly registered cancel
		// handles wiped by the previous turn's end() — losing the ability
		// to cancel the new turn.
		// The orphan check runs LAST (after turnMu.Unlock): a headless turn
		// that finishes with zero attached clients leaves an idle runtime
		// nobody is viewing — evict it so it reads as a plain saved session.
		defer rt.evictOrphanedIfPossible()
		defer rt.turnMu.Unlock()
		defer rt.stream.end()
		defer func() { done <- nil }()
		// runTurnBody owns the evicted check (an evicted runtime is a clean
		// no-op turn: the defers above still signal errCh, clear the stream
		// handles, and release the lock) and the turn-active lifecycle.
		rt.runTurnBody(turnCtx, content, images, turnOpts{
			owner:        owner,
			tagPositions: true,
			reportErr:    true,
			persist:      true,
		})
	}(content, images, streamCtx, errCh)
}

// errTurnEvicted is returned by runTurnBody when the runtime was evicted
// before the turn could start. Callers map it to their own failure mode:
// web turns are a silent no-op, child turns report it to the parent.
var errTurnEvicted = errors.New("turn evicted before start")

// turnOpts captures the per-caller differences between the two entry
// points that share runTurnBody: the web turn (startTurn) and the
// subagent child turn (runChildTurn).
type turnOpts struct {
	// owner is the connection that started the turn (web turns); nil for
	// child turns.
	owner *wsConn
	// tagPositions stamps the live thinking/content segment positions on
	// token frames (web panes render positioned segments; child panes do
	// not).
	tagPositions bool
	// reportErr logs the turn error and fires rt.turnErrorHook (web turns;
	// child turns surface the error to the parent as the tool result
	// instead).
	reportErr bool
	// persist consumes the agent's persist error and appends the
	// context-usage frame (web turns; child turns persist through
	// doPersist and the spawner's outcome flush).
	persist bool
}

// runTurnBody executes one streaming turn synchronously. It is the shared
// core of startTurn (web turns) and runChildTurn (subagent child turns);
// the callers keep the goroutine, the turn lock, and the stream handles.
//
//	Precondition: the caller holds rt.turnMu and has called
//	rt.stream.begin with ctx's cancel.
//	Postcondition: the turn is no longer active and turn_end has been
//	broadcast (both while the lock is still held).
//
// It returns errTurnEvicted when the runtime was evicted before the turn
// could start; the caller decides how to surface that.
func (rt *sessionRuntime) runTurnBody(ctx context.Context, content string, images []llm.ImageInput, o turnOpts) (string, error) {
	// The runtime may have been evicted while the caller waited for the
	// turn lock: close/delete/cap eviction proceed WITHOUT turnMu when
	// the lock is held (the stuck-turn path), so an eviction can land
	// after the caller's own evicted check. Starting the turn would
	// stream into the void and persist a message into a torn-down
	// session (the delivery worker's pop-then-start handoff is exactly
	// this window). The caller's defers still run, so the early return is
	// a clean no-op turn: turn state is reset, errCh is signaled, the
	// stream handles are cleared, and the lock is released.
	if rt.evicted.Load() {
		return "", errTurnEvicted
	}
	// The turn is published as active ONLY while holding the lock: a
	// window with turnActive=true but turnMu free would let a
	// delivery/attach turn start concurrently and clobber the active flag
	// (or run ahead of the turn's own work). Both defers run while the
	// lock is still held: turn_end first, so a new turn's first events can
	// never interleave ahead of it, then the state reset, so the next turn
	// never sees a stale turnActive/turnOwner.
	defer rt.setTurnActive(false, time.Time{}, nil)
	defer rt.broadcast(WSMessage{Type: "turn_end", SessionID: rt.agent.SessionID})
	rt.setTurnActive(true, time.Now(), o.owner)
	// An installed approverOverride (subagent D6 forwarding, board start
	// deny-when-unattended) replaces the default per-session approver.
	approver := rt.deleteApprover()
	if rt.approverOverride != nil {
		approver = rt.approverOverride
	}
	appCtx := agent.ContextWithDeleteApprover(ctx, approver)
	// write fans out to every attached socket and tags the source
	// sessionId. A write failure detaches that socket (broadcast does it);
	// it NEVER cancels the LLM call — the turn belongs to the session and
	// keeps running headless (§4).
	write := func(v WSMessage) {
		if appCtx.Err() != nil {
			return
		}
		if v.SessionID == "" {
			v.SessionID = rt.agent.SessionID
		}
		rt.broadcast(v)
	}
	tokens := streamutil.NewTokenBatcher(func(think bool, text string) {
		if think {
			msg := WSMessage{Type: "thinking_token", Content: text}
			if o.tagPositions {
				msg.ThinkingPos = rt.liveThinkingSegmentEnd(text)
			}
			write(msg)
		} else {
			msg := WSMessage{Type: "stream", Content: text}
			if o.tagPositions {
				msg.ContentPos = rt.liveContentSegmentEnd(text)
			}
			write(msg)
		}
	}, wsTokenFlushInterval)

	// Tool-call argument deltas: the highest-rate callback in a
	// tool-heavy turn (a patch_file diff arrives as hundreds of ~1KB
	// fragments), so they get the same coalescing as content tokens
	// instead of one WebSocket frame per provider chunk. Each flushed
	// segment is stamped with the end offset of the text it carries
	// (liveToolArgsSegmentEnd), not the buffer length — the client's
	// trimToEnd merge relies on endPos - text.length matching the
	// previous endPos exactly.
	argsBatch := streamutil.NewArgsBatcher(func(index int, id, name, text string) {
		write(WSMessage{
			Type:       "tool_call_delta",
			Tool:       name,
			ToolCallID: id,
			Index:      index,
			ArgsDelta:  text,
			ArgsPos:    rt.liveToolArgsSegmentEnd(index, text),
		})
	}, wsTokenFlushInterval)

	// Live terminal tabs for shell tools: a tab is opened lazily on the
	// first output chunk (which carries the exact command string), fed by
	// a per-tool batcher, and closed on tool end. Both maps are keyed by
	// the tool call ID, which doubles as the terminal tab ID. They are
	// accessed from the exec pipe goroutine (OnToolOutput) and the stream
	// goroutine (OnToolResult), so access is mutex-guarded.
	var termMu sync.Mutex
	termBatches := map[string]*streamutil.TokenBatcher{}
	termOpened := map[string]struct{}{}
	// Shared rate meter (same type, interval, and estimator as the TUI):
	// the builder feeds it every content/thinking/tool-args delta and
	// emits stream_stats frames a few times per second while the round
	// streams.
	speed := streamutil.NewSpeedMeter(streamutil.StatsInterval)

	handlers := rt.buildStreamHandlers(appCtx, write, tokens, argsBatch, speed, &termMu, termBatches, termOpened)

	out, err := rt.agent.StreamProcessInputWithImages(appCtx, content, images, handlers)
	if err != nil {
		if appCtx.Err() != nil {
			tokens.Flush()
			argsBatch.Flush()
			// Broadcast directly (not via write, which early-returns on a
			// cancelled ctx) so the cancellation reaches attached clients.
			rt.broadcast(WSMessage{Type: "cancelled", Content: "Cancelled.", SessionID: rt.agent.SessionID})
			return out, err
		}
		tokens.Flush()
		argsBatch.Flush()
		write(WSMessage{Type: "stream_end"})
		write(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
		if o.reportErr {
			log.Printf("stream error: %v", err)
			if rt.turnErrorHook != nil {
				rt.turnErrorHook(err)
			}
		}
		return out, err
	}
	var persistErr error
	var ctxMsg WSMessage
	if o.persist {
		// No turnMu re-acquire here: the caller holds the session turn
		// lock for the whole turn. ConsumePersistError is internally
		// synchronized (persistMu), so it is safe even when a
		// shutdown/delete/eviction flush runs concurrently without turnMu.
		persistErr = rt.agent.ConsumePersistError()
		// context.Background() rather than the (dead) request context:
		// the turn outlives the connection (§4).
		ctxMsg = contextMsg(context.Background(), rt.agent)
	}
	if persistErr != nil {
		write(WSMessage{Type: "response", Content: fmt.Sprintf("Warning: failed to save session: %v", persistErr)})
	}
	tokens.Flush()
	argsBatch.Flush()
	// No trailing stream_end here: every round already wrote one via the
	// OnStreamEnd handler above, and turn_end marks the turn boundary.
	if ctxMsg.Type != "" {
		write(ctxMsg)
	}
	return out, nil
}

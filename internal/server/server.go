package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	sesspkg "gogen/internal/session"
	"gogen/internal/streamutil"

	"github.com/gorilla/websocket"
)

// spawnUserTerminal starts the interactive user shell for a WebSocket
// connection (if none is alive) and reports its lifecycle over the socket:
// user_term_opened once the shell is up, user_term_exit when it exits. Output
// is streamed as user_term_output chunks from the PTY read goroutine. The
// shell runs in the workspace's current working directory at spawn time (the
// executor has its own mutex, so no turn lock is needed).
func (s *Server) spawnUserTerminal(ws *wsConn, holder *userTermHolder) {
	if holder.get() != nil {
		return
	}
	wd := s.ws.Exec.GetWorkingDir()
	ut, err := startUserTerminal(wd, func(chunk string) {
		_ = ws.writeJSON(WSMessage{Type: "user_term_output", Content: chunk})
	})
	if err != nil {
		_ = ws.writeJSON(WSMessage{Type: "user_term_exit", Content: "failed to start shell: " + err.Error(), Code: -1})
		return
	}
	// Claim the holder atomically. Two concurrent requests (user_term_request
	// while an earlier spawn is in flight) would otherwise both spawn and the
	// loser would leak a pty; trySet fails only when a terminal is already
	// held, and the loser is closed here.
	if !holder.trySet(ut) {
		_ = ut.Close()
		return
	}
	_ = ws.writeJSON(WSMessage{Type: "user_term_opened", Content: ut.Title(), WorkingDir: wd})
	go func() {
		<-ut.Done()
		code := ut.ExitCode()
		// Only report the exit if this terminal is still the connection's
		// current one (a respawn may already have replaced it).
		if holder.clear(ut) {
			_ = ws.writeJSON(WSMessage{Type: "user_term_exit", Content: fmt.Sprintf("shell exited (%d)", code), Code: code})
		}
	}()
}

type Server struct {
	// ws is the shared workspace (executor, store, config, provider factory,
	// fsMu). registry owns the live sessions (multi-session plan §2).
	ws             *Workspace
	registry       *sessionRegistry
	config         *config.Config
	allowedOrigins map[string]struct{}
	authToken      string
	tlsCertFile    string
	tlsKeyFile     string
	wsConnsMu      sync.Mutex
	wsConns        []*websocket.Conn
	connLimiter    *rateLimitState
	upgradeLimiter *ipLimiter
	staticAssets   staticAssetCache // lazily gzip-compressed embedded assets
}

func NewServer(a *agent.Agent, cfg *config.Config) *Server {
	allowed := parseAllowedOrigins("")
	token := ""
	tlsCert, tlsKey := "", ""
	if cfg != nil {
		allowed = parseAllowedOrigins(cfg.WebAllowedOrigins)
		token = strings.TrimSpace(cfg.WebAuthToken)
		tlsCert = strings.TrimSpace(cfg.WebTLSCertFile)
		tlsKey = strings.TrimSpace(cfg.WebTLSKeyFile)
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GOGEN_WEB_TOKEN"))
	}
	if tlsCert == "" {
		tlsCert = strings.TrimSpace(os.Getenv("GOGEN_WEB_TLS_CERT"))
	}
	if tlsKey == "" {
		tlsKey = strings.TrimSpace(os.Getenv("GOGEN_WEB_TLS_KEY"))
	}
	ws := newWorkspaceFromAgent(a, cfg)
	maxActive := 0
	if cfg != nil {
		maxActive = cfg.WebMaxActiveSessions
	}
	reg := newSessionRegistry(maxActive)
	if a.SessionID == "" {
		a.SessionID = sesspkg.NewID()
	}
	hold := time.Duration(0)
	if cfg != nil {
		hold = cfg.ApprovalHold()
	}
	rt := newSessionRuntimeWithHold(a, hold)
	reg.register(a.SessionID, rt)
	// In web mode the registry is the sole pruner: Save's
	// internal auto-prune protects only one session and could delete another
	// live session's file; the registry prunes with the full active set.
	if ws.Store != nil {
		ws.Store.SetAutoPrune(false)
	}
	// Wrap FS-mutating tools with the workspace filesystem lock:
	// a streaming turn no longer blocks editor saves except during the actual
	// mutation window of a tool.
	a.SetToolHandlers(wrapToolHandlers(agent.BuiltinToolHandlers(), &ws.fsMu))
	s := &Server{
		ws:             ws,
		registry:       reg,
		config:         cfg,
		allowedOrigins: allowed,
		authToken:      token,
		tlsCertFile:    tlsCert,
		tlsKeyFile:     tlsKey,
		connLimiter:    newRateLimitState(defaultMaxWSConns),
		upgradeLimiter: newIPLimiter(5, 10), // 5 upgrades/sec/IP, burst 10
	}
	// Background model validation for a restored default session runs after
	// the server starts; push the result to the session's clients so the
	// toolbar does not keep showing a model that was cleared or replaced by
	// the validation.
	a.OnModelChanged = func() { s.pushConfigForAgent(a) }
	return s
}

// newSessionRuntimeFor builds a session runtime carrying the server's
// configured approval-hold window (see web_approval_hold_secs / F2).
func (s *Server) newSessionRuntimeFor(a *agent.Agent) *sessionRuntime {
	hold := time.Duration(0)
	if s.config != nil {
		hold = s.config.ApprovalHold()
	}
	return newSessionRuntimeWithHold(a, hold)
}

func (s *Server) wsUpgrader() websocket.Upgrader {
	allowed := s.allowedOrigins
	return websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return checkWSOrigin(r, allowed)
		},
		// permessage-deflate: the browser negotiates it automatically, and a
		// full-session history payload (multi-MB of repetitive JSON —
		// reasoning blocks, code, tool results) shrinks ~10x on the wire.
		// Go test clients don't request compression by default, so existing
		// tests are unaffected.
		EnableCompression: true,
	}
}

// agentConfigMsgBasic returns config fields that are cheap to read for the
// given session agent. Every field is internally synchronized (executor,
// provider, statsMu for mode/thinking/label), so it is safe WITHOUT the
// session's turnMu — the attach handshake must never block on a running
// turn, which holds turnMu for its entire duration. Do not call ContextStats
// while holding turnMu — tokenize after unlocking via applyContextStats.
func agentConfigMsgBasic(a *agent.Agent) WSMessage {
	mode, thinking := a.ModeAndThinkingLevel()
	msg := WSMessage{
		Type:          "config",
		WorkingDir:    a.Executor.GetWorkingDir(),
		Model:         a.CurrentModel(),
		Mode:          mode.String(),
		ThinkingLevel: string(thinking),
		GlobalMode:    a.GlobalMode,
		SessionID:     a.SessionID,
		SessionLabel:  a.SessionLabelSnapshot(),
	}
	// Reasoning-effort options and description for the current model
	// (in-memory lookups, never block), so the client can render the
	// per-model chips and a hover tooltip.
	msg.ReasoningEfforts = a.CurrentModelEfforts()
	msg.ModelDescription = a.CurrentModelDescription()
	return msg
}

// pushConfigForAgent broadcasts a fresh config snapshot for a session agent
// to its attached clients. Used after background model validation so a
// client that attached before validation completed does not keep showing a
// stale model (one that was cleared or auto-selected by the validation).
// Safe without turnMu: agentConfigMsgBasic is internally synchronized (see
// its contract above).
func (s *Server) pushConfigForAgent(a *agent.Agent) {
	if a == nil {
		return
	}
	if rt, ok := s.registry.get(a.SessionID); ok {
		rt.broadcast(agentConfigMsgBasic(a))
	}
}

// agentConfigMsg is an internally-synchronized basic snapshot plus
// ContextStats applied outside any lock (ContextStats snapshots under its own
// statsMu). No turnMu is taken — callers may hold it (session command echoes)
// or not (the attach handshake, which must never block on a running turn).
func agentConfigMsg(ctx context.Context, rt *sessionRuntime) WSMessage {
	a := rt.agent
	msg := agentConfigMsgBasic(a)
	fillModelPricing(a, &msg)
	accum := a.SnapshotUsageAccum()
	applyContextStats(&msg, a.ContextStats(ctx), &accum)
	return msg
}

// fillModelPricing looks up pricing for the current model from the models.dev
// registry cache (never blocks — pure map lookup).
func fillModelPricing(a *agent.Agent, msg *WSMessage) {
	if p, ok := a.Provider.(*llm.OpenAIProvider); ok && msg.Model != "" {
		if in, out, cached, ok := p.ModelPricing(msg.Model); ok {
			msg.InputPricePer1M = in
			msg.OutputPricePer1M = out
			msg.CachedPricePer1M = cached
		}
	}
}

func sessionEntries(list []agent.SessionInfo, active map[string]bool) []SessionEntry {
	out := make([]SessionEntry, len(list))
	for i, s := range list {
		out[i] = SessionEntry{
			ID:           s.ID,
			UpdatedAt:    s.UpdatedAt,
			MessageCount: s.MessageCount,
			Label:        s.Label,
			Oneshot:      s.Oneshot,
			Active:       active[s.ID],
		}
	}
	return out
}

// activeSet returns the set of registered session ids.
func (r *sessionRegistry) activeSet() map[string]bool {
	ids := r.activeIDs()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func historyEntries(msgs []llm.Message) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(msgs))
	for idx, m := range msgs {
		createdAt := ""
		if !m.CreatedAt.IsZero() {
			createdAt = m.CreatedAt.UTC().Format(time.RFC3339Nano)
		}

		switch m.Role {
		case "user":
			if m.Content == "" {
				// Pure-image messages (no text) are still valid history: they
				// carry their images. Only skip when there is nothing at all.
				if len(m.Images) == 0 {
					continue
				}
			}
			out = append(out, HistoryEntry{Role: m.Role, Content: m.Content, Images: m.Images, Index: idx, CreatedAt: createdAt})
		case "assistant":
			if m.Content == "" && len(m.ToolCalls) == 0 && m.Reasoning == "" && m.Refusal == "" {
				continue
			}
			entry := HistoryEntry{
				Role:      m.Role,
				Content:   m.Content,
				Reasoning: m.Reasoning,
				Refusal:   m.Refusal,
				Model:     m.Model,
				Index:     idx,
				CreatedAt: createdAt,
			}
			if len(m.ToolCalls) > 0 {
				entry.ToolCalls = make([]HistoryToolCall, len(m.ToolCalls))
				for i, tc := range m.ToolCalls {
					entry.ToolCalls[i] = HistoryToolCall{
						ID:   tc.ID,
						Name: tc.Name,
						Args: tc.Args,
					}
				}
			}
			out = append(out, entry)

		case "tool":
			if m.Content == "" && m.ToolCallID == "" {
				continue
			}
			out = append(out, HistoryEntry{
				Role:       m.Role,
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
				Index:      idx,
				CreatedAt:  createdAt,
			})
		}
	}
	return out
}

func (s *Server) writeSessionCommandResult(ws *wsConn, ctx context.Context, rt *sessionRuntime, result agent.SessionCommandResult, err error) {
	a := rt.agent
	resp := WSMessage{Type: "response"}
	clearChat := false
	if err != nil {
		resp.Content = fmt.Sprintf("Error: %v", err)
	} else {
		resp.Content = result.Output
		if result.Action == agent.SessionActionClearChat {
			resp.SessionAction = string(result.Action)
			clearChat = true
		}
	}

	var cfg WSMessage
	var history []llm.Message
	needHistory := clearChat && err == nil && len(result.History) == 0
	// No turnMu here: everything below is already-computed or internally
	// synchronized (sessionEntries reads the registry under its own lock;
	// the config snapshot is lock-free; result.History came from the
	// command). A resume of a session with a RUNNING turn must deliver its
	// reply immediately — the turn holds turnMu for its entire duration,
	// and blocking here is exactly the "can't switch to the responding
	// session until it's done" symptom.
	if err == nil && len(result.Sessions) > 0 {
		resp.Type = "sessions"
		resp.Sessions = sessionEntries(result.Sessions, s.registry.activeSet())
	}
	cfg = agentConfigMsgBasic(a)
	if len(result.History) > 0 {
		history = append([]llm.Message(nil), result.History...)
	}
	if needHistory {
		// /new (and any clear with empty History) — still emit history so
		// clients can reliably run post-session follow-ups (e.g. resend).
		history = a.SnapshotMessages()
	}
	// The sessions payload is connection-scoped sidebar state; leave its
	// SessionID empty so the client routes it to the active pane instead of
	// tying it to one session (which could drop it after a reconnect or a
	// cross-tab default change).
	if resp.Type != "sessions" {
		resp.SessionID = cfg.SessionID
	}
	resp.Mode = cfg.Mode
	// Paint sessions/history before ContextStats tokenization (can be slow on
	// large restored sessions / cold tiktoken init). clear_chat + history
	// carry the sessionId so the client routes them to the right pane.
	_ = ws.writeJSON(resp)
	if clearChat && err == nil {
		_ = ws.writeJSON(WSMessage{Type: "clear_chat", SessionID: cfg.SessionID})
	}
	// Always emit history on a clear (even empty) so clients can reliably
	// run post-session follow-ups (e.g. resend).
	if (clearChat && err == nil) || len(history) > 0 {
		_ = ws.writeJSON(WSMessage{
			Type:         "history",
			History:      historyEntries(history),
			HistoryEpoch: rt.agent.HistoryEpoch(),
			// Same as attach: a resumed session with a running turn carries
			// its in-flight reply here.
			Rewind:    rt.liveRewind(),
			SessionID: cfg.SessionID,
		})
	}
	// Context stats (tokenization) can take seconds on a large uncached
	// session; compute them off the read loop like every other handler so a
	// slow probe cannot block this connection's messages (including cancel).
	// The response/history above were already enqueued, so the send-queue
	// FIFO keeps the ordering stable.
	go func() {
		stats := a.ContextStats(ctx)
		accum := a.SnapshotUsageAccum()
		applyContextStats(&cfg, stats, &accum)
		fillModelPricing(a, &cfg)
		ctxMsg := WSMessage{Type: "context"}
		applyContextStats(&ctxMsg, stats, &accum)
		_ = ws.writeJSON(ctxMsg)
		_ = ws.writeJSON(cfg)
	}()
}

func (s *Server) modelEntries(models []llm.ModelInfo) []ModelEntry {
	out := make([]ModelEntry, len(models))
	for i, m := range models {
		out[i] = ModelEntry{
			ID:               m.ID,
			ContextLimit:     m.ContextLimit,
			Current:          m.Current,
			InputPricePer1M:  m.InputPricePer1M,
			OutputPricePer1M: m.OutputPricePer1M,
			CachedPricePer1M: m.CachedPricePer1M,
			ReasoningEfforts: m.ReasoningEfforts,
			Description:      m.Description,
		}
	}
	return out
}

// upgradedWS bundles the result of a shared WebSocket handshake: the raw
// connection (for ReadJSON in the read loop), the wrapped wsConn (for
// writes), and the cleanup the handler must defer.
type upgradedWS struct {
	conn    *websocket.Conn
	ws      *wsConn
	cleanup func()
}

// errWSUpgrade is returned by upgradeWS after the HTTP error response has
// already been written, so handlers can simply return.
var errWSUpgrade = errors.New("websocket upgrade failed")

// upgradeWS performs the handshake shared by the chat and editor endpoints:
// auth check, upgrade + connection rate limiting (both sockets of one tab
// count against the same global connection limiter), origin check, protocol
// upgrade, connection tracking (for shutdown force-close), read-deadline and
// pong setup, and wrapping in the write-queue wsConn. The returned cleanup
// tears the socket down in the same order HandleWS used: closeSend (lets the
// writer drain its queue) → unregister → raw close → release the connection
// slot. Callers must defer the cleanup.
func (s *Server) upgradeWS(w http.ResponseWriter, r *http.Request) (*upgradedWS, error) {
	if !s.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, errWSUpgrade
	}
	if s.upgradeLimiter != nil && !s.upgradeLimiter.allow(clientIP(r)) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return nil, errWSUpgrade
	}
	release := func() {}
	if s.connLimiter != nil && !s.connLimiter.acquireConn() {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return nil, errWSUpgrade
	}
	release = s.connLimiter.releaseConn
	upg := s.wsUpgrader()
	conn, err := upg.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		release()
		return nil, errWSUpgrade
	}
	s.registerWSConn(conn)
	// Pong handler extends the read deadline whenever the browser replies to
	// our pings. If the client stops responding (tab closed, network gone),
	// the read deadline elapses, ReadJSON fails, and the handler tears down —
	// which closes the write side too. This is what surfaces half-open
	// connections that would otherwise freeze the UI silently.
	if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
		log.Printf("websocket set read deadline: %v", err)
	}
	conn.SetPongHandler(func(string) error {
		if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
			log.Printf("websocket set read deadline: %v", err)
		}
		return nil
	})

	ws := newWSConn(conn)
	cleanup := func() {
		ws.closeSend()
		s.unregisterWSConn(conn)
		_ = conn.Close()
		release()
	}
	return &upgradedWS{conn: conn, ws: ws, cleanup: cleanup}, nil
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	u, err := s.upgradeWS(w, r)
	if err != nil {
		return
	}
	defer u.cleanup()
	conn := u.conn
	ws := u.ws

	// The connection's pane: the session it is currently attached to and the
	// default target for messages without a sessionId. Lifecycle ops
	// (session_new/resume/fork) switch the pane; teardown detaches from
	// whatever the current pane is — WITHOUT cancelling any turn (the turn is
	// owned by the runtime, so disconnecting never kills it, §4).
	pane := s.registry.first()
	if pane == nil {
		// The registry can be empty: every runtime was evicted — the last
		// pane was explicitly closed (session_close) or the last client
		// detached from an idle session (orphan eviction). Bootstrap a
		// default session (latest saved, or a fresh one) so the connection
		// and any legacy id-less message have a target.
		pane = s.createBootstrapSession()
	}
	if pane == nil {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: no session available"})
		return
	}

	// Attach this connection as a viewer of the session.
	s.attachSession(ws, r, pane, true)
	// Teardown detaches the connection from EVERY session it is attached to
	// (the current pane plus any background panes) — WITHOUT cancelling any
	// turn (the turn is owned by the runtime, so disconnecting never kills
	// it, §4). A killed tab cannot send session_detach per pane, and a stale
	// attachment would leak the dead socket and could leave a pending
	// delete-approval hanging forever instead of auto-denying.
	defer s.registry.detachAll(ws)

	// Interactive user shell for this connection, killed on disconnect. The
	// shell itself is spawned after the config/history handshake below so
	// connection setup never depends on pty availability.
	userTermHolder := &userTermHolder{}
	defer func() {
		if ut := userTermHolder.get(); ut != nil {
			ut.Close()
		}
	}()

	// Spawn the user shell after the handshake so a pty failure (sandboxed
	// or headless environments) can never delay the config/history messages.
	s.spawnUserTerminal(ws, userTermHolder)

	incoming := make(chan WSMessage, 8)
	go func() {
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				close(incoming)
				return
			}
			// Complete delete approvals here so they never sit behind a
			// main-loop turnMu.Lock() (the stream holds turnMu while waiting
			// for approval). Route by sessionId so each session's approvals
			// resolve independently.
			if msg.Type == "delete_approval_response" {
				if rt := s.resolveRuntime(msg.SessionID); rt != nil {
					rt.completeApproval(msg.ApprovalID, msg.Approved)
				}
				continue
			}
			incoming <- msg
		}
	}()

	var target *sessionRuntime
	for msg := range incoming {
		target, pane = s.resolveMessageTarget(msg, pane)
		if h, ok := wsHandlers[msg.Type]; ok {
			h(s, ws, r, &pane, target, msg, userTermHolder)
		}
	}
}

// resolveMessageTarget resolves the session runtime a message acts on: the
// id-resolved runtime for explicit ids, or the connection's pane for id-less
// messages. Session-scoped messages WITHOUT an explicit sessionId act on this
// connection's own pane, not the server-global default. The default is only
// the bootstrap fallback for the initial attach (pane := registry.first())
// and is moved by ANY tab's session_attach/session_new (setDefault is
// global), so an id-less cancel/set_mode/session_detach would silently hit
// the WRONG session in a multi-tab setup. The pane is the sender's current
// session, which is what an id-less message means. On first load the pane IS
// the default, so legacy behavior is unchanged. (Approval responses are
// intercepted in the read goroutine and keep default routing — an empty id
// there is a malformed client, and the goroutine must not touch the shared
// pane.) Returns the target and the (possibly re-aligned) pane.
func (s *Server) resolveMessageTarget(msg WSMessage, pane *sessionRuntime) (*sessionRuntime, *sessionRuntime) {
	target := s.resolveRuntime(msg.SessionID)
	if msg.SessionID == "" {
		// The pane pointer can reference a runtime that left the
		// registry while this connection was open — its session was
		// deleted by another tab (session_delete detaches every
		// attached client), or it was cap/orphan-evicted while the
		// pane was open. Routing an id-less message to the evicted
		// runtime would silently drop it (the handlers' evicted
		// guard), so fall back to the default session when one is
		// live. When the registry is EMPTY (this connection closed
		// its only pane via session_close / session_delete and has
		// not re-keyed yet), deliberately do NOT bootstrap: the
		// latest saved session is the one the user just closed, and
		// createBootstrapSession would resurrect it as an active
		// runtime (the sessions payload would show it "active" again
		// and session_close's eviction would appear undone). The
		// stale pane stays in place and the handlers' evicted guard
		// drops the message safely; the client re-keys (session_new /
		// session_attach) and the next explicit-id message re-aligns
		// the pane.
		if pane == nil || pane.evicted.Load() {
			if d := s.registry.first(); d != nil {
				pane = d
			}
		}
		target = pane
	}
	return target, pane
}

// wsMessageHandler dispatches one inbound message type. pane is the
// connection's current session pointer (handlers may re-align it, e.g. the
// fork/edit-resend path), target is the id-resolved runtime (nil for stale
// ids — handlers drop the message), and holder is the connection's
// interactive user terminal (for user_term_*). A handler's return is
// equivalent to the old switch's `continue`: the message is consumed.
type wsMessageHandler func(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder)

// wsHandlers maps inbound message types to their handlers. Unknown types are
// dropped silently, exactly like the switch's implicit default.
var wsHandlers = map[string]wsMessageHandler{
	"session_fork":       wsHandleFork,
	"fs_list":            wsHandleFSRead,
	"fs_read":            wsHandleFSRead,
	"fs_search":          wsHandleFSRead,
	"git_status":         wsHandleFSRead,
	"git_file_diff":      wsHandleFSRead,
	"fs_write":           wsHandleFSWrite,
	"fs_replace":         wsHandleFSWrite,
	"fs_apply_patch":     wsHandleFSWrite,
	"list_sessions":      wsHandleListSessions,
	"session_new":        wsHandleSessionAction,
	"session_resume":     wsHandleSessionAction,
	"session_delete":     wsHandleSessionAction,
	"list_models":        wsHandleListModels,
	"set_model":          wsHandleSetModel,
	"set_mode":           wsHandleSetMode,
	"set_thinking_level": wsHandleSetThinkingLevel,
	"config":             wsHandleConfig,
	"cancel":             wsHandleCancel,
	"session_attach":     wsHandleAttach,
	"session_detach":     wsHandleDetach,
	"session_close":      wsHandleClose,
	"user_term_input":    wsHandleUserTermInput,
	"user_term_resize":   wsHandleUserTermResize,
	"user_term_request":  wsHandleUserTermRequest,
	"compact":            wsHandleCompact,
	"message":            wsHandleMessage,
}

// wsHandleFork handles session_fork: the fork source is the pane named by
// sessionId (edit-resend forks its own pane's session). Re-align the
// connection's pane with the explicit id so a reconnect-stale pointer cannot
// fork the wrong session, and drop the request when the source session is
// gone entirely (never fork a different session).
func wsHandleFork(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if msg.SessionID != "" {
		t := s.resolveRuntime(msg.SessionID)
		if t == nil {
			if *pane != nil && (*pane).agent.SessionID == msg.SessionID {
				t = *pane
			} else {
				return
			}
		}
		*pane = t
	}
	s.handleWSSessionAction(ws, r.Context(), pane, msg)
}

func wsHandleFSRead(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	s.handleFSReadMessage(ws, r.Context(), msg)
}

func wsHandleFSWrite(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	s.handleFSWriteMessage(ws, r.Context(), msg)
}

// wsHandleListSessions lists the saved sessions for the targeted session's
// working directory. An empty registry drops the request (handleWSListSessions
// dereferences rt.agent in a goroutine and a nil runtime would panic the
// whole process); the client re-requests after its next session_new /
// session_attach.
func wsHandleListSessions(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		target = s.registry.first()
	}
	if target == nil {
		return
	}
	s.handleWSListSessions(ws, target)
}

// wsHandleSessionAction handles session_new/session_resume/session_delete.
// session_new creates for the connection's CURRENT pane, but the client's
// edit-resend path scopes it with the acting pane's id (beginResend:
// histIdx == 0 sends session_new with sessionId). Re-align the pane pointer
// to that id (like session_fork does) so a reconnect-stale pointer can never
// replace the WRONG pane's session. session_resume/session_delete are NOT
// re-aligned: their sessionId names the TARGET session, not the acting pane.
func wsHandleSessionAction(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if msg.Type == "session_new" && msg.SessionID != "" {
		if t := s.resolveRuntime(msg.SessionID); t != nil {
			*pane = t
		}
	}
	s.handleWSSessionAction(ws, r.Context(), pane, msg)
}

// wsHandleListModels lists the provider models for the targeted session. An
// empty registry drops the request (handleWSListModels dereferences
// rt.agent in a goroutine and a nil runtime would panic the process).
func wsHandleListModels(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		target = s.registry.first()
	}
	if target == nil {
		return
	}
	s.handleWSListModels(ws, r.Context(), target)
}

func wsHandleSetModel(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		return
	}
	s.handleWSSetModel(ws, r.Context(), target, msg)
}

func wsHandleSetMode(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		return
	}
	s.handleWSSetMode(ws, r.Context(), target, msg)
}

func wsHandleSetThinkingLevel(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		return
	}
	s.handleWSSetThinkingLevel(ws, r.Context(), target, msg)
}

// wsHandleConfig routes config scoped by the pane's sessionId. After a
// reconnect the re-attach loop leaves the pane pointer on the LAST attached
// pane, which can differ from the client's active pane — re-align it with
// the explicit id so the working-dir change interrupts the right session's
// turn. session_delete/session_detach are NOT re-aligned here: their
// sessionId names the TARGET session, not the acting pane.
func wsHandleConfig(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if msg.SessionID != "" && target != nil {
		*pane = target
	}
	s.handleWSConfig(ws, r.Context(), pane, msg)
}

// wsHandleCancel cancels the targeted session's in-flight turn. Cancel is
// the ONLY way to stop a turn, and it works cross-connection (scoped to the
// targeted session).
func wsHandleCancel(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		return
	}
	target.stream.cancelInFlight()
}

// wsHandleAttach makes the session the connection's current pane and resends
// session_state + history + config + context. Sessions that are not currently
// active are loaded from the store, so the sidebar's "open session" works for
// saved sessions too.
func wsHandleAttach(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	rt2, err := s.ensureSessionRuntime(msg.SessionID)
	if err != nil {
		// The session no longer exists (deleted elsewhere / server
		// restarted with pruning): tell the client to drop the pane.
		_ = ws.writeJSON(WSMessage{Type: "session_removed", SessionID: msg.SessionID, Content: err.Error()})
	} else {
		s.switchPane(ws, pane, rt2)
		s.attachSession(ws, r, rt2, !msg.NoHistory)
	}
}

// wsHandleDetach handles session_detach: the client declared the pane
// closed; detach without cancelling any running turn.
func wsHandleDetach(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		return
	}
	target.detach(ws)
}

// wsHandleClose handles session_close: the client pressed ✕ on an open pane;
// the session is explicitly closed. Detach, then — when no other socket is
// still attached (another tab may be watching the same session and must not
// have its turn cancelled or its runtime evicted) — cancel the in-flight
// turn, flush, and unregister. The session stays saved on disk and reopens
// from the store like any other saved session (ensureSessionRuntime). If the
// detach already orphan-evicted an idle runtime, closeRuntime is a no-op
// (evicted flag).
func wsHandleClose(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		return
	}
	target.detach(ws)
	if target.clientCount() == 0 {
		s.registry.closeRuntime(target)
	}
}

func wsHandleUserTermInput(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if ut := holder.get(); ut != nil {
		_ = ut.Write([]byte(msg.Content))
	}
}

func wsHandleUserTermResize(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if ut := holder.get(); ut != nil && msg.Cols > 0 && msg.Rows > 0 {
		_ = ut.Resize(uint16(msg.Cols), uint16(msg.Rows))
	}
}

func wsHandleUserTermRequest(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	s.spawnUserTerminal(ws, holder)
}

func wsHandleCompact(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if target == nil {
		return
	}
	s.handleWSCompact(ws, r, target)
}

// wsHandleMessage routes a user message. The client scopes every message
// with the sessionId of the pane it was typed in, but the server routes
// messages via the connection's current pane. After a reconnect the
// re-attach loop leaves that pointer on the LAST attached pane, which may
// not be the pane the client is using — so route by the explicit id (the
// pane pointer remains the fallback for legacy empty ids). An id that no
// longer resolves is either an in-flight eviction of THIS very session
// (continue on the same runtime) or a stale pane — in the latter case drop
// the message rather than deliver it to a different session.
func wsHandleMessage(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	if msg.SessionID != "" {
		if target == nil {
			if *pane != nil && (*pane).agent.SessionID == msg.SessionID {
				target = *pane
			} else {
				return
			}
		}
		*pane = target
	}
	s.handleWSUserMessage(ws, r, pane, msg)
}

// resolveRuntime returns the session runtime for id. An EMPTY id targets the
// default (first registered) session — legacy messages without a sessionId.
// An id that is not registered (the session was evicted by the active-cap or
// deleted) must NOT fall back to the default: session-scoped operations
// (cancel, session_detach, session_close, set_model/mode/thinking, compact,
// approvals) would otherwise silently hit the WRONG session — e.g. a pane
// closed after eviction sending session_detach for the evicted id would
// detach the default session from the connection, and a cancel for an
// evicted session would kill the default session's turn. Unknown non-empty
// ids return nil; callers treat that as a stale operation and ignore it.
func (s *Server) resolveRuntime(id string) *sessionRuntime {
	if id == "" {
		return s.registry.first()
	}
	rt, ok := s.registry.get(id)
	if !ok {
		return nil
	}
	return rt
}

// attachSession registers a socket as a viewer of the session and sends the
// attach payload: session_state first (turnActive for "resuming…" rendering,
// E28), then — asynchronously, because a running turn holds the session turn
// lock and the handshake must never block on it — the cheap config snapshot,
// the persisted history, and the full config with context stats (mirrors the
// connect handshake; N panes do not serialize N tokenizations). A reconnect
// mid-turn therefore gets session_state immediately, the live stream events,
// and the completed history when the turn ends.
//
// sendHistory selects the payload shape: a full attach (true, used by the
// connect handshake and the active pane) includes the history snapshot +
// rewind so the client can rebuild the transcript; a lightweight re-register
// (false) skips it. The client uses the lightweight form for BACKGROUND panes
// on reconnect, whose transcript re-derives from a full attach when focused —
// the (potentially multi-MB) snapshot would be discarded client-side, so it
// is neither built nor sent.
func (s *Server) attachSession(ws *wsConn, r *http.Request, rt *sessionRuntime, sendHistory bool) {
	rt.attach(ws)
	s.sendSessionState(ws, rt)
	go func() {
		if !sendHistory {
			// Lightweight re-register: no history snapshot, no rewind. The
			// session_state sent above already told the client whether the
			// session is mid-turn; the config frames below refresh the
			// pane's toolbar/context mirrors.
			_ = ws.writeJSON(agentConfigMsgBasic(rt.agent))
			_ = ws.writeJSON(agentConfigMsg(r.Context(), rt))
			return
		}
		// Snapshot and send history FIRST, without the turn lock. A running
		// turn holds turnMu for its ENTIRE duration (startTurn defers the
		// unlock), so taking turnMu.RLock here would leave a mid-turn page
		// open / reconnect with an empty transcript until the turn finishes
		// — minutes for a long agent run, or indefinitely for a stuck turn.
		// SnapshotMessages deep-clones under its own statsMu (its documented
		// contract: web history snapshots never hold turnMu), so the
		// transcript is consistent with the live stream and paints at once.
		msgs := rt.agent.SnapshotMessages()
		if len(msgs) > 0 {
			_ = ws.writeJSON(WSMessage{
				Type:         "history",
				History:      historyEntries(msgs),
				HistoryEpoch: rt.agent.HistoryEpoch(),
				// The in-flight turn's partial output, so a mid-turn attach
				// shows the current reply instead of "Resuming…" with no
				// context until the turn ends.
				Rewind:    rt.liveRewind(),
				SessionID: rt.agent.SessionID,
			})
		}
		// Config echo: no turnMu — every field is internally synchronized
		// (agentConfigMsgBasic), so a mid-turn attach gets the session's
		// identity/toolbar state immediately instead of when the turn ends.
		// Only the context-stats badge may lag (tokenization of a freshly
		// restored session runs in agentConfigMsg below).
		_ = ws.writeJSON(agentConfigMsgBasic(rt.agent))
		_ = ws.writeJSON(agentConfigMsg(r.Context(), rt))
	}()
}

// sendSessionState writes the session_state message describing the session's
// in-flight turn so a reconnecting client can render "resuming…".
func (s *Server) sendSessionState(ws *wsConn, rt *sessionRuntime) {
	active, _ := rt.turnState()
	_ = ws.writeJSON(WSMessage{
		Type:       "session_state",
		SessionID:  rt.agent.SessionID,
		TurnActive: active,
	})
}

// HandleWSEditor serves the editor WebSocket endpoint (/ws/editor). It is the
// workspace-scoped counterpart of HandleWS: it handles only filesystem and git
// messages (fs_list/fs_read/fs_search/fs_write/fs_replace/fs_apply_patch,
// git_status/git_file_diff) and ignores chat/session messages. The editor
// socket is independent of any chat session, so a busy or streaming session
// never blocks editor saves behind the chat read loop. Write messages
// serialize on the workspace filesystem lock (fsMu) — they wait only
// for the actual mutation window of a
// running tool, never for the whole streaming turn.
func (s *Server) HandleWSEditor(w http.ResponseWriter, r *http.Request) {
	u, err := s.upgradeWS(w, r)
	if err != nil {
		return
	}
	defer u.cleanup()
	conn := u.conn
	ws := u.ws

	incoming := make(chan WSMessage, 8)
	go func() {
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				close(incoming)
				return
			}
			incoming <- msg
		}
	}()

	for msg := range incoming {
		switch msg.Type {
		case "fs_list", "fs_read", "fs_search", "git_status", "git_file_diff":
			s.handleFSReadMessage(ws, r.Context(), msg)
		case "fs_write", "fs_replace", "fs_apply_patch":
			s.handleFSWriteMessage(ws, r.Context(), msg)
		default:
			// Non-editor messages (chat, sessions, terminal) are not
			// supported on this socket; ignore them.
		}
	}
}

func (s *Server) handleWSListSessions(ws *wsConn, rt *sessionRuntime) {
	// Listing hits the session store on disk (metadata index read, label
	// migration file reads, legacy full-scan fallback). Run it off the WS
	// read loop like handleWSListModels, so a slow store cannot block chat,
	// FS, or editor messages behind the sidebar.
	go func() {
		_, sessions, listErr := rt.agent.FormatSessionListForUI()
		if listErr != nil {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", listErr)})
			return
		}
		// The reply deliberately carries NO SessionID: the sessions payload
		// is connection-scoped sidebar state (the full saved list), not a
		// message for one session. Tagging it with the current session id
		// made the client route (and possibly drop) it when that id was not
		// the active pane — e.g. after another tab moved the global default.
		_ = ws.writeJSON(WSMessage{
			Type:     "sessions",
			Sessions: sessionEntries(sessions, s.registry.activeSet()),
		})
	}()
}

func (s *Server) handleWSSessionAction(ws *wsConn, ctx context.Context, pane **sessionRuntime, msg WSMessage) {
	var cmd string
	switch msg.Type {
	case "session_new":
		cmd = "/new"
	case "session_resume", "session_delete":
		if strings.TrimSpace(msg.SessionID) == "" {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: sessionId is required"})
			return
		}
		if msg.Type == "session_resume" {
			cmd = "resume " + strings.TrimSpace(msg.SessionID)
		} else {
			cmd = "resume del " + strings.TrimSpace(msg.SessionID)
		}
	case "session_fork":
		forkArg := fmt.Sprintf("%d", msg.MessageIndex)
		if msg.MessageIndex < 0 {
			forkArg = "last"
		}
		cmd = "fork " + forkArg
	}

	// Registry lifecycle ops: no turn lock needed — new/resume/
	// fork leave the previous session running (continuation), delete drains
	// its own target.
	result, _, err := s.runSessionCommand(ctx, ws, pane, cmd)
	s.writeSessionCommandResult(ws, ctx, *pane, result, err)
}

func (s *Server) handleWSListModels(ws *wsConn, ctx context.Context, rt *sessionRuntime) {
	go func() {
		models, err := rt.agent.ListModels(ctx)
		if err != nil {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
			return
		}
		// CurrentModel reads under the provider's own modelsMu (the same
		// contract agentConfigMsgBasic relies on), so no turnMu is needed —
		// a running turn never delays the model catalog reply.
		current := rt.agent.CurrentModel()
		_ = ws.writeJSON(WSMessage{
			Type:   "models",
			Model:  current,
			Models: s.modelEntries(models),
		})
	}()
}

func (s *Server) handleWSSetModel(ws *wsConn, ctx context.Context, rt *sessionRuntime, msg WSMessage) {
	// SelectModel resolves the selector against the provider catalog, which
	// performs network I/O on first use. Pre-fetch the catalog OUTSIDE the
	// turn lock so a slow endpoint cannot stall the whole session (its
	// turns and every other turnMu-taking handler); SelectModel under the
	// lock then only touches the in-memory cache. If the pre-fetch fails,
	// SelectModel surfaces the same error under the lock — no regression.
	_, _ = rt.agent.ListModels(ctx)
	if !rt.acquireTurnForHandler(ws) {
		return
	}
	a := rt.agent
	err := a.SelectModel(ctx, msg.Model)
	cfg := agentConfigMsgBasic(a)
	fillModelPricing(a, &cfg)
	rt.turnMu.Unlock()
	if err != nil {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
		return
	}
	// Model is per-session: SelectModel above applied to this pane's
	// provider only. The workspace default (ws.Model) is fixed at startup and
	// never mutated here, and no other session's provider is touched, so two
	// panes can run different models concurrently. The config echo goes
	// to this pane only (its own Mode/ThinkingLevel/Model).
	// Tokenization + echo off the read loop: ContextStats on a large
	// uncached session takes seconds, and the read loop serializes every
	// message (including pane switches).
	go func() {
		accum := a.SnapshotUsageAccum()
		applyContextStats(&cfg, a.ContextStats(ctx), &accum)
		_ = ws.writeJSON(cfg)
	}()
}

func (s *Server) handleWSSetMode(ws *wsConn, ctx context.Context, rt *sessionRuntime, msg WSMessage) {
	if !rt.acquireTurnForHandler(ws) {
		return
	}
	a := rt.agent
	modeSet := false
	var cfg WSMessage
	if m, ok := agent.ParseMode(msg.Mode); ok {
		a.SetMode(m)
		modeSet = true
		cfg = agentConfigMsgBasic(a)
	}
	rt.turnMu.Unlock()
	if modeSet {
		// Echo off the read loop (tokenization can take seconds on a large
		// uncached session; the read loop serializes every message).
		go func() {
			accum := a.SnapshotUsageAccum()
			applyContextStats(&cfg, a.ContextStats(ctx), &accum)
			_ = ws.writeJSON(cfg)
		}()
	}
}

func (s *Server) handleWSSetThinkingLevel(ws *wsConn, ctx context.Context, rt *sessionRuntime, msg WSMessage) {
	if !rt.acquireTurnForHandler(ws) {
		return
	}
	a := rt.agent
	if s.isValidThinkingLevel(a, msg.ThinkingLevel) {
		a.SetThinkingLevel(agent.ThinkingLevel(msg.ThinkingLevel))
	}
	cfg := agentConfigMsgBasic(a)
	rt.turnMu.Unlock()
	fillModelPricing(a, &cfg)
	// Echo off the read loop (tokenization can take seconds on a large
	// uncached session; the read loop serializes every message).
	go func() {
		accum := a.SnapshotUsageAccum()
		applyContextStats(&cfg, a.ContextStats(ctx), &accum)
		_ = ws.writeJSON(cfg)
	}()
}

// isValidThinkingLevel reports whether v is a valid reasoning-effort selection
// for the session's current model: ""/"off" are always valid (omit), and any
// other value is valid only when it is in the model's effective accepted set
// (models.dev when known, DefaultReasoningEfforts otherwise). Providers
// without effort reporting (test stubs) accept any non-blank value.
func (s *Server) isValidThinkingLevel(a *agent.Agent, v string) bool {
	level := agent.NormalizeThinkingLevel(v)
	if level == "" || level == agent.ThinkingOff {
		return true // omit
	}
	if p, ok := a.Provider.(llm.ReasoningEffortsProvider); ok {
		// Membership check against the normalized value ("Max" → "max").
		return slices.Contains(p.ModelReasoningEfforts(a.CurrentModel()), string(level))
	}
	return true
}

func (s *Server) handleWSConfig(ws *wsConn, ctx context.Context, pane **sessionRuntime, msg WSMessage) {
	// Changing the working directory is only allowed in global mode: in
	// project mode the server is scoped to one project directory and
	// sessions persist under it, so re-pointing the workspace would orphan
	// sessions and escape the project boundary. The TUI's /dir command is a
	// separate path (not web mode) and is unaffected.
	if !s.ws.GlobalMode {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: changing the working directory is only allowed in global mode (start gogen with --global)"})
		return
	}
	absDir, err := filepath.Abs(msg.WorkingDir)
	if err != nil {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: invalid path: %v", err)})
		return
	}
	info, err := os.Stat(absDir)
	if err != nil || !info.IsDir() {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: directory does not exist: %s", absDir)})
		return
	}
	// The working dir is workspace-global. Interrupt the pane's own turn
	// (cancel-then-lock), then sync the change to EVERY session agent under
	// its own turn lock in a fixed (sorted) order — SetWorkingDir /
	// AfterWorkingDirChange mutate persist fields that a mid-turn doPersist
	// would race, so each must run under that session's turn lock.
	rt := *pane
	if rt.ownsTurn(ws) {
		rt.stream.cancelInFlight()
	}
	// Deliberately do NOT mirror the change into s.config.WorkingDir: the
	// server never re-reads that field after construction (SaveConfig is
	// reachable only from the --save-config CLI flag and the TUI, both
	// outside web mode), and writing it here would be an unsynchronized
	// write to a struct other goroutines read for unrelated fields. The
	// authoritative runtime value is ws.WorkingDir (set below) and the
	// per-session agents' WorkingDir (applyWorkingDirToAll).
	s.ws.SetWorkingDir(absDir)
	// Apply to every session agent OFF the read loop: a running turn
	// holds its session's turnMu for its ENTIRE duration, so waiting for all
	// of them here would freeze this connection's messages — pane switches,
	// sends, cancels — for as long as the longest running turn. The dir is
	// workspace-global; each agent's SetWorkingDir is atomic under its own
	// turnMu, so messages issued while the change is in flight simply see
	// the pre-change dir until their session is updated.
	go func(paneRT *sessionRuntime) {
		skipped := s.applyWorkingDirToAll(absDir)
		a := paneRT.agent
		if !paneRT.turnMu.TryRLock() {
			// The pane's own turn is still stuck (the sweep skipped it): the
			// config echo would hang on the lock. Send the skip report and
			// let the next config request or the turn end re-sync the client.
			if len(skipped) > 0 {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: workingDirSkipMessage(absDir, skipped)})
			}
			return
		}
		cfg := agentConfigMsgBasic(a)
		paneRT.turnMu.RUnlock()
		accum := a.SnapshotUsageAccum()
		applyContextStats(&cfg, a.ContextStats(ctx), &accum)
		_ = ws.writeJSON(WSMessage{Type: "config", WorkingDir: absDir, Model: cfg.Model, ContextLimit: cfg.ContextLimit, UsedTokens: cfg.UsedTokens, UsedSource: cfg.UsedSource, UsedPercent: cfg.UsedPercent, CompactAt: cfg.CompactAt, MessageCount: cfg.MessageCount, NearCompact: cfg.NearCompact, WarnNearCompact: cfg.WarnNearCompact, ToolTruncated: cfg.ToolTruncated, Mode: cfg.Mode, GlobalMode: cfg.GlobalMode})
		if len(skipped) > 0 {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: workingDirSkipMessage(absDir, skipped)})
		}
	}(*pane)
}

// applyWorkingDirToAll syncs a workspace working-dir change to every session
// agent. Each agent's SetWorkingDir + AfterWorkingDirChange run under its own
// turn lock, acquired one at a time in sorted id order (never nested, so no
// lock-order deadlock; a running turn must finish or be cancelled before its
// session's lock is taken). Sessions that cannot be quiesced within the
// standard drain budget are skipped and returned so the caller can report
// them; they keep the pre-change directory until their turn finishes and the
// change is re-issued.
func (s *Server) applyWorkingDirToAll(absDir string) (skipped []string) {
	ids := s.registry.activeIDs()
	sort.Strings(ids)
	// A working-dir change relocates every session's persisted state into the
	// new directory: each agent's SetWorkingDir + AfterWorkingDirChange below
	// forces a full save there, which would stamp Updated=now on every open
	// session — the saved-session list would rank them all as "just updated"
	// even though none was interacted with. Capture each session's current
	// persisted Updated (from its pre-change directory — each agent's
	// WorkingDir still points there until the sweep) and restore it into the
	// new directory's index right after the flush. Best-effort: sessions with
	// no persisted state (never-saved /new panes) or a failed restore keep
	// the fresh stamp, matching the pre-fix behavior.
	for _, id := range ids {
		rt, ok := s.registry.get(id)
		if !ok {
			continue
		}
		// Bounded wait instead of a blocking Lock: a running (possibly
		// stuck) turn holds its session's turnMu for its ENTIRE duration,
		// and a blocking Lock here would hang the goroutine — and the
		// working-dir change — on that one session forever. Only the
		// requesting pane's turn was cancelled above; other sessions' turns
		// are not interrupted.
		if !rt.tryAcquireTurn(wsTurnAcquireWait) {
			if !rt.tryAcquireTurn(wsStreamDrainWait) {
				skipped = append(skipped, id)
				continue
			}
		}
		var prevUpdated time.Time
		if s.ws.Store != nil {
			prevUpdated = s.ws.Store.UpdatedAt(rt.agent.WorkingDir, id)
		}
		rt.agent.SetWorkingDir(absDir)
		rt.agent.AfterWorkingDirChange()
		if s.ws.Store != nil && !prevUpdated.IsZero() {
			_ = s.ws.Store.SetUpdatedAt(absDir, id, prevUpdated)
		}
		rt.turnMu.Unlock()
	}
	return skipped
}

// workingDirSkipMessage reports a partially-applied working-dir change: the
// listed sessions were busy (running turns that were not interrupted) and
// still use the old directory.
func workingDirSkipMessage(absDir string, skipped []string) string {
	return fmt.Sprintf("Working directory set to %s; %d session(s) were busy and still use the old directory (re-issue the change once their turns finish): %s",
		absDir, len(skipped), strings.Join(skipped, ", "))
}

// handleWSCompact runs the manual compaction command (/compact) for a web
// client. Unlike a regular message it never reaches the LLM as a prompt: it
// cancels any in-flight turn, acquires the session turn lock, emits a
// "compacting" event so the client can show a persistent progress indicator,
// runs CompactHistory (which may take a while — it summarizes the middle via
// the provider), then reports the result and refreshed context stats. Runs in
// a goroutine so the slow summarization does not block the WS read loop.
func (s *Server) handleWSCompact(ws *wsConn, r *http.Request, rt *sessionRuntime) {
	// Interrupt semantics are scoped to the connection that owns the current
	// turn: a second connection attached as a viewer must not kill a
	// turn it does not own — it waits and gets the busy rejection instead.
	if rt.ownsTurn(ws) {
		rt.stream.cancelInFlight()
	}
	if !rt.tryAcquireTurn(wsTurnAcquireWait) {
		// Cancel may have timed out while a tool was still exiting; wait once more.
		if rt.ownsTurn(ws) {
			rt.stream.cancelInFlight()
		}
		if !rt.tryAcquireTurn(wsStreamDrainWait) {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
			return
		}
	}
	// Same eviction guard as handleWSUserMessage/acquireTurnForHandler: the
	// session may have left the registry while we waited for the lock; a
	// compact on an evicted runtime would be invisible to shutdown.
	if rt.evicted.Load() {
		rt.turnMu.Unlock()
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
	if !s.acquireTurnForHandler(ws, rt) {
		return
	}

	a := rt.agent
	modeOut, modeHandled := a.HandleModeCommand(msg.Content)
	if modeHandled {
		modeCfg := agentConfigMsgBasic(a)
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

// acquireTurnForHandler takes the session turn lock for a message handler,
// waiting briefly and re-cancelling once when the previous turn is still
// draining. Returns false when the runtime is busy or was evicted; the
// caller drops the message. On success the caller owns rt.turnMu.
func (s *Server) acquireTurnForHandler(ws *wsConn, rt *sessionRuntime) bool {
	if !rt.tryAcquireTurn(wsTurnAcquireWait) {
		// Cancel may have timed out while a tool was still exiting; wait once
		// more. Only re-cancel when this connection owns the turn — a second
		// connection must not kill a turn it does not own.
		if rt.ownsTurn(ws) {
			rt.stream.cancelInFlight()
		}
		if !rt.tryAcquireTurn(wsStreamDrainWait) {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
			return false
		}
	}

	// The session may have been evicted (registry cap / delete) after this
	// connection resolved it (e.g. a stale id-less pane). Starting a turn on
	// an evicted runtime would be invisible to cancel/prune/shutdown. The
	// flag is set while the eviction holds turnMu, so the check under the
	// lock is race-free (see acquireTurnForHandler). Drop silently — the
	// client already got session_detached and closed the pane.
	if rt.evicted.Load() {
		rt.turnMu.Unlock()
		return false
	}
	return true
}

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
	rt.setTurnActive(true, time.Now(), owner)
	go func(content string, images []llm.ImageInput, turnCtx context.Context, done chan error) {
		// Defers run LIFO, so the cleanup executes in this order: turn_end
		// broadcast → setTurnActive(false) → done → stream.end() →
		// turnMu.Unlock().
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
		defer rt.setTurnActive(false, time.Time{}, nil)
		defer rt.broadcast(WSMessage{Type: "turn_end", SessionID: rt.agent.SessionID})
		ctx := agent.ContextWithDeleteApprover(turnCtx, rt.deleteApprover())
		// write fans out to every attached socket and tags the source
		// sessionId. A write failure detaches that socket (broadcast does it);
		// it NEVER cancels the LLM call — the turn belongs to the session and
		// keeps running headless (§4).
		write := func(v WSMessage) {
			if ctx.Err() != nil {
				return
			}
			if v.SessionID == "" {
				v.SessionID = rt.agent.SessionID
			}
			rt.broadcast(v)
		}
		tokens := streamutil.NewTokenBatcher(func(think bool, text string) {
			if think {
				write(WSMessage{Type: "thinking_token", Content: text, ThinkingPos: rt.liveThinkingSegmentEnd(text)})
			} else {
				write(WSMessage{Type: "stream", Content: text, ContentPos: rt.liveContentSegmentEnd(text)})
			}
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

		handlers := rt.buildStreamHandlers(ctx, write, tokens, &termMu, termBatches, termOpened)

		_, err := rt.agent.StreamProcessInputWithImages(ctx, content, images, handlers)
		var persistErr error
		var ctxMsg WSMessage
		if err == nil {
			// No turnMu re-acquire here: this goroutine already holds the
			// session turn lock (handed off by handleWSUserMessage) for the
			// whole turn. ConsumePersistError is internally synchronized
			// (persistMu), so it is safe even when a shutdown/delete/eviction
			// flush runs concurrently without turnMu.
			persistErr = rt.agent.ConsumePersistError()
			// context.Background() rather than the (dead) request context:
			// the turn outlives the connection (§4).
			ctxMsg = contextMsg(context.Background(), rt.agent)
		}
		if err != nil {
			if ctx.Err() != nil {
				tokens.Flush()
				// Broadcast directly (not via write, which early-returns on a
				// cancelled ctx) so the cancellation reaches attached clients.
				rt.broadcast(WSMessage{Type: "cancelled", Content: "Cancelled.", SessionID: rt.agent.SessionID})
				return
			}
			tokens.Flush()
			write(WSMessage{Type: "stream_end"})
			write(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
			log.Printf("stream error: %v", err)
			return
		}
		if persistErr != nil {
			write(WSMessage{Type: "response", Content: fmt.Sprintf("Warning: failed to save session: %v", persistErr)})
		}
		tokens.Flush()
		// No trailing stream_end here: every round already wrote one via the
		// OnStreamEnd handler above, and turn_end marks the turn boundary.
		if ctxMsg.Type != "" {
			write(ctxMsg)
		}
	}(content, images, streamCtx, errCh)
}

// buildStreamHandlers wires the runtime's live-turn state and the token
// batcher to the agent's stream callbacks. write fans out to every attached
// socket with session tagging; the terminal-tab maps are mutex-guarded
// (accessed from the exec pipe and the stream goroutines).
func (rt *sessionRuntime) buildStreamHandlers(ctx context.Context, write func(WSMessage), tokens *streamutil.TokenBatcher, termMu *sync.Mutex, termBatches map[string]*streamutil.TokenBatcher, termOpened map[string]struct{}) *llm.StreamHandlers {
	return &llm.StreamHandlers{
		OnCompacting: func() {
			write(WSMessage{Type: "compacting"})
		},
		OnStart: func() {
			// Reset the live-turn buffer for the new turn.
			rt.liveTurnBegin()
			// Tell the client the server-side index of the user message
			// that StreamProcessInput just appended (for edit/resend).
			// Index goes in Content because WSMessage.Index has omitempty
			// and the first message is index 0.
			userIdx := rt.agent.MessageCount() - 1
			if userIdx >= 0 {
				write(WSMessage{Type: "user_acked", Content: fmt.Sprintf("%d", userIdx)})
			}
			write(WSMessage{Type: "thinking"})
			if ctx.Err() != nil {
				return
			}
			write(contextMsg(ctx, rt.agent))
		},
		OnRoundStart: func() {
			rt.liveRoundBegin()
			write(WSMessage{Type: "thinking"})
			if ctx.Err() != nil {
				return
			}
			write(contextMsg(ctx, rt.agent))
		},
		OnStreamOpened: func() {
			write(WSMessage{Type: "waiting"})
		},
		OnStreamActivity: func() {},
		OnThinkingToken: func(token string) {
			rt.liveAppendThinking(token)
			tokens.ThinkToken(token)
		},
		OnToken: func(token string) {
			rt.liveAppendContent(token)
			tokens.StreamToken(token)
		},
		OnStreamEnd: func() {
			tokens.Flush()
			rt.liveRoundEnd()
			write(WSMessage{Type: "stream_end"})
		},
		OnReplyModel: func(model string) {
			// Fired by the agent before each round's OnStreamEnd, so
			// this frame must arrive before stream_end for the client
			// to stamp the still-live assistant bubble (intermediate
			// content+tool rounds included). Flush pending content
			// tokens first so the client has created the bubble by the
			// time model_used is processed.
			if model == "" {
				return
			}
			tokens.Flush()
			write(WSMessage{Type: "model_used", Model: model})
		},
		OnToolCallStart: func(index int, id, name string) {
			tokens.Flush()
			rt.liveToolStart(index, id, name)
			write(WSMessage{
				Type:       "tool_call_start",
				Tool:       name,
				ToolCallID: id,
				Index:      index,
			})
		},
		OnToolCallArgsDelta: func(index int, id, name, argsDelta string) {
			tokens.Flush()
			argsPos := rt.liveToolArgsAppend(index, argsDelta)
			write(WSMessage{
				Type:       "tool_call_delta",
				Tool:       name,
				ToolCallID: id,
				Index:      index,
				ArgsDelta:  argsDelta,
				ArgsPos:    argsPos,
			})
		},
		OnToolCall: func(tc llm.ToolCall) {
			tokens.Flush()
			write(WSMessage{
				Type:       "tool_call",
				Tool:       tc.Name,
				ToolCallID: tc.ID,
				Index:      tc.Index,
				Args:       tc.Args,
			})
		},
		OnToolExecute: func(name string) {
			write(WSMessage{Type: "tool_execute", Tool: name})
		},
		OnToolOutput: func(id, name, command, chunk string) {
			if ctx.Err() != nil {
				return
			}
			termMu.Lock()
			first := false
			if _, ok := termOpened[id]; !ok {
				termOpened[id] = struct{}{}
				first = true
			}
			b := termBatches[id]
			if b == nil {
				b = streamutil.NewTokenBatcher(func(_ bool, text string) {
					write(WSMessage{Type: "term_output", TermID: id, Content: text})
				}, wsTokenFlushInterval)
				termBatches[id] = b
			}
			termMu.Unlock()
			if first {
				write(WSMessage{Type: "term_opened", TermID: id, ToolCallID: id, Tool: name, Content: "$ " + command})
			}
			b.StreamToken(chunk)
		},
		OnToolResult: func(id, name, result string, success bool) {
			// Close this tool call's live terminal tab, if one was
			// opened. Flush first so buffered chunks land before
			// term_exit (the send queue is FIFO).
			termMu.Lock()
			b := termBatches[id]
			delete(termBatches, id)
			_, opened := termOpened[id]
			delete(termOpened, id)
			termMu.Unlock()
			if b != nil {
				b.Flush()
				b.Close()
			}
			if opened {
				write(WSMessage{Type: "term_exit", TermID: id, ToolCallID: id, Success: success})
			}
			write(WSMessage{
				Type:            "tool_result",
				Tool:            name,
				ToolCallID:      id,
				Result:          truncateToolResult(result),
				Success:         success,
				ResultTruncated: len(result) > 128*1024,
			})
		},
	}
}

// truncateToolResult cuts oversized tool results at a rune boundary so the
// client never renders a broken UTF-8 character, marking the cut explicitly.
func truncateToolResult(result string) string {
	const maxResult = 128 * 1024
	if len(result) <= maxResult {
		return result
	}
	// Rune-safe cut: slicing at maxResult could split a UTF-8
	// rune mid-sequence and render a broken character.
	return contextmgr.TruncateRuneSafe(result, maxResult) + fmt.Sprintf("\n… truncated (%d bytes total)", len(result))
}

// Start serves the web UI until ctx is cancelled or the listener fails.
// On cancel it force-closes WebSockets and the HTTP listener so shutdown
// is not blocked by hijacked connections, then returns so the caller can
// FlushSession (Ctrl+C / SIGTERM in --web mode).
func (s *Server) Start(ctx context.Context, addr string) error {
	if !IsLoopbackBind(addr) {
		if s.authToken == "" {
			return fmt.Errorf("non-loopback bind %q requires an auth token; set GOGEN_WEB_TOKEN or web_auth_token", addr)
		}
		if s.tlsCertFile == "" || s.tlsKeyFile == "" {
			log.Printf("WARNING: non-loopback bind %q without TLS — auth token is sent in plain text. Set GOGEN_WEB_TLS_CERT and GOGEN_WEB_TLS_KEY (or web_tls_cert_file / web_tls_key_file) for encryption.", addr)
		}
		log.Printf("listening on non-loopback %s with token auth", addr)
	}
	if s.authToken != "" {
		// Log the token on startup so users can construct the login URL.
		log.Printf("auth token: %s", s.authToken)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWS)
	mux.HandleFunc("/ws/editor", s.HandleWSEditor)
	mux.HandleFunc("/", s.HandleStatic)
	srv := &http.Server{Addr: addr, Handler: mux}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if s.tlsCertFile != "" && s.tlsKeyFile != "" {
			err = srv.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		errCh <- err
	}()

	s.trackHTTPServer(srv)
	defer s.untrackHTTPServer()

	select {
	case <-ctx.Done():
		s.ForceClose()
		select {
		case err := <-errCh:
			if err == nil || errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

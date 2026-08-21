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
	"gogen/internal/mcp"
	"gogen/internal/onoff"
	"gogen/internal/projectfile"
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
	// bootstrapLimiter throttles the unauthenticated ?token= / ?pair=
	// bootstrap endpoints per source IP.
	bootstrapLimiter *ipLimiter
	// pairMu guards the onboarding pairing code (see pairing.go): the code,
	// its expiry, and the number of uses so far.
	pairMu     sync.Mutex
	pairCode   string
	pairExpiry time.Time
	pairMinted time.Time // when the current code was installed (SetPairingCode)
	pairUses   int
	// lastPairIP / lastPairAt remember the most recent accepted pairing
	// exchange (IP + time) so the unauthenticated-page path can recognize
	// "an exchange just succeeded from this device, yet no cookie arrived"
	// — the browser-side cookie failure (different cookie jar or blocked
	// cookies) — and diagnose it in the log and on the sign-in page.
	lastPairIP   string
	lastPairAt   time.Time
	staticAssets staticAssetCache // lazily gzip-compressed embedded assets

	// providerTestBuilder builds a throwaway provider for test_provider
	// (never registered, never wired to a session). Defaults to a real
	// OpenAIProvider; tests inject a mock builder.
	providerTestBuilder func(op ProviderOpRequest, workingDir string) (llm.LLMProvider, error)

	// mcpTestFn probes one MCP stdio server for test_mcp (never registers
	// anything). Defaults to mcp.TestServer; tests inject a stub so no real
	// process is spawned.
	mcpTestFn func(ctx context.Context, server config.MCPServerConfig) ([]llm.Tool, error)
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
	rt := newSessionRuntimeWithHold(a, ws.ApprovalHold())
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
	a.SetToolHandlers(ws.buildToolHandlers())
	s := &Server{
		ws:               ws,
		registry:         reg,
		config:           cfg,
		allowedOrigins:   allowed,
		authToken:        token,
		tlsCertFile:      tlsCert,
		tlsKeyFile:       tlsKey,
		connLimiter:      newRateLimitState(defaultMaxWSConns),
		upgradeLimiter:   newIPLimiter(5, 10), // 5 upgrades/sec/IP, burst 10
		bootstrapLimiter: newIPLimiter(1, 5),  // 1 bootstrap/sec/IP, burst 5
	}
	s.providerTestBuilder = func(op ProviderOpRequest, workingDir string) (llm.LLMProvider, error) {
		return llm.NewOpenAIProvider(op.APIKey, op.Model, op.BaseURL, workingDir), nil
	}
	s.mcpTestFn = mcp.TestServer
	// Background model validation for a restored default session runs after
	// the server starts; push the result to the session's clients so the
	// toolbar does not keep showing a model that was cleared or replaced by
	// the validation.
	a.OnModelChanged = func() {
		s.pushConfigForAgent(a)
		s.maybeProbeReasoningEfforts(context.Background(), a)
	}
	// Agent board-tool mutations broadcast a fresh board_state so every open
	// kanban tab stays live, plus a success notice (toast) so the user sees
	// agent-triggered changes even when they didn't initiate them (the
	// initial agent, and every session created later via NewSessionAgent).
	ws.BoardChangedHook = func(msg string) {
		s.broadcastBoardState()
		s.broadcastBoardNotice(msg)
	}
	a.SetOnBoardChanged(ws.BoardChangedHook)
	// The initial agent carries the same live feature flags as the
	// workspace (seeded from cfg at construction), but it must use the
	// WORKSPACE's single shared board manager — not the per-process manager
	// setup.go created for it. Two managers over the same board directory
	// would split the in-process serialization (claims/moves/NextID) between
	// the first session and every later session + the web board tab.
	// Re-seeding here also keeps the initial agent consistent when
	// NewServer is fed an agent built outside newAgent (tests/embeds).
	a.SetBoardEnabled(ws.GetBoardEnabled())
	a.SetSubagentsEnabled(ws.GetSubagentEnabled())
	a.SetSubagentMaxDepth(ws.GetSubagentMaxDepth())
	a.SetSubagentMaxConcurrent(ws.GetSubagentMaxConcurrent())
	a.SetBoardManager(ws.GetBoardManager())
	a.SetSkillsManager(ws.skillsManager)
	// The subagent spawner needs the registry, so it is installed after the
	// server is constructed; NewSessionAgent seeds it on every later session.
	ws.SubagentSpawner = &subagentSpawner{s: s, children: newChildRegistry()}
	a.SetSubagentSpawner(ws.SubagentSpawner)
	// A session leaving the registry takes its continuable subagent
	// children with it (cancel + release): a child whose parent is gone
	// cannot deliver report/completion anyway.
	s.registry.evictHook = func(id string) {
		if sp, ok := ws.SubagentSpawner.(*subagentSpawner); ok {
			sp.cancelAll(id)
		}
	}
	// Job-completion notices: the deliverer resolves a session id to its
	// live runtime; NewSessionAgent installs per-agent hooks on top of it
	// for later sessions, this install covers the initial agent.
	if ws.jobNotices {
		ws.jobNoticeDeliverer = func(id, summary string) {
			if rt, ok := s.registry.get(id); ok {
				rt.deliverToSession(summary)
			}
		}
		a.SetJobNoticeHook(func(summary string) {
			ws.jobNoticeDeliverer(a.SessionID, summary)
		})
	}
	return s
}

// SetDefaultModel updates the workspace default model that new session
// providers are seeded from. Called by the web startup validation goroutine
// (runWeb) once ValidateRestoredModel has resolved the effective model
// (possibly auto-selected or cleared), so a session created mid-validation
// never reads a half-updated seed. Synchronized on the workspace with
// session creation (ProviderFactory reads DefaultModel).
func (s *Server) SetDefaultModel(name string) {
	s.ws.SetDefaultModel(name)
}

// newSessionRuntimeFor builds a session runtime carrying the server's
// configured approval-hold window (see web_approval_hold_secs / F2). Reads
// the live runtime overlay, so a settings-modal change applies to runtimes
// created afterwards.
func (s *Server) newSessionRuntimeFor(a *agent.Agent) *sessionRuntime {
	return newSessionRuntimeWithHold(a, s.ws.ApprovalHold())
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
	msg.ReasoningEffortsUnsupported = reasoningEffortsUnsupported(a)
	msg.ModelDescription = a.CurrentModelDescription()
	// Live feature flags: the settings modal renders and toggles these.
	msg.Board = onOff(a.BoardEnabled())
	msg.Subagent = onOff(a.SubagentsEnabled())
	msg.SubagentMaxDepth = a.SubagentMaxDepth()
	return msg
}

// onOff renders a boolean as the config-WS "on"/"off" spelling.
func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// reasoningEffortsUnsupported reports whether the session's current model
// definitively has no reasoning-effort control (a known models.dev entry with
// no effort options, or a llama.cpp capability probe that reported no
// support) — the client hides the thinking chips in that case. In-memory
// lookup, never blocks.
func reasoningEffortsUnsupported(a *agent.Agent) bool {
	if a == nil || a.Provider == nil {
		return false
	}
	if p, ok := a.Provider.(*llm.OpenAIProvider); ok {
		return p.ReasoningEffortUnsupported(a.CurrentModel())
	}
	return false
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
		msg := agentConfigMsgBasic(a)
		s.decorateConfig(&msg)
		rt.broadcast(msg)
	}
}

// maybeProbeReasoningEfforts derives the session model's accepted
// reasoning-effort values from a llama.cpp /props (+ /apply-template)
// capability probe and pushes a fresh config when the derived set differs
// from what clients are showing (the initial config echo carries the
// fallback set). Runs in its own goroutine — bounded network I/O; the
// provider caches per model, so repeat triggers are no-ops.
func (s *Server) maybeProbeReasoningEfforts(ctx context.Context, a *agent.Agent) {
	if a == nil || a.Provider == nil {
		return
	}
	p, ok := a.Provider.(*llm.OpenAIProvider)
	if !ok {
		return
	}
	go func() {
		changed, err := p.ProbeReasoningEfforts(ctx, a.CurrentModel())
		if err != nil {
			return // keep the fallback set; a later trigger retries
		}
		if changed {
			s.pushConfigForAgent(a)
		}
	}()
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

// echoConfigOffLoop applies context stats to cfg and writes the config echo
// off the read loop: ContextStats tokenization can take seconds on a large
// uncached session, and the read loop serializes every message on the
// connection (including cancel).
func echoConfigOffLoop(ws *wsConn, ctx context.Context, a *agent.Agent, cfg *WSMessage) {
	go func() {
		accum := a.SnapshotUsageAccum()
		applyContextStats(cfg, a.ContextStats(ctx), &accum)
		_ = ws.writeJSON(*cfg)
	}()
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

func sessionEntries(list []agent.SessionInfo, active, turnActive map[string]bool) []SessionEntry {
	out := make([]SessionEntry, 0, len(list))
	for _, s := range list {
		out = append(out, SessionEntry{
			ID:              s.ID,
			UpdatedAt:       s.UpdatedAt,
			MessageCount:    s.MessageCount,
			Label:           s.Label,
			Oneshot:         s.Oneshot,
			ParentID:        s.ParentID,
			Active:          active[s.ID],
			TurnActive:      turnActive[s.ID],
			SubagentStatus:  s.SubagentStatus,
			SubagentSummary: s.SubagentSummary,
		})
	}
	return out
}

// activeSet returns the session ids that are genuinely live for the
// sessions payload's "resume to continue" indicator: a runtime with at
// least one attached VIEWER (open as a pane in some tab) or a running
// turn. Merely being registered is not enough — the restored default
// session and passive (approval-only) attachments must not pin the
// indicator for a session nobody is viewing (README: the indicator only
// appears for sessions open in another tab or with a turn running
// server-side). Ids are snapshotted under the registry lock and each
// runtime's state read without it (no lock ordering with clientsMu /
// stateMu), mirroring turnActiveSet.
func (r *sessionRegistry) activeSet() map[string]bool {
	ids := r.activeIDs()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		rt, ok := r.get(id)
		if !ok {
			continue
		}
		if rt.viewerCount() > 0 {
			out[id] = true
			continue
		}
		if active, _ := rt.turnState(); active {
			out[id] = true
		}
	}
	return out
}

// turnActiveSet returns the ids of registered runtimes that currently have
// a running turn. The sessions payload uses it so the client can tell a
// genuinely running session ("responding") from one that is merely
// registered-but-idle (open as a pane, or resumed from the store) — the
// plain active set cannot. Ids are snapshotted under the registry lock and
// each runtime's turn state read without it (no lock ordering with stateMu).
func (r *sessionRegistry) turnActiveSet() map[string]bool {
	ids := r.activeIDs()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		rt, ok := r.get(id)
		if !ok {
			continue
		}
		if active, _ := rt.turnState(); active {
			out[id] = true
		}
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
		resp.Sessions = sessionEntries(result.Sessions, s.registry.activeSet(), s.registry.turnActiveSet())
	}
	cfg = agentConfigMsgBasic(a)
	s.decorateConfig(&cfg)
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
			Provider:         m.Provider,
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
		if e, ok := wsHandlers[msg.Type]; ok {
			e.handle(s, ws, r, &pane, target, msg, userTermHolder)
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

// wsHandlerKind tags which handler family serves a message type. Go only
// allows comparing function values to nil, so tests assert socket routing
// through this tag instead of the handler itself.
type wsHandlerKind string

const (
	wsKindChat    wsHandlerKind = "chat"
	wsKindFSRead  wsHandlerKind = "fs-read"
	wsKindFSWrite wsHandlerKind = "fs-write"
)

type wsHandlerEntry struct {
	handle wsMessageHandler
	kind   wsHandlerKind
}

// wsHandlers maps inbound message types to their handlers. Unknown types are
// dropped silently, exactly like the switch's implicit default. FS entries
// carry a kind tag so TestWSEditorHandlerParity can assert they stay in
// lockstep with the /ws/editor socket's editorReadTypes/editorWriteTypes.
var wsHandlers = map[string]wsHandlerEntry{
	"session_fork":       {handle: wsHandleFork},
	"fs_list":            {handle: wsHandleFSRead, kind: wsKindFSRead},
	"fs_read":            {handle: wsHandleFSRead, kind: wsKindFSRead},
	"fs_search":          {handle: wsHandleFSRead, kind: wsKindFSRead},
	"git_status":         {handle: wsHandleFSRead, kind: wsKindFSRead},
	"git_file_diff":      {handle: wsHandleFSRead, kind: wsKindFSRead},
	"git_commit_message": {handle: wsHandleFSRead, kind: wsKindFSRead},
	"fs_write":           {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"fs_replace":         {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"fs_apply_patch":     {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"git_commit":         {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"git_stage":          {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"git_unstage":        {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"git_push":           {handle: wsHandleFSWrite, kind: wsKindFSWrite},
	"list_sessions":      {handle: wsHandleListSessions},
	"session_new":        {handle: wsHandleSessionAction},
	"session_resume":     {handle: wsHandleSessionAction},
	"session_delete":     {handle: wsHandleSessionAction},
	"list_models":        {handle: wsHandleListModels},
	"set_model":          {handle: wsHandleSetModel},
	"set_mode":           {handle: wsHandleSetMode},
	"set_thinking_level": {handle: wsHandleSetThinkingLevel},
	"config":             {handle: wsHandleConfig},
	"cancel":             {handle: wsHandleCancel},
	"board_op":           {handle: wsHandleBoardOp},
	"provider_save":      {handle: wsHandleProviderSave},
	"provider_delete":    {handle: wsHandleProviderDelete},
	"test_provider":      {handle: wsHandleTestProvider},
	"test_mcp":           {handle: wsHandleTestMCP},
	"session_attach":     {handle: wsHandleAttach},
	"session_detach":     {handle: wsHandleDetach},
	"session_close":      {handle: wsHandleClose},
	"user_term_input":    {handle: wsHandleUserTermInput},
	"user_term_resize":   {handle: wsHandleUserTermResize},
	"user_term_request":  {handle: wsHandleUserTermRequest},
	"compact":            {handle: wsHandleCompact},
	"message":            {handle: wsHandleMessage},
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
		// Closing a nested (subagent) child reports back to its parent
		// session: the main agent must learn the child was stopped by the
		// user (delivered as a system message once the parent is idle /
		// its turn ends). Skipped when the runtime was already evicted —
		// closeRuntime would be a no-op, so there is nothing to report.
		if parentID := target.agent.ParentID(); parentID != "" && !target.evicted.Load() {
			label := target.agent.SessionLabelSnapshot()
			if label == "" {
				label = target.agent.SessionID
			}
			s.registry.deliverToParent(parentID, fmt.Sprintf("[subagent %s] closed by the user — its session stays saved and can be reopened.", label))
		}
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
		if sendHistory {
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
		}
		// Config echo: no turnMu — every field is internally synchronized
		// (agentConfigMsgBasic), so a mid-turn attach gets the session's
		// identity/toolbar state immediately instead of when the turn ends.
		// Only the context-stats badge may lag (tokenization of a freshly
		// restored session runs in agentConfigMsg below). For a lightweight
		// re-register (sendHistory=false) the session_state sent above already
		// told the client whether the session is mid-turn; these frames just
		// refresh the pane's toolbar/context mirrors.
		basic := agentConfigMsgBasic(rt.agent)
		full := agentConfigMsg(r.Context(), rt)
		s.decorateConfig(&basic)
		s.decorateConfig(&full)
		_ = ws.writeJSON(basic)
		_ = ws.writeJSON(full)
		// Derive llama.cpp reasoning-effort options for the (possibly
		// restored) model and push a correction when they differ from the
		// fallback set the configs above carried.
		s.maybeProbeReasoningEfforts(r.Context(), rt.agent)
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

// editorReadTypes and editorWriteTypes are the message types served by the
// /ws/editor socket (HandleWSEditor). They must stay in lockstep with the
// wsHandlers map (the main chat socket routes the same types): missing
// either registration silently drops the message. TestWSEditorHandlerParity
// asserts the two agree.
var editorReadTypes = []string{
	"fs_list", "fs_read", "fs_search",
	"git_status", "git_file_diff", "git_commit_message",
}
var editorWriteTypes = []string{
	"fs_write", "fs_replace", "fs_apply_patch",
	"git_commit", "git_stage", "git_unstage", "git_push",
}

// typeInList reports whether t is present in list (small linear scan — the
// lists are a handful of constants).
func typeInList(list []string, t string) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}

// HandleWSEditor serves the editor WebSocket endpoint (/ws/editor). It is the
// workspace-scoped counterpart of HandleWS: it handles only the filesystem
// and git message types in editorReadTypes/editorWriteTypes and ignores
// chat/session messages. The editor
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
		switch {
		case typeInList(editorReadTypes, msg.Type):
			s.handleFSReadMessage(ws, r.Context(), msg)
		case typeInList(editorWriteTypes, msg.Type):
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
		// The full list — INCLUDING nested (subagent) sessions — so the
		// sidebar renders persisted children under their parent row after a
		// page reload / late attach (subagent_started/finished events are
		// not replayed to connecting clients). The client skips nested
		// entries when building flat rows and groups them under parents.
		sessions, listErr := rt.agent.SessionListAll()
		if listErr != nil {
			writeNoticeError(ws, "sessions", fmt.Sprintf("Error: %v", listErr))
			return
		}
		if sessions == nil {
			sessions = []agent.SessionInfo{}
		}
		// The reply deliberately carries NO SessionID: the sessions payload
		// is connection-scoped sidebar state (the full saved list), not a
		// message for one session. Tagging it with the current session id
		// made the client route (and possibly drop) it when that id was not
		// the active pane — e.g. after another tab moved the global default.
		_ = ws.writeJSON(WSMessage{
			Type:     "sessions",
			Sessions: sessionEntries(sessions, s.registry.activeSet(), s.registry.turnActiveSet()),
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
			writeNoticeError(ws, "models", fmt.Sprintf("Error: %v", err))
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
		// UI-channel handler (model picker): the busy rejection must NOT
		// render into the chat transcript — it toasts as a model notice.
		writeNoticeError(ws, "model", errAgentBusy)
		return
	}
	a := rt.agent
	err := a.SelectModel(ctx, msg.Model)
	cfg := agentConfigMsgBasic(a)
	fillModelPricing(a, &cfg)
	s.decorateConfig(&cfg)
	rt.turnMu.Unlock()
	if err != nil {
		writeNoticeError(ws, "model", fmt.Sprintf("Error: %v", err))
		return
	}
	// Model is per-session: SelectModel above applied to this pane's
	// provider only. The workspace default (ws.Model) is only mutated by the
	// settings modal's default-profile save (provider_save), never here, and
	// no other session's provider is touched, so two panes can run different
	// models concurrently. The config echo goes to this pane only (its own
	// Mode/ThinkingLevel/Model).
	// Tokenization + echo off the read loop: ContextStats on a large
	// uncached session takes seconds, and the read loop serializes every
	// message (including pane switches).
	echoConfigOffLoop(ws, ctx, a, &cfg)
	// llama.cpp endpoints: derive the new model's true reasoning-effort
	// options from /props (+ /apply-template) and push a config update when
	// they differ from the fallback the echo above carried (the provider
	// caches per model, so repeat switches are no-ops).
	s.maybeProbeReasoningEfforts(ctx, a)
}

func (s *Server) handleWSSetMode(ws *wsConn, ctx context.Context, rt *sessionRuntime, msg WSMessage) {
	if !rt.acquireTurnForHandler(ws) {
		// UI-channel handler (mode picker): busy rejection as a notice.
		writeNoticeError(ws, "mode", errAgentBusy)
		return
	}
	a := rt.agent
	modeSet := false
	var cfg WSMessage
	if m, ok := agent.ParseMode(msg.Mode); ok {
		a.SetMode(m)
		modeSet = true
		cfg = agentConfigMsgBasic(a)
		s.decorateConfig(&cfg)
	}
	rt.turnMu.Unlock()
	if modeSet {
		// Echo off the read loop (tokenization can take seconds on a large
		// uncached session; the read loop serializes every message).
		echoConfigOffLoop(ws, ctx, a, &cfg)
	}
}

func (s *Server) handleWSSetThinkingLevel(ws *wsConn, ctx context.Context, rt *sessionRuntime, msg WSMessage) {
	if !rt.acquireTurnForHandler(ws) {
		// UI-channel handler (thinking-level picker): busy rejection as a
		// notice.
		writeNoticeError(ws, "thinking", errAgentBusy)
		return
	}
	a := rt.agent
	if s.isValidThinkingLevel(a, msg.ThinkingLevel) {
		a.SetThinkingLevel(agent.ThinkingLevel(msg.ThinkingLevel))
	}
	cfg := agentConfigMsgBasic(a)
	rt.turnMu.Unlock()
	fillModelPricing(a, &cfg)
	s.decorateConfig(&cfg)
	// Echo off the read loop (tokenization can take seconds on a large
	// uncached session; the read loop serializes every message).
	echoConfigOffLoop(ws, ctx, a, &cfg)
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
	// The config message carries independent settings. The working-dir
	// branch keeps its historical global-mode gate; the feature-flag
	// branches (Board / Subagent / SubagentMaxDepth) are project settings
	// and work in ANY mode — the whole point of the settings modal; the
	// runtime-config branch (ConfigFields) applies the settings-modal
	// options (live or restart-staged, see handleWSRuntimeConfig).
	if msg.WorkingDir != "" {
		s.handleWSWorkingDir(ws, ctx, pane, msg)
	}
	if msg.Board != "" || msg.Subagent != "" || msg.SubagentMaxDepth != 0 || msg.SubagentMaxConcurrent != 0 {
		s.handleWSFeatureFlags(ws, msg)
	}
	if len(msg.ConfigFields) > 0 {
		s.handleWSRuntimeConfig(ws, msg)
	}
}

// handleWSWorkingDir handles the working-dir branch of the config message.
func (s *Server) handleWSWorkingDir(ws *wsConn, ctx context.Context, pane **sessionRuntime, msg WSMessage) {
	// Changing the working directory is only allowed in global mode: in
	// project mode the server is scoped to one project directory and
	// sessions persist under it, so re-pointing the workspace would orphan
	// sessions and escape the project boundary. The TUI's /dir command is a
	// separate path (not web mode) and is unaffected.
	if !s.ws.GlobalMode {
		writeNoticeError(ws, "workspace", "Error: changing the working directory is only allowed in global mode (start gogen with --global)")
		return
	}
	absDir, err := filepath.Abs(msg.WorkingDir)
	if err != nil {
		writeNoticeError(ws, "workspace", fmt.Sprintf("Error: invalid path: %v", err))
		return
	}
	info, err := os.Stat(absDir)
	if err != nil || !info.IsDir() {
		writeNoticeError(ws, "workspace", fmt.Sprintf("Error: directory does not exist: %s", absDir))
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
				writeNoticeError(ws, "workspace", workingDirSkipMessage(absDir, skipped))
			}
			return
		}
		cfg := agentConfigMsgBasic(a)
		paneRT.turnMu.RUnlock()
		accum := a.SnapshotUsageAccum()
		applyContextStats(&cfg, a.ContextStats(ctx), &accum)
		echo := WSMessage{Type: "config", WorkingDir: absDir, Model: cfg.Model, ContextLimit: cfg.ContextLimit, UsedTokens: cfg.UsedTokens, UsedSource: cfg.UsedSource, UsedPercent: cfg.UsedPercent, CompactAt: cfg.CompactAt, MessageCount: cfg.MessageCount, NearCompact: cfg.NearCompact, WarnNearCompact: cfg.WarnNearCompact, ToolTruncated: cfg.ToolTruncated, Mode: cfg.Mode, GlobalMode: cfg.GlobalMode, Board: cfg.Board, Subagent: cfg.Subagent, SubagentMaxDepth: cfg.SubagentMaxDepth}
		s.decorateConfig(&echo)
		_ = ws.writeJSON(echo)
		if len(skipped) > 0 {
			writeNoticeError(ws, "workspace", workingDirSkipMessage(absDir, skipped))
		}
	}(*pane)
}

// parseOnOff parses a config-WS on/off value. ok is false for anything that
// is not a recognized on/off spelling (see onoff.Parse).
func parseOnOff(v string) (on, ok bool) {
	return onoff.Parse(v)
}

// handleWSFeatureFlags handles the Board / Subagent / SubagentMaxDepth
// branches of the config message (the live settings-modal toggles). Any
// invalid value rejects the whole request with an error reply; on success
// the workspace flags are updated, every live session agent is swept, the
// effective config is persisted (durability — activation is the sweep), and
// a fresh config push is broadcast to all clients so every tab updates
// instantly.
func (s *Server) handleWSFeatureFlags(ws *wsConn, msg WSMessage) {
	var board, boardSet bool
	var subagent, subagentSet bool
	if msg.Board != "" {
		var ok bool
		board, ok = parseOnOff(msg.Board)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid board value %q (want on or off)", msg.Board))
			return
		}
		boardSet = true
	}
	if msg.Subagent != "" {
		var ok bool
		subagent, ok = parseOnOff(msg.Subagent)
		if !ok {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid subagent value %q (want on or off)", msg.Subagent))
			return
		}
		subagentSet = true
	}
	if msg.SubagentMaxDepth < 0 {
		writeNoticeError(ws, "settings", "Error: subagentMaxDepth must be >= 0")
		return
	}
	if msg.SubagentMaxConcurrent < 0 {
		writeNoticeError(ws, "settings", "Error: subagentMaxConcurrent must be >= 0")
		return
	}
	if boardSet {
		s.ws.SetBoardEnabled(board)
	}
	if subagentSet {
		s.ws.SetSubagentEnabled(subagent)
	}
	if msg.SubagentMaxDepth > 0 {
		s.ws.SetSubagentMaxDepth(msg.SubagentMaxDepth)
	}
	if msg.SubagentMaxConcurrent > 0 {
		s.ws.SetSubagentMaxConcurrent(msg.SubagentMaxConcurrent)
	}
	// Sweep + persist + broadcast OFF the read loop: the persistence is a
	// file write and the broadcast fans out to every attached socket.
	go func() {
		s.applyFeatureFlagsToAll()
		if s.config != nil {
			s.persistConfig(s.effectiveConfig())
		}
		s.broadcastConfigAll()
	}()
}

// effectiveConfig returns the config snapshot used for persistence: the
// startup config with every live-mutable value overlaid (feature flags,
// registered provider list, runtime overlay incl. restart-staged settings).
// A later single-field persist must never revert an earlier live change.
// Returns nil when the server has no startup config (tests).
func (s *Server) effectiveConfig() *config.Config {
	if s == nil || s.config == nil {
		return nil
	}
	out := *s.config
	r := s.ws.GetRuntimeConfig()
	// Live-adjustable fields: the runtime overlay wins (it is seeded from
	// the startup config, so unchanged fields persist their original
	// values).
	out.OpenAIKey = r.OpenAIKey
	out.OpenAIModel = r.OpenAIModel
	out.OpenAIURL = r.OpenAIURL
	out.ContextLimit = r.ContextLimit
	out.CompactThreshold = r.CompactThreshold
	out.CompactKeepRecentMessages = r.CompactKeepRecentMessages
	out.MaxToolResultBytes = r.MaxToolResultBytes
	out.CompactReserveTokens = r.CompactReserveTokens
	out.CompactLastResort = r.CompactLastResort
	out.CommandSafetyMode = r.CommandSafetyMode
	out.CommandAllowlist = r.CommandAllowlist
	out.DeleteApproval = r.DeleteApproval
	out.CommandSandbox = r.CommandSandbox
	out.CommandTimeoutSecs = r.CommandTimeoutSecs
	out.TreeSitter = r.TreeSitter
	out.TreeSitterLangs = r.TreeSitterLangs
	out.WebFetch = r.WebFetch
	out.WebSearch = r.WebSearch
	out.WebSearchBackend = r.WebSearchBackend
	out.WebSearchAPIKey = r.WebSearchAPIKey
	out.WebAllowedDomains = r.WebAllowedDomains
	out.WebFetchMode = r.WebFetchMode
	out.PreserveReasoning = r.PreserveReasoning
	out.SessionMaxCount = r.SessionMaxCount
	out.SessionMaxAgeDays = r.SessionMaxAgeDays
	out.WebApprovalHoldSecs = r.WebApprovalHoldSecs
	// Restart-staged settings (applied on the next start).
	out.WebBind = r.WebBind
	out.WebAllowedOrigins = r.WebAllowedOrigins
	out.WebAuthToken = r.WebAuthToken
	out.WebTLSCertFile = r.WebTLSCertFile
	out.WebTLSKeyFile = r.WebTLSKeyFile
	out.WebMaxActiveSessions = r.WebMaxActiveSessions
	out.MCP = r.MCP
	out.MCPServers = r.MCPServers
	// Feature flags + provider list live in their own workspace stores.
	out.Board = onOff(s.ws.GetBoardEnabled())
	out.Subagent = onOff(s.ws.GetSubagentEnabled())
	out.SubagentMaxDepth = s.ws.GetSubagentMaxDepth()
	out.SubagentMaxConcurrent = s.ws.GetSubagentMaxConcurrent()
	out.SubagentModel = r.SubagentModel
	out.SubagentThinkingLevel = r.SubagentThinkingLevel
	out.BoardStartPrompt = r.BoardStartPrompt
	out.SystemPrompt = r.SystemPrompt
	out.SubagentPrompt = r.SubagentPrompt
	out.OpenAIProviders = s.ws.GetOpenAIProviders()
	return &out
}

// applyFeatureFlagsToAll syncs the workspace feature flags to every live
// session agent so the board / subagent tools appear or disappear
// immediately. The flag stores are atomic and per-turn tool derivation
// (llmTools/AllowedToolNames/executeTool) reads them, so no turn locks and
// no handler-map rebuild are needed; a running turn is never interrupted.
func (s *Server) applyFeatureFlagsToAll() {
	board := s.ws.GetBoardEnabled()
	subagent := s.ws.GetSubagentEnabled()
	depth := s.ws.GetSubagentMaxDepth()
	concurrent := s.ws.GetSubagentMaxConcurrent()
	// Enabling the board creates the shared manager (data from a previous
	// enable persists; disabling keeps it so re-enabling restores the
	// board).
	var bm *agent.BoardManager
	if board {
		bm = s.ws.ensureBoardManager()
	}
	for _, id := range s.registry.activeIDs() {
		rt, ok := s.registry.get(id)
		if !ok {
			continue
		}
		a := rt.agent
		a.SetBoardEnabled(board)
		a.SetSubagentsEnabled(subagent)
		a.SetSubagentMaxDepth(depth)
		a.SetSubagentMaxConcurrent(concurrent)
		a.SetBoardManager(bm)
	}
}

// broadcastConfigAll pushes a fresh config snapshot to every attached client
// of every live session (the settings modal syncs across tabs from these).
func (s *Server) broadcastConfigAll() {
	for _, id := range s.registry.activeIDs() {
		if rt, ok := s.registry.get(id); ok {
			msg := agentConfigMsgBasic(rt.agent)
			s.decorateConfig(&msg)
			rt.broadcast(msg)
		}
	}
}

// decorateConfig fills the workspace-level config fields on a config
// message: the registered provider list (never the keys), the config file
// path the storage warning renders, the live runtime-config values the
// settings modal displays, and the restart-pending list for the banner.
// Cheap accessor reads; safe without the session turn lock.
func (s *Server) decorateConfig(msg *WSMessage) {
	if s == nil || msg == nil {
		return
	}
	msg.Providers = s.providerEntries()
	msg.ConfigFilePath = s.configFilePath()
	r := s.ws.GetRuntimeConfig()
	msg.CommandSafetyMode = r.CommandSafetyMode
	msg.CommandAllowlist = r.CommandAllowlist
	msg.DeleteApproval = r.DeleteApproval
	msg.CommandSandbox = r.CommandSandbox
	msg.CommandTimeoutSecs = r.CommandTimeoutSecs
	msg.ContextLimitConfig = r.ContextLimit
	msg.CompactThreshold = r.CompactThreshold
	msg.CompactKeepRecentMessages = r.CompactKeepRecentMessages
	msg.MaxToolResultBytes = r.MaxToolResultBytes
	msg.CompactReserveTokens = r.CompactReserveTokens
	msg.CompactLastResort = r.CompactLastResort
	msg.WebFetch = r.WebFetch
	msg.WebSearch = r.WebSearch
	msg.WebSearchBackend = r.WebSearchBackend
	msg.WebSearchAPIKeySet = r.WebSearchAPIKey != ""
	msg.WebAllowedDomains = r.WebAllowedDomains
	msg.WebFetchMode = r.WebFetchMode
	msg.TreeSitter = r.TreeSitter
	msg.TreeSitterLangs = r.TreeSitterLangs
	msg.PreserveReasoning = r.PreserveReasoning
	msg.SubagentMaxConcurrent = s.ws.GetSubagentMaxConcurrent()
	msg.SubagentModel = &r.SubagentModel
	msg.SubagentThinkingLevel = &r.SubagentThinkingLevel
	msg.BoardStartPrompt = agent.ResolvePromptTemplate(r.BoardStartPrompt, agent.DefaultBoardStartPrompt)
	msg.SystemPrompt = agent.ResolvePromptTemplate(r.SystemPrompt, agent.DefaultSystemPromptTemplate())
	msg.SubagentPrompt = agent.ResolvePromptTemplate(r.SubagentPrompt, agent.DefaultSubagentPrompt)
	msg.SessionMaxCount = r.SessionMaxCount
	msg.SessionMaxAgeDays = r.SessionMaxAgeDays
	msg.WebApprovalHoldSecs = r.WebApprovalHoldSecs
	msg.WebBind = r.WebBind
	msg.WebAllowedOrigins = r.WebAllowedOrigins
	msg.WebAuthTokenSet = r.WebAuthToken != ""
	msg.WebTLSCertFile = r.WebTLSCertFile
	msg.WebTLSKeyFile = r.WebTLSKeyFile
	msg.WebMaxActiveSessions = r.WebMaxActiveSessions
	msg.MCP = r.MCP
	msg.MCPServers = s.mcpEntries()
	msg.RestartRequired = s.restartPendingFields()
}

// providerEntries projects the registered provider list for the client: the
// implicit default profile (built from the legacy config fields, live-
// editable but not deletable) followed by the live additional providers.
// Keys are never pushed — only the apiKeySet flag.
func (s *Server) providerEntries() []ProviderEntry {
	out := make([]ProviderEntry, 0, 1+len(s.ws.GetOpenAIProviders()))
	r := s.ws.GetRuntimeConfig()
	def := ProviderEntry{
		Name:      "default",
		BaseURL:   r.OpenAIURL,
		Model:     r.OpenAIModel,
		APIKeySet: r.OpenAIKey != "",
	}
	out = append(out, def)
	for _, p := range s.ws.GetOpenAIProviders() {
		out = append(out, ProviderEntry{
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			Model:     p.Model,
			APIKeySet: p.APIKey != "",
			Deletable: true,
		})
	}
	return out
}

// configFilePath returns where the effective config is persisted: the
// project .gogen/gogen.conf in project mode, the global config file in
// global mode. Drives the provider-key storage warning in the settings UI.
func (s *Server) configFilePath() string {
	if s.ws.GlobalMode {
		return projectfile.GlobalConfigPath()
	}
	return projectfile.DefaultSavePath(s.ws.GetWorkingDir())
}

// persistConfig writes the effective config so a live toggle survives a
// restart. Project mode writes .gogen/gogen.conf; global mode writes the
// global config file. The write is best-effort (log on failure) — the live
// toggle is already applied to the running process.
//
// Secrets (openai_api_key, MCP server env) are preserved when the EXISTING
// file already contains them: the toggle rewrite must never drop a key the
// user stored in the file (IncludeSecrets=false would rewrite the file
// without it). Keys that only ever came from the environment stay out of
// the file, exactly as before.
func (s *Server) persistConfig(cfg *config.Config) {
	var err error
	if s.ws.GlobalMode {
		err = projectfile.SaveGlobalConfig(cfg, projectfile.WriteOptions{
			IncludeSecrets: projectfile.ConfigFileHasSecrets(projectfile.GlobalConfigPath()),
		})
	} else {
		path := projectfile.DefaultSavePath(s.ws.GetWorkingDir())
		includeSecrets := projectfile.ConfigFileHasSecrets(path)
		if !includeSecrets {
			// The user's config may live in a .md front matter with no
			// .gogen/gogen.conf yet: creating a key-less .conf here would
			// shadow the .md's key (a .conf takes precedence on load).
			if cfgPath, ok := projectfile.DiscoverConfigPath(s.ws.GetWorkingDir()); ok {
				includeSecrets = projectfile.ConfigFileHasSecrets(cfgPath)
			}
		}
		err = projectfile.SaveConfig(path, "", cfg, "", projectfile.WriteOptions{
			IncludeSecrets: includeSecrets,
		})
	}
	if err != nil {
		log.Printf("config save failed: %v", err)
	}
}

// persistConfigForced writes the effective config with secrets forced on.
// Provider saves through the UI always persist their API keys (the user
// entered them explicitly and expects them stored); projectfile writes the
// file 0600 in that case. Side effect: any legacy openai_api_key that only
// came from the environment is also persisted on this write — accepted,
// since the user just opted into storing provider keys.
func (s *Server) persistConfigForced(cfg *config.Config) {
	if s == nil || s.ws == nil || cfg == nil {
		return
	}
	var err error
	if s.ws.GlobalMode {
		err = projectfile.SaveGlobalConfig(cfg, projectfile.WriteOptions{IncludeSecrets: true})
	} else {
		path := projectfile.DefaultSavePath(s.ws.GetWorkingDir())
		err = projectfile.SaveConfig(path, "", cfg, "", projectfile.WriteOptions{IncludeSecrets: true})
	}
	if err != nil {
		log.Printf("config save failed: %v", err)
	}
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

	// Live terminal tabs for shell tools: a tab is opened lazily on the
	// first output chunk (which carries the exact command string), fed by
	// a per-tool batcher, and closed on tool end. Both maps are keyed by
	// the tool call ID, which doubles as the terminal tab ID. They are
	// accessed from the exec pipe goroutine (OnToolOutput) and the stream
	// goroutine (OnToolResult), so access is mutex-guarded.
	var termMu sync.Mutex
	termBatches := map[string]*streamutil.TokenBatcher{}
	termOpened := map[string]struct{}{}

	handlers := rt.buildStreamHandlers(appCtx, write, tokens, &termMu, termBatches, termOpened)

	out, err := rt.agent.StreamProcessInputWithImages(appCtx, content, images, handlers)
	if err != nil {
		if appCtx.Err() != nil {
			tokens.Flush()
			// Broadcast directly (not via write, which early-returns on a
			// cancelled ctx) so the cancellation reaches attached clients.
			rt.broadcast(WSMessage{Type: "cancelled", Content: "Cancelled.", SessionID: rt.agent.SessionID})
			return out, err
		}
		tokens.Flush()
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
	// No trailing stream_end here: every round already wrote one via the
	// OnStreamEnd handler above, and turn_end marks the turn boundary.
	if ctxMsg.Type != "" {
		write(ctxMsg)
	}
	return out, nil
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
		OnCondensed: func(note string) {
			// Last-resort condensation announcement (Phase 0e): the
			// client renders it as a banner above the composer.
			write(WSMessage{Type: "condensed", Content: note, SessionID: rt.agent.SessionID})
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

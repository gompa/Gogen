package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	sesspkg "gogen/internal/session"

	"github.com/gorilla/websocket"
)

//go:embed all:web
var webAssets embed.FS

var errWSClosed = errors.New("websocket closed")

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex

	sendQ chan WSMessage
	quit  chan struct{} // closed by closeSend to stop writers + writeLoop
	done  chan struct{} // closed when writeLoop exits, so writeJSON fails fast
	once  sync.Once
}

const (
	wsSendQueueSize   = 4096
	wsPingInterval    = 30 * time.Second
	wsWriteTimeout    = 30 * time.Second
	wsReadTimeout     = 60 * time.Second
	wsTurnAcquireWait = 150 * time.Millisecond
	wsStreamDrainWait = 45 * time.Second
)

func drainStreamErr(ch chan error) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(wsStreamDrainWait):
		log.Printf("warning: timed out waiting for stream goroutine to exit")
	}
}

// tryAcquireTurn waits briefly for turnMu (e.g. after cancelling our own stream).
// Returns false if another client still holds the agent.
func (s *Server) tryAcquireTurn(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if s.turnMu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newWSConn(conn *websocket.Conn) *wsConn {
	w := &wsConn{
		conn:  conn,
		sendQ: make(chan WSMessage, wsSendQueueSize),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go w.writeLoop()
	return w
}

func (w *wsConn) writeLoop() {
	// Closing the conn on exit is critical: it tears down the read loop (so
	// HandleWS cleans up) AND makes the browser fire onclose so it reconnects.
	// Without this, a single transient write error kills the writer silently
	// while the LLM keeps "sending" into a dead queue and the UI freezes.
	defer w.conn.Close()
	defer close(w.done)
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.quit:
			return
		case msg := <-w.sendQ:
			w.mu.Lock()
			if err := w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				w.mu.Unlock()
				log.Printf("websocket set write deadline: %v", err)
				return
			}
			err := w.conn.WriteJSON(msg)
			w.mu.Unlock()
			if err != nil {
				return
			}
		case <-ticker.C:
			// Pings detect half-open connections (e.g. NAT/proxy idle
			// timeouts, backgrounded tabs) that pass write deadlines but
			// never reach the browser. A failed ping kills the writer,
			// triggering teardown + reconnect via the deferred Close.
			w.mu.Lock()
			if err := w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
				w.mu.Unlock()
				log.Printf("websocket set write deadline: %v", err)
				return
			}
			err := w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			w.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (w *wsConn) closeSend() {
	w.once.Do(func() {
		// Signal quit instead of closing sendQ so concurrent writeJSON
		// sends cannot panic on a closed channel.
		close(w.quit)
	})
}

func (w *wsConn) writeJSON(v WSMessage) error {
	err := w.enqueueJSON(v)
	if err != nil && !errors.Is(err, errWSClosed) {
		log.Printf("websocket write (%s): %v", v.Type, err)
	}
	return err
}

func (w *wsConn) enqueueJSON(v WSMessage) error {
	if w == nil || w.conn == nil {
		return errWSClosed
	}
	select {
	case <-w.quit:
		return errWSClosed
	case <-w.done:
		return errWSClosed
	default:
	}
	select {
	case w.sendQ <- v:
		return nil
	case <-w.quit:
		return errWSClosed
	case <-w.done:
		return errWSClosed
	default:
		// Queue full: block briefly rather than stall the LLM stream reader forever.
		select {
		case w.sendQ <- v:
			return nil
		case <-w.quit:
			return errWSClosed
		case <-w.done:
			return errWSClosed
		case <-time.After(5 * time.Second):
			return fmt.Errorf("websocket send queue full")
		}
	}
}

type Server struct {
	agent          *agent.Agent
	config         *config.Config
	agentMu        sync.RWMutex
	turnMu         sync.Mutex // serializes agent-mutating work across WS clients
	allowedOrigins map[string]struct{}
	authToken      string
	tlsCertFile    string
	tlsKeyFile     string
	connLimiter    *rateLimitState
	upgradeLimiter *ipLimiter
}

type ModelEntry struct {
	ID           string `json:"id"`
	ContextLimit int    `json:"contextLimit,omitempty"`
	Current      bool   `json:"current,omitempty"`
}

type SessionEntry struct {
	ID           string `json:"id"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
	MessageCount int    `json:"messageCount,omitempty"`
	Label        string `json:"label,omitempty"`
	Current      bool   `json:"current,omitempty"`
}

type HistoryToolCall struct {
	ID   string                 `json:"id"`
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

type HistoryEntry struct {
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	Reasoning  string            `json:"reasoning,omitempty"`
	ToolCalls  []HistoryToolCall `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
}

type WSMessage struct {
	Type             string                 `json:"type"`
	Content          string                 `json:"content,omitempty"`
	Tool             string                 `json:"tool,omitempty"`
	ToolCallID       string                 `json:"toolCallId,omitempty"`
	Index            int                    `json:"index,omitempty"`
	ArgsDelta        string                 `json:"argsDelta,omitempty"`
	Args             map[string]interface{} `json:"args,omitempty"`
	Result           string                 `json:"result,omitempty"`
	Success          bool                   `json:"success,omitempty"`
	ResultTruncated  bool                   `json:"resultTruncated,omitempty"`
	WorkingDir       string                 `json:"workingDir,omitempty"`
	Model            string                 `json:"model,omitempty"`
	ContextLimit     int                    `json:"contextLimit,omitempty"`
	UsedTokens       int                    `json:"usedTokens,omitempty"`
	UsedSource       string                 `json:"usedSource,omitempty"`
	PromptTokens     int                    `json:"promptTokens,omitempty"`
	CompletionTokens int                    `json:"completionTokens,omitempty"`
	CachedTokens     int                    `json:"cachedTokens,omitempty"`
	CompactAt        int                    `json:"compactAt,omitempty"`
	MessageCount     int                    `json:"messageCount,omitempty"`
	NearCompact      bool                   `json:"nearCompact,omitempty"`
	UsedPercent      float64                `json:"usedPercent,omitempty"`
	ToolTruncated    bool                   `json:"toolTruncated,omitempty"`
	Models           []ModelEntry           `json:"models,omitempty"`
	ApprovalID       string                 `json:"approvalId,omitempty"`
	Approved         bool                   `json:"approved,omitempty"`
	Paths            []string               `json:"paths,omitempty"`
	Reason           string                 `json:"reason,omitempty"`
	Mode             string                 `json:"mode,omitempty"`
	SessionID        string                 `json:"sessionId,omitempty"`
	SessionAction    string                 `json:"sessionAction,omitempty"`
	Sessions         []SessionEntry         `json:"sessions,omitempty"`
	History          []HistoryEntry         `json:"history,omitempty"`
	// Filesystem / git editor APIs
	Path       string              `json:"path,omitempty"`
	Pattern    string              `json:"pattern,omitempty"`
	Glob       string              `json:"glob,omitempty"`
	Language   string              `json:"language,omitempty"`
	Error      string              `json:"error,omitempty"`
	Entries    []FSEntry           `json:"entries,omitempty"`
	GitEntries []GitStatusEntry    `json:"gitEntries,omitempty"`
	Matches    []agent.SearchMatch `json:"matches,omitempty"`
	Truncated  bool                `json:"truncated,omitempty"`
	Original   string              `json:"original,omitempty"`
	Modified   string              `json:"modified,omitempty"`
	RequestID  string              `json:"requestId,omitempty"`
	Replacement string             `json:"replacement,omitempty"`
	Replaced   int                 `json:"replaced,omitempty"`
	FileCount  int                 `json:"fileCount,omitempty"`
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
	return &Server{
		agent:          a,
		config:         cfg,
		allowedOrigins: allowed,
		authToken:      token,
		tlsCertFile:    tlsCert,
		tlsKeyFile:     tlsKey,
		connLimiter:    newRateLimitState(defaultMaxWSConns),
		upgradeLimiter: newIPLimiter(5, 10), // 5 upgrades/sec/IP, burst 10
	}
}

func (s *Server) wsUpgrader() websocket.Upgrader {
	allowed := s.allowedOrigins
	return websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return checkWSOrigin(r, allowed)
		},
	}
}

func applyContextStats(msg *WSMessage, stats agent.TurnContext) {
	snap := stats.Snapshot
	if snap.Limit > 0 {
		msg.ContextLimit = snap.Limit
	}
	if snap.Used > 0 {
		msg.UsedTokens = snap.Used
	}
	msg.UsedSource = "estimated"
	msg.PromptTokens = stats.PromptTokens
	msg.CompletionTokens = stats.CompletionTokens
	msg.CachedTokens = stats.CachedTokens
	msg.CompactAt = snap.CompactAt
	msg.MessageCount = snap.MessageCount
	msg.NearCompact = snap.NearCompact
	msg.ToolTruncated = snap.ToolTruncated
	if snap.Limit > 0 {
		msg.UsedPercent = snap.Percent
	}
}

func (s *Server) agentConfigMsg(ctx context.Context) WSMessage {
	msg := WSMessage{
		Type:       "config",
		WorkingDir: s.agent.Executor.WorkingDir,
		Model:      s.agent.CurrentModel(),
		Mode:       s.agent.Mode.String(),
		SessionID:  s.agent.SessionID,
	}
	applyContextStats(&msg, s.agent.ContextStats(ctx))
	return msg
}

// agentConfigMsgBasic returns a config message without the expensive
// context-stats computation.  Use this on initial WS connect so the
// client receives config + history immediately; context stats follow
// via a separate "context" message.
func (s *Server) agentConfigMsgBasic() WSMessage {
	return WSMessage{
		Type:       "config",
		WorkingDir: s.agent.Executor.WorkingDir,
		Model:      s.agent.CurrentModel(),
		Mode:       s.agent.Mode.String(),
		SessionID:  s.agent.SessionID,
	}
}

func sessionEntries(list []agent.SessionInfo, currentID string) []SessionEntry {
	out := make([]SessionEntry, len(list))
	for i, s := range list {
		out[i] = SessionEntry{
			ID:           s.ID,
			UpdatedAt:    s.UpdatedAt,
			MessageCount: s.MessageCount,
			Label:        s.Label,
			Current:      s.ID == currentID,
		}
	}
	return out
}

func historyEntries(msgs []llm.Message) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "user":
			if m.Content == "" {
				continue
			}
			out = append(out, HistoryEntry{Role: m.Role, Content: m.Content})
		case "assistant":
			if m.Content == "" && len(m.ToolCalls) == 0 && m.Reasoning == "" {
				continue
			}
			entry := HistoryEntry{Role: m.Role, Content: m.Content, Reasoning: m.Reasoning}
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
			})
		}
	}
	return out
}

func (s *Server) writeSessionCommandResult(ws *wsConn, ctx context.Context, result agent.SessionCommandResult, err error) {
	resp := WSMessage{Type: "response"}
	if err != nil {
		resp.Content = fmt.Sprintf("Error: %v", err)
	} else {
		resp.Content = result.Output
		if result.Action == agent.SessionActionClearChat {
			resp.SessionAction = string(result.Action)
			_ = ws.writeJSON(WSMessage{Type: "clear_chat"})
		}
		if len(result.Sessions) > 0 {
			resp.Type = "sessions"
			resp.Sessions = sessionEntries(result.Sessions, s.agent.SessionID)
		}
	}
	cfg := s.agentConfigMsg(ctx)
	resp.SessionID = cfg.SessionID
	resp.Mode = cfg.Mode
	// Send history before context so the client can start painting immediately.
	_ = ws.writeJSON(resp)
	if len(result.History) > 0 {
		_ = ws.writeJSON(WSMessage{Type: "history", History: historyEntries(result.History)})
	}
	_ = ws.writeJSON(s.contextMsg(ctx))
	_ = ws.writeJSON(cfg)
}

func (s *Server) contextMsg(ctx context.Context) WSMessage {
	msg := WSMessage{Type: "context"}
	applyContextStats(&msg, s.agent.ContextStats(ctx))
	return msg
}

func (s *Server) modelEntries(models []llm.ModelInfo) []ModelEntry {
	out := make([]ModelEntry, len(models))
	for i, m := range models {
		out[i] = ModelEntry{
			ID:           m.ID,
			ContextLimit: m.ContextLimit,
			Current:      m.Current,
		}
	}
	return out
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.upgradeLimiter != nil && !s.upgradeLimiter.allow(clientIP(r)) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if s.connLimiter != nil && !s.connLimiter.acquireConn() {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	if s.connLimiter != nil {
		defer s.connLimiter.releaseConn()
	}
	upg := s.wsUpgrader()
	conn, err := upg.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Pong handler extends the read deadline whenever the browser replies to
	// our pings. If the client stops responding (tab closed, network gone),
	// the read deadline elapses, ReadJSON fails, and HandleWS tears down —
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
	defer ws.closeSend()
	session := newWSSession(ws)
	msgLimiter := newWSMessageLimiter()

	var streamCancel context.CancelFunc
	var streamErr chan error
	var streamMu sync.Mutex
	defer func() {
		streamMu.Lock()
		if streamCancel != nil {
			streamCancel()
			if streamErr != nil {
				drainStreamErr(streamErr)
			}
		}
		streamMu.Unlock()
	}()

	s.agentMu.RLock()
	msgs := append([]llm.Message(nil), s.agent.Messages...)
	cfgMsg := s.agentConfigMsgBasic()
	s.agentMu.RUnlock()
	_ = ws.writeJSON(cfgMsg)
	if len(msgs) > 0 {
		_ = ws.writeJSON(WSMessage{Type: "history", History: historyEntries(msgs)})
	}

	// Compute context stats asynchronously so the client can start
	// painting history immediately.  The context message arrives
	// shortly after; the client already handles late context updates.
	go func() {
		s.agentMu.RLock()
		ctxMsg := s.contextMsg(r.Context())
		s.agentMu.RUnlock()
		_ = ws.writeJSON(ctxMsg)
	}()

	incoming := make(chan WSMessage, 8)
	go func() {
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				close(incoming)
				return
			}
			if !msgLimiter.Allow() {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: rate limit exceeded"})
				continue
			}
			// Complete delete approvals here so they never sit behind a main-loop
			// turnMu.Lock() (stream holds turnMu while waiting for approval).
			if msg.Type == "delete_approval_response" {
				session.completeApproval(msg.ApprovalID, msg.Approved)
				continue
			}
			incoming <- msg
		}
	}()

	for msg := range incoming {
		switch msg.Type {
		case "delete_approval_response":
			// Already handled in the reader; keep for safety if ever enqueued.
			session.completeApproval(msg.ApprovalID, msg.Approved)
			continue
		case "fs_list", "fs_read", "fs_search", "git_status", "git_file_diff", "fs_replace":
			s.handleFSReadMessage(ws, r.Context(), msg)
			continue
		case "fs_write":
			s.handleFSWriteMessage(ws, msg)
			continue
		case "list_sessions":
			_, sessions, err := s.agent.FormatSessionListForUI()
			if err != nil {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
				continue
			}
			_ = ws.writeJSON(WSMessage{
				Type:      "sessions",
				Sessions:  sessionEntries(sessions, s.agent.SessionID),
				SessionID: s.agent.SessionID,
			})
			continue
		case "session_new":
			streamMu.Lock()
			if streamCancel != nil {
				streamCancel()
				prevErr := streamErr
				streamCancel = nil
				streamErr = nil
				streamMu.Unlock()
				drainStreamErr(prevErr)
			} else {
				streamMu.Unlock()
			}
			if !s.tryAcquireTurn(wsTurnAcquireWait) {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
				continue
			}
			result, _, err := s.agent.HandleSessionCommand(r.Context(), "/new", sesspkg.NewID())
			s.writeSessionCommandResult(ws, r.Context(), result, err)
			s.turnMu.Unlock()
			continue
		case "session_resume":
			streamMu.Lock()
			if streamCancel != nil {
				streamCancel()
				prevErr := streamErr
				streamCancel = nil
				streamErr = nil
				streamMu.Unlock()
				drainStreamErr(prevErr)
			} else {
				streamMu.Unlock()
			}
			if !s.tryAcquireTurn(wsTurnAcquireWait) {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
				continue
			}
			id := strings.TrimSpace(msg.SessionID)
			if id == "" {
				s.turnMu.Unlock()
				_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: sessionId is required"})
				continue
			}
			result, _, err := s.agent.HandleSessionCommand(r.Context(), "resume "+id, sesspkg.NewID())
			s.writeSessionCommandResult(ws, r.Context(), result, err)
			s.turnMu.Unlock()
			continue
		case "cancel":
			streamMu.Lock()
			if streamCancel != nil {
				streamCancel()
				prevErr := streamErr
				streamCancel = nil
				streamErr = nil
				streamMu.Unlock()
				drainStreamErr(prevErr)
			} else {
				streamMu.Unlock()
			}
			continue
		case "message":
			// Cancel any in-flight stream BEFORE taking turnMu.
			streamMu.Lock()
			if streamCancel != nil {
				streamCancel()
				prevErr := streamErr
				streamCancel = nil
				streamErr = nil
				streamMu.Unlock()
				drainStreamErr(prevErr)
			} else {
				streamMu.Unlock()
			}

			if !s.tryAcquireTurn(wsTurnAcquireWait) {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
				continue
			}
			if out, handled := agent.HandleHelpCommand(msg.Content, true, false); handled {
				s.turnMu.Unlock()
				_ = ws.writeJSON(WSMessage{Type: "response", Content: out})
				continue
			}
			if out, handled := s.agent.HandleModeCommand(msg.Content); handled {
				_ = ws.writeJSON(s.agentConfigMsg(r.Context()))
				s.turnMu.Unlock()
				_ = ws.writeJSON(WSMessage{Type: "response", Content: out})
				continue
			}
			if out, handled := s.agent.HandleContextCommand(r.Context(), msg.Content); handled {
				ctxMsg := s.contextMsg(r.Context())
				s.turnMu.Unlock()
				_ = ws.writeJSON(ctxMsg)
				_ = ws.writeJSON(WSMessage{Type: "response", Content: out})
				continue
			}
			if result, handled, err := s.agent.HandleSessionCommand(r.Context(), msg.Content, sesspkg.NewID()); handled {
				s.writeSessionCommandResult(ws, r.Context(), result, err)
				s.turnMu.Unlock()
				continue
			}
			if out, handled, err := s.agent.HandleModelsCommand(r.Context(), msg.Content); handled {
				resp := WSMessage{Type: "response", Content: out}
				if err != nil {
					resp.Content = fmt.Sprintf("Error: %v", err)
				} else {
					if models, listErr := s.agent.ListModels(r.Context()); listErr == nil && len(models) > 1 {
						resp.Type = "models"
						resp.Models = s.modelEntries(models)
					}
					cfg := s.agentConfigMsg(r.Context())
					resp.Model = cfg.Model
					resp.ContextLimit = cfg.ContextLimit
					resp.UsedTokens = cfg.UsedTokens
					resp.UsedSource = cfg.UsedSource
					resp.UsedPercent = cfg.UsedPercent
					if strings.HasPrefix(strings.TrimSpace(msg.Content), "/models ") || strings.HasPrefix(strings.TrimSpace(msg.Content), "models ") {
						_ = ws.writeJSON(cfg)
					}
				}
				s.turnMu.Unlock()
				_ = ws.writeJSON(resp)
				break
			}

			// Transfer turnMu ownership to the stream goroutine (do not Unlock here).
			streamMu.Lock()
			streamCtx, cancel := context.WithCancel(r.Context())
			streamCancel = cancel
			streamErr = make(chan error, 1)
			errCh := streamErr
			streamMu.Unlock()
			go func(content string, ctx context.Context, cancel context.CancelFunc, done chan error) {
				defer func() {
					streamMu.Lock()
					streamCancel = nil
					streamErr = nil
					streamMu.Unlock()
				}()
				defer func() { done <- nil }()
				defer s.turnMu.Unlock()
				defer func() {
					// Always notify the client the turn is over (success, error, or cancel).
					_ = ws.writeJSON(WSMessage{Type: "turn_end"})
				}()
				ctx = agent.ContextWithDeleteApprover(ctx, session.deleteApprover())
				var writeFailed atomic.Bool
				failWrite := sync.Once{}
				write := func(v WSMessage) {
					if ctx.Err() != nil {
						return
					}
					if err := ws.writeJSON(v); err != nil {
						// Do not keep streaming into a full/dead queue — cancel
						// the LLM and tear down so the browser reconnects.
						writeFailed.Store(true)
						failWrite.Do(func() {
							cancel()
							_ = ws.conn.Close()
						})
						return
					}
				}
				tokens := newWSTokenBatcher(write)

				handlers := &llm.StreamHandlers{
					OnStart: func() {
						write(WSMessage{Type: "thinking"})
					},
					OnRoundStart: func() {
						write(WSMessage{Type: "thinking"})
					},
					OnStreamOpened: func() {
						write(WSMessage{Type: "waiting"})
					},
					OnStreamActivity: func() {},
					OnThinkingToken:  tokens.thinkToken,
					OnToken:          tokens.streamToken,
					OnStreamEnd: func() {
						tokens.flush()
						write(WSMessage{Type: "stream_end"})
					},
					OnToolCallStart: func(index int, id, name string) {
						tokens.flush()
						write(WSMessage{
							Type:       "tool_call_start",
							Tool:       name,
							ToolCallID: id,
							Index:      index,
						})
					},
					OnToolCallArgsDelta: func(index int, id, name, argsDelta string) {
						write(WSMessage{
							Type:       "tool_call_delta",
							Tool:       name,
							ToolCallID: id,
							Index:      index,
							ArgsDelta:  argsDelta,
						})
					},
					OnToolCall: func(tc llm.ToolCall) {
						tokens.flush()
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
					OnToolResult: func(id, name, result string, success bool) {
						truncated := false
						const maxResult = 128 * 1024
						origLen := len(result)
						if origLen > maxResult {
							result = result[:maxResult] + fmt.Sprintf("\n… truncated (%d bytes total)", origLen)
							truncated = true
						}
						write(WSMessage{
							Type:            "tool_result",
							Tool:            name,
							ToolCallID:      id,
							Result:          result,
							Success:         success,
							ResultTruncated: truncated,
						})
					},
				}

				_, err := s.agent.StreamProcessInput(ctx, content, handlers)
				if err != nil {
					if ctx.Err() != nil {
						tokens.flush()
						if !writeFailed.Load() {
							_ = ws.writeJSON(WSMessage{Type: "cancelled", Content: "Cancelled."})
						}
						return
					}
					if ctx.Err() == nil {
						tokens.flush()
						write(WSMessage{Type: "stream_end"})
						write(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
						log.Printf("stream error: %v", err)
					}
					return
				}
				if persistErr := s.agent.ConsumePersistError(); persistErr != nil {
					write(WSMessage{Type: "response", Content: fmt.Sprintf("Warning: failed to save session: %v", persistErr)})
				}
				tokens.flush()
				write(WSMessage{Type: "stream_end"})
				write(s.contextMsg(r.Context()))
			}(msg.Content, streamCtx, cancel, errCh)
			// Do not block here — keep reading so delete_approval_response can complete.
			continue
		case "list_models":
			models, err := s.agent.ListModels(r.Context())
			if err != nil {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
				continue
			}
			_ = ws.writeJSON(WSMessage{
				Type:   "models",
				Model:  s.agent.CurrentModel(),
				Models: s.modelEntries(models),
			})
			continue
		case "set_model":
			streamMu.Lock()
			if streamCancel != nil {
				streamCancel()
				prevErr := streamErr
				streamCancel = nil
				streamErr = nil
				streamMu.Unlock()
				drainStreamErr(prevErr)
			} else {
				streamMu.Unlock()
			}
			if !s.tryAcquireTurn(wsTurnAcquireWait) {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
				continue
			}
			err := s.agent.SelectModel(r.Context(), msg.Model)
			cfg := s.agentConfigMsg(r.Context())
			s.turnMu.Unlock()
			if err != nil {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
				continue
			}
			_ = ws.writeJSON(cfg)
			continue
		case "set_mode":
			streamMu.Lock()
			if streamCancel != nil {
				streamCancel()
				prevErr := streamErr
				streamCancel = nil
				streamErr = nil
				streamMu.Unlock()
				drainStreamErr(prevErr)
			} else {
				streamMu.Unlock()
			}
			if !s.tryAcquireTurn(wsTurnAcquireWait) {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
				continue
			}
			modeSet := false
			if m, ok := agent.ParseMode(msg.Mode); ok {
				s.agent.SetMode(m)
				modeSet = true
			}
			s.turnMu.Unlock()
			if modeSet {
				_ = ws.writeJSON(s.agentConfigMsg(r.Context()))
			}
			continue
		case "config":
			absDir, err := filepath.Abs(msg.WorkingDir)
			if err != nil {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: invalid path: %v", err)})
				continue
			}
			info, err := os.Stat(absDir)
			if err != nil || !info.IsDir() {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: directory does not exist: %s", absDir)})
				continue
			}
			streamMu.Lock()
			if streamCancel != nil {
				streamCancel()
				prevErr := streamErr
				streamCancel = nil
				streamErr = nil
				streamMu.Unlock()
				drainStreamErr(prevErr)
			} else {
				streamMu.Unlock()
			}
			if !s.tryAcquireTurn(wsTurnAcquireWait) {
				_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
				continue
			}
			s.agent.SetWorkingDir(absDir)
			s.config.WorkingDir = absDir
			s.turnMu.Unlock()
			cfg := s.agentConfigMsg(r.Context())
			_ = ws.writeJSON(WSMessage{Type: "config", WorkingDir: absDir, Model: cfg.Model, ContextLimit: cfg.ContextLimit, UsedTokens: cfg.UsedTokens, UsedSource: cfg.UsedSource, UsedPercent: cfg.UsedPercent, CompactAt: cfg.CompactAt, MessageCount: cfg.MessageCount, NearCompact: cfg.NearCompact, ToolTruncated: cfg.ToolTruncated, Mode: cfg.Mode})
		}
	}
}

func (s *Server) HandleStatic(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	// Bootstrap: accept ?token= once, set HttpOnly cookie, redirect without query.
	if s.authToken != "" {
		if q := strings.TrimSpace(r.URL.Query().Get("token")); q != "" {
			if q == s.authToken {
				setAuthCookie(w, s.authToken, secure)
				http.Redirect(w, r, r.URL.Path, http.StatusFound)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if !s.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	path := r.URL.Path
	if path == "/" || path == "" {
		content, err := webAssets.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(content)
		return
	}

	// Serve embedded assets under /monaco/... (and future static paths).
	rel := strings.TrimPrefix(path, "/")
	if strings.Contains(rel, "..") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := "web/" + rel
	content, err := webAssets.ReadFile(name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentTypeForExt(filepath.Ext(name)))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(content)
}

func contentTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".ttf":
		return "font/ttf"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".map":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

func (s *Server) checkAuth(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		if strings.TrimSpace(c.Value) == s.authToken {
			return true
		}
	}
	if tok := strings.TrimSpace(r.Header.Get("X-Gogen-Token")); tok != "" && tok == s.authToken {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		tok := strings.TrimSpace(auth[7:])
		if tok == s.authToken {
			return true
		}
	}
	return false
}

func (s *Server) Start(addr string) error {
	if !isLoopbackBind(addr) {
		if s.authToken == "" {
			return fmt.Errorf("non-loopback bind %q requires GOGEN_WEB_TOKEN (or web_auth_token) for authentication", addr)
		}
		if s.tlsCertFile == "" || s.tlsKeyFile == "" {
			return fmt.Errorf("non-loopback bind %q requires TLS: set GOGEN_WEB_TLS_CERT and GOGEN_WEB_TLS_KEY (or web_tls_cert_file / web_tls_key_file)", addr)
		}
		log.Printf("listening on non-loopback %s with token auth and TLS", addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWS)
	mux.HandleFunc("/", s.HandleStatic)
	if s.tlsCertFile != "" && s.tlsKeyFile != "" {
		return http.ListenAndServeTLS(addr, s.tlsCertFile, s.tlsKeyFile, mux)
	}
	return http.ListenAndServe(addr, mux)
}

func isLoopbackBind(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	} else if strings.HasPrefix(addr, ":") {
		host = "0.0.0.0"
	}
	host = strings.TrimSpace(strings.ToLower(host))
	// Empty host in ":port" form means all interfaces.
	if host == "" {
		return false
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		ip := net.ParseIP(strings.Trim(host, "[]"))
		return ip != nil && ip.IsLoopback()
	}
}

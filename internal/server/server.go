package server

import (
	"context"
	"embed"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	sesspkg "gogen/internal/session"

	"github.com/gorilla/websocket"
)

//go:embed web/*
var webAssets embed.FS

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex

	sendQ chan WSMessage
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
		case msg, ok := <-w.sendQ:
			if !ok {
				return
			}
			w.mu.Lock()
			_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
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
			_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
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
		close(w.sendQ)
	})
}

func (w *wsConn) writeJSON(v WSMessage) error {
	if w == nil || w.conn == nil {
		return fmt.Errorf("websocket closed")
	}
	select {
	case <-w.done:
		return fmt.Errorf("websocket closed")
	default:
	}
	select {
	case w.sendQ <- v:
		return nil
	default:
		// Queue full: block briefly rather than stall the LLM stream reader forever.
		select {
		case w.sendQ <- v:
			return nil
		case <-w.done:
			return fmt.Errorf("websocket closed")
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

type HistoryEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type WSMessage struct {
	Type               string                 `json:"type"`
	Content            string                 `json:"content,omitempty"`
	Tool               string                 `json:"tool,omitempty"`
	ToolCallID         string                 `json:"toolCallId,omitempty"`
	Index              int                    `json:"index,omitempty"`
	ArgsDelta          string                 `json:"argsDelta,omitempty"`
	Args               map[string]interface{} `json:"args,omitempty"`
	Result             string                 `json:"result,omitempty"`
	Success            bool                   `json:"success,omitempty"`
	ResultTruncated    bool                   `json:"resultTruncated,omitempty"`
	WorkingDir         string                 `json:"workingDir,omitempty"`
	Model              string                 `json:"model,omitempty"`
	ContextLimit       int                    `json:"contextLimit,omitempty"`
	UsedTokens         int                    `json:"usedTokens,omitempty"`
	UsedSource         string                 `json:"usedSource,omitempty"`
	PromptTokens       int                    `json:"promptTokens,omitempty"`
	CompletionTokens   int                    `json:"completionTokens,omitempty"`
	CachedTokens       int                    `json:"cachedTokens,omitempty"`
	CompactAt          int                    `json:"compactAt,omitempty"`
	MessageCount       int                    `json:"messageCount,omitempty"`
	NearCompact        bool                   `json:"nearCompact,omitempty"`
	UsedPercent        float64                `json:"usedPercent,omitempty"`
	ToolTruncated      bool                   `json:"toolTruncated,omitempty"`
	Models             []ModelEntry           `json:"models,omitempty"`
	ApprovalID         string                 `json:"approvalId,omitempty"`
	Approved           bool                   `json:"approved,omitempty"`
	Paths              []string               `json:"paths,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Mode               string                 `json:"mode,omitempty"`
	SessionID          string                 `json:"sessionId,omitempty"`
	SessionAction      string                 `json:"sessionAction,omitempty"`
	Sessions           []SessionEntry         `json:"sessions,omitempty"`
	History            []HistoryEntry          `json:"history,omitempty"`
}

func NewServer(a *agent.Agent, cfg *config.Config) *Server {
	allowed := parseAllowedOrigins("")
	token := ""
	if cfg != nil {
		allowed = parseAllowedOrigins(cfg.WebAllowedOrigins)
		token = strings.TrimSpace(cfg.WebAuthToken)
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GOGEN_WEB_TOKEN"))
	}
	return &Server{
		agent:          a,
		config:         cfg,
		allowedOrigins: allowed,
		authToken:      token,
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
		if m.Role == "user" || m.Role == "assistant" {
			if m.Content != "" {
				out = append(out, HistoryEntry{Role: m.Role, Content: m.Content})
			}
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
	// ContextStats may trigger a network call — compute outside any agentMu lock.
	ctxMsg := s.contextMsg(ctx)
	cfg := s.agentConfigMsg(ctx)
	resp.SessionID = cfg.SessionID
	resp.Mode = cfg.Mode
	_ = ws.writeJSON(ctxMsg)
	_ = ws.writeJSON(resp)
	if len(result.History) > 0 {
		_ = ws.writeJSON(WSMessage{Type: "history", History: historyEntries(result.History)})
	}
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
	conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return nil
	})

	ws := newWSConn(conn)
	defer ws.closeSend()
	session := newWSSession(ws)

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
	cfgMsg := s.agentConfigMsg(r.Context())
	msgs := append([]llm.Message(nil), s.agent.Messages...)
	s.agentMu.RUnlock()
	_ = ws.writeJSON(cfgMsg)
	if len(msgs) > 0 {
		_ = ws.writeJSON(WSMessage{Type: "history", History: historyEntries(msgs)})
	}

	incoming := make(chan WSMessage, 8)
	go func() {
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				close(incoming)
				return
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
			go func(content string, ctx context.Context, done chan error) {
				defer func() { done <- nil }()
				defer s.turnMu.Unlock()
				ctx = agent.ContextWithDeleteApprover(ctx, session.deleteApprover())
				write := func(v WSMessage) {
					if ctx.Err() != nil {
						return
					}
					if err := ws.writeJSON(v); err != nil {
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
						write(WSMessage{
							Type:       "tool_call",
							Tool:       tc.Name,
							ToolCallID: tc.ID,
							Index:      tc.Index,
							Args:       tc.Args,
						})
					},
					OnToolResult: func(id, name, result string, success bool) {
						truncated := false
						const maxResult = 4096
						if len(result) > maxResult {
							result = result[:maxResult] + fmt.Sprintf("\n… truncated (%d bytes total)", len(result))
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
					if ctx.Err() == nil {
						tokens.flush()
						write(WSMessage{Type: "stream_end"})
						write(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
						log.Printf("stream error: %v", err)
					}
					return
				}
				tokens.flush()
				write(WSMessage{Type: "stream_end"})
				write(s.contextMsg(r.Context()))
			}(msg.Content, streamCtx, errCh)
			// Do not block here — keep reading so delete_approval_response can complete.
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
	if !s.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	content, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(content)
}

func (s *Server) checkAuth(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	if tok := strings.TrimSpace(r.URL.Query().Get("token")); tok != "" && tok == s.authToken {
		return true
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
		log.Printf("warning: listening on non-loopback %s with token auth enabled", addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWS)
	mux.HandleFunc("/", s.HandleStatic)
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

package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	"gogen/internal/streamutil"

	"github.com/gorilla/websocket"
)

//go:embed all:web
var webAssets embed.FS

var errWSClosed = errors.New("websocket closed")

// staticAsset is a lazily compressed embedded asset served by HandleStatic.
// raw and gzip are cached after first use so a page load does not re-compress
// the multi-MB Monaco bundle on every request.
type staticAsset struct {
	contentType string
	etag        string // weak ETag derived from the raw content hash
	raw         []byte
	gzip        []byte // nil when the asset is not worth gzip-ing
}

// staticAssetCache caches embedded assets after their first request. Entries
// are immutable once stored, so readers can use the returned pointer freely
// after the mutex is released.
type staticAssetCache struct {
	mu      sync.Mutex
	entries map[string]*staticAsset
}

// peek returns the cached asset for name without reading or compressing anything.
func (c *staticAssetCache) peek(name string) (*staticAsset, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.entries[name]
	return a, ok
}

// get returns the cached asset for name, filling it from content on first use.
// gzipable + minGzipSize control whether gzip bytes are produced (gzip when
// len(content) > minGzipSize); pass minGzipSize = -1 to compress any size.
func (c *staticAssetCache) get(name string, content []byte, contentType string, gzipable bool, minGzipSize int) *staticAsset {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a, ok := c.entries[name]; ok {
		return a
	}
	a := &staticAsset{contentType: contentType, raw: content, etag: weakETag(content)}
	if gzipable && len(content) > minGzipSize {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(content)
		_ = gz.Close()
		a.gzip = buf.Bytes()
	}
	if c.entries == nil {
		c.entries = make(map[string]*staticAsset)
	}
	c.entries[name] = a
	return a
}

// weakETag returns a weak validator for content. Weak is used because the same
// entity is served both identity- and gzip-encoded (differing bytes but equal
// semantics); Vary: Accept-Encoding keeps the variants apart in caches.
func weakETag(content []byte) string {
	sum := sha256.Sum256(content)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagMatches reports whether an If-None-Match header matches etag (exact
// match, one of several comma-separated candidates, or a `*` wildcard).
func etagMatches(header, etag string) bool {
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) == etag {
			return true
		}
	}
	return false
}

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
	// UI cancel: wait briefly for StreamProcessInput to finish cancel repair.
	wsStreamDrainWait = 2 * time.Second
)

// drainStreamErr waits for the stream goroutine to signal exit.
// Returns true if the signal arrived, false on timeout (caller should keep ch).
func drainStreamErr(ch chan error) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(wsStreamDrainWait):
		log.Printf("warning: timed out waiting for stream goroutine to exit")
		return false
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

// acquireTurnForHandler cancels any in-flight stream on this connection, then
// tries to acquire the global agent turn lock. On failure it writes the
// standard "busy" error and returns false.
func (s *Server) acquireTurnForHandler(ws *wsConn, stream *wsConnStream) bool {
	stream.cancelInFlight()
	if !s.tryAcquireTurn(wsTurnAcquireWait) {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
		return false
	}
	return true
}

// spawnUserTerminal starts the interactive user shell for a WebSocket
// connection (if none is alive) and reports its lifecycle over the socket:
// user_term_opened once the shell is up, user_term_exit when it exits. Output
// is streamed as user_term_output chunks from the PTY read goroutine. The
// shell runs in the agent's current working directory at spawn time.
func (s *Server) spawnUserTerminal(ws *wsConn, holder *userTermHolder) {
	if holder.get() != nil {
		return
	}
	var wd string
	s.lockAgentRead(func() {
		wd = s.agent.Executor.GetWorkingDir()
	})
	ut, err := startUserTerminal(wd, func(chunk string) {
		_ = ws.writeJSON(WSMessage{Type: "user_term_output", Content: chunk})
	})
	if err != nil {
		_ = ws.writeJSON(WSMessage{Type: "user_term_exit", Content: "failed to start shell: " + err.Error(), Code: -1})
		return
	}
	holder.set(ut)
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
	agentMu        sync.RWMutex // protects Agent reads/writes; see agent_sync.go
	turnMu         sync.Mutex   // serializes agent-mutating work across WS clients
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

type ModelEntry struct {
	ID               string  `json:"id"`
	ContextLimit     int     `json:"contextLimit,omitempty"`
	Current          bool    `json:"current,omitempty"`
	InputPricePer1M  float64 `json:"inputPricePer1M,omitempty"`
	OutputPricePer1M float64 `json:"outputPricePer1M,omitempty"`
	CachedPricePer1M float64 `json:"cachedPricePer1M,omitempty"`
}

type SessionEntry struct {
	ID           string `json:"id"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
	MessageCount int    `json:"messageCount,omitempty"`
	Label        string `json:"label,omitempty"`
	Oneshot      bool   `json:"oneshot,omitempty"`
	Current      bool   `json:"current,omitempty"`
	// Label is now the full first user message — CSS text-overflow: ellipsis
	// handles dynamic truncation on the client side.
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
	Refusal    string            `json:"refusal,omitempty"`
	ToolCalls  []HistoryToolCall `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
	Index      int               `json:"index"`               // index in agent.Messages (0 is valid; do not omitempty)
	CreatedAt  string            `json:"createdAt,omitempty"` // RFC3339Nano UTC when the message was created
}

type WSMessage struct {
	Type            string                 `json:"type"`
	Content         string                 `json:"content,omitempty"`
	Tool            string                 `json:"tool,omitempty"`
	TermID          string                 `json:"termId,omitempty"`
	Cols            int                    `json:"cols,omitempty"`
	Rows            int                    `json:"rows,omitempty"`
	Code            int                    `json:"code,omitempty"`
	ToolCallID      string                 `json:"toolCallId,omitempty"`
	Index           int                    `json:"index,omitempty"`
	ArgsDelta       string                 `json:"argsDelta,omitempty"`
	Args            map[string]interface{} `json:"args,omitempty"`
	Result          string                 `json:"result,omitempty"`
	Success         bool                   `json:"success,omitempty"`
	ResultTruncated bool                   `json:"resultTruncated,omitempty"`
	WorkingDir      string                 `json:"workingDir,omitempty"`
	Model           string                 `json:"model,omitempty"`
	// Pricing for the current model (USD per 1M tokens), populated from models.dev cache.
	InputPricePer1M  float64 `json:"inputPricePer1M,omitempty"`
	OutputPricePer1M float64 `json:"outputPricePer1M,omitempty"`
	CachedPricePer1M float64 `json:"cachedPricePer1M,omitempty"`
	ContextLimit     int     `json:"contextLimit,omitempty"`
	UsedTokens       int     `json:"usedTokens,omitempty"`
	UsedSource       string  `json:"usedSource,omitempty"`
	PromptTokens     int     `json:"promptTokens,omitempty"`
	CompletionTokens int     `json:"completionTokens,omitempty"`
	CachedTokens     int     `json:"cachedTokens,omitempty"`
	CompactAt        int     `json:"compactAt,omitempty"`
	MessageCount     int     `json:"messageCount,omitempty"`
	NearCompact      bool    `json:"nearCompact,omitempty"`
	UsedPercent      float64 `json:"usedPercent,omitempty"`
	ToolTruncated    bool    `json:"toolTruncated,omitempty"`
	// Accumulated session usage
	TotalPromptTokens     int            `json:"totalPromptTokens,omitempty"`
	TotalCompletionTokens int            `json:"totalCompletionTokens,omitempty"`
	TotalCachedTokens     int            `json:"totalCachedTokens,omitempty"`
	TotalTurns            int            `json:"totalTurns,omitempty"`
	Models                []ModelEntry   `json:"models,omitempty"`
	ApprovalID            string         `json:"approvalId,omitempty"`
	Approved              bool           `json:"approved,omitempty"`
	Paths                 []string       `json:"paths,omitempty"`
	Reason                string         `json:"reason,omitempty"`
	Mode                  string         `json:"mode,omitempty"`
	ThinkingLevel         string         `json:"thinkingLevel,omitempty"`
	GlobalMode            bool           `json:"globalMode,omitempty"`
	SessionID             string         `json:"sessionId,omitempty"`
	SessionAction         string         `json:"sessionAction,omitempty"`
	SessionLabel          string         `json:"sessionLabel,omitempty"`
	MessageIndex          int            `json:"messageIndex,omitempty"`
	Sessions              []SessionEntry `json:"sessions,omitempty"`
	History               []HistoryEntry `json:"history,omitempty"`
	// Filesystem / git editor APIs
	Path        string              `json:"path,omitempty"`
	Pattern     string              `json:"pattern,omitempty"`
	Glob        string              `json:"glob,omitempty"`
	Language    string              `json:"language,omitempty"`
	Error       string              `json:"error,omitempty"`
	Entries     []FSEntry           `json:"entries,omitempty"`
	GitEntries  []GitStatusEntry    `json:"gitEntries,omitempty"`
	Matches     []agent.SearchMatch `json:"matches,omitempty"`
	Truncated   bool                `json:"truncated,omitempty"`
	Original    string              `json:"original,omitempty"`
	Modified    string              `json:"modified,omitempty"`
	RequestID   string              `json:"requestId,omitempty"`
	Replacement string              `json:"replacement,omitempty"`
	Replaced    int                 `json:"replaced,omitempty"`
	FileCount   int                 `json:"fileCount,omitempty"`
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

func applyContextStats(msg *WSMessage, stats agent.TurnContext, accum *agent.UsageAccumulator) {
	snap := stats.Snapshot
	if snap.Limit > 0 {
		msg.ContextLimit = snap.Limit
	}
	if snap.Used > 0 {
		msg.UsedTokens = snap.Used
	}
	msg.PromptTokens = stats.PromptTokens
	msg.CompletionTokens = stats.CompletionTokens
	msg.CachedTokens = stats.CachedTokens
	msg.CompactAt = snap.CompactAt
	msg.MessageCount = snap.MessageCount
	msg.NearCompact = snap.NearCompact
	msg.ToolTruncated = snap.ToolTruncated
	// When the API returned exact usage, the Snapshot.Used incorporates that
	// authoritative baseline (plus estimates for any messages added after the
	// last request). Otherwise it's purely a local estimate.
	msg.UsedSource = "estimated"
	if stats.LastUsage != nil && stats.LastUsage.PromptTokens > 0 {
		msg.UsedSource = "api"
	}
	if snap.Limit > 0 {
		msg.UsedPercent = snap.Percent
	}
	if accum != nil {
		msg.TotalPromptTokens = accum.TotalPromptTokens
		msg.TotalCompletionTokens = accum.TotalCompletionTokens
		msg.TotalCachedTokens = accum.TotalCachedTokens
		msg.TotalTurns = accum.TotalTurns
	}
}

// agentConfigMsgBasic returns config fields that are cheap to read.
// Caller must hold agentMu (R or W). Do not call ContextStats while holding
// agentMu — tokenize after unlocking via applyContextStats.
func (s *Server) agentConfigMsgBasic() WSMessage {
	return WSMessage{
		Type:          "config",
		WorkingDir:    s.agent.Executor.GetWorkingDir(),
		Model:         s.agent.CurrentModel(),
		Mode:          s.agent.Mode.String(),
		ThinkingLevel: string(s.agent.ThinkingLevel),
		GlobalMode:    s.agent.GlobalMode,
		SessionID:     s.agent.SessionID,
		SessionLabel:  s.agent.SessionLabelSnapshot(),
	}
}

// agentConfigMsg is a locked basic snapshot plus ContextStats applied outside
// agentMu. Prefer this when the caller does not already hold agentMu.
func (s *Server) agentConfigMsg(ctx context.Context) WSMessage {
	var msg WSMessage
	s.lockAgentRead(func() {
		msg = s.agentConfigMsgBasic()
	})
	s.fillModelPricing(ctx, &msg)
	accum := s.agent.SnapshotUsageAccum()
	applyContextStats(&msg, s.agent.ContextStats(ctx), &accum)
	return msg
}

// fillModelPricing looks up pricing for the current model from the models.dev
// registry cache (never blocks — pure map lookup).
func (s *Server) fillModelPricing(_ context.Context, msg *WSMessage) {
	if p, ok := s.agent.Provider.(*llm.OpenAIProvider); ok && msg.Model != "" {
		if in, out, cached, ok := p.ModelPricing(msg.Model); ok {
			msg.InputPricePer1M = in
			msg.OutputPricePer1M = out
			msg.CachedPricePer1M = cached
		}
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
			Oneshot:      s.Oneshot,
			Current:      s.ID == currentID,
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
				continue
			}
			out = append(out, HistoryEntry{Role: m.Role, Content: m.Content, Index: idx, CreatedAt: createdAt})
		case "assistant":
			if m.Content == "" && len(m.ToolCalls) == 0 && m.Reasoning == "" && m.Refusal == "" {
				continue
			}
			entry := HistoryEntry{
				Role:      m.Role,
				Content:   m.Content,
				Reasoning: m.Reasoning,
				Refusal:   m.Refusal,
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

func (s *Server) writeSessionCommandResult(ws *wsConn, ctx context.Context, result agent.SessionCommandResult, err error) {
	resp := WSMessage{Type: "response"}
	clearChat := false
	if err != nil {
		resp.Content = fmt.Sprintf("Error: %v", err)
	} else {
		resp.Content = result.Output
		if result.Action == agent.SessionActionClearChat {
			resp.SessionAction = string(result.Action)
			clearChat = true
			_ = ws.writeJSON(WSMessage{Type: "clear_chat"})
		}
	}

	var cfg WSMessage
	var history []llm.Message
	needHistory := clearChat && err == nil && len(result.History) == 0
	s.lockAgentRead(func() {
		if err == nil && len(result.Sessions) > 0 {
			resp.Type = "sessions"
			resp.Sessions = sessionEntries(result.Sessions, s.agent.SessionID)
		}
		cfg = s.agentConfigMsgBasic()
		if len(result.History) > 0 {
			history = append([]llm.Message(nil), result.History...)
		}
	})
	if needHistory {
		// /new (and any clear with empty History) — still emit history so
		// clients can reliably run post-session follow-ups (e.g. resend).
		history = s.agent.SnapshotMessages()
	}
	resp.SessionID = cfg.SessionID
	resp.Mode = cfg.Mode
	// Paint sessions/history before ContextStats tokenization (can be slow on
	// large restored sessions / cold tiktoken init).
	_ = ws.writeJSON(resp)
	if clearChat && err == nil {
		_ = ws.writeJSON(WSMessage{Type: "history", History: historyEntries(history)})
	} else if len(history) > 0 {
		_ = ws.writeJSON(WSMessage{Type: "history", History: historyEntries(history)})
	}
	stats := s.agent.ContextStats(ctx)
	accum := s.agent.SnapshotUsageAccum()
	applyContextStats(&cfg, stats, &accum)
	s.fillModelPricing(ctx, &cfg)
	ctxMsg := WSMessage{Type: "context"}
	applyContextStats(&ctxMsg, stats, &accum)
	_ = ws.writeJSON(ctxMsg)
	_ = ws.writeJSON(cfg)
}

// contextMsg builds a context stats message. Must not be called while holding
// agentMu — ContextStats tokenizes the full history view.
func (s *Server) contextMsg(ctx context.Context) WSMessage {
	msg := WSMessage{Type: "context"}
	accum := s.agent.SnapshotUsageAccum()
	applyContextStats(&msg, s.agent.ContextStats(ctx), &accum)
	msg.SessionLabel = s.agent.SessionLabelSnapshot()
	return msg
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

	s.registerWSConn(conn)
	defer s.unregisterWSConn(conn)
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

	stream := &wsConnStream{}
	defer stream.close()

	// Interactive user shell for this connection, killed on disconnect. The
	// shell itself is spawned after the config/history handshake below so
	// connection setup never depends on pty availability.
	userTermHolder := &userTermHolder{}
	defer func() {
		if ut := userTermHolder.get(); ut != nil {
			ut.Close()
		}
	}()

	s.agentMu.RLock()
	cfgMsg := s.agentConfigMsgBasic()
	s.agentMu.RUnlock()
	msgs := s.agent.SnapshotMessages()
	_ = ws.writeJSON(cfgMsg)
	if len(msgs) > 0 {
		_ = ws.writeJSON(WSMessage{Type: "history", History: historyEntries(msgs)})
	}

	// Send full config with context stats and pricing asynchronously so the
	// client can start painting history immediately. Tokenization runs without agentMu.
	go func() {
		_ = ws.writeJSON(s.agentConfigMsg(r.Context()))
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
		case "fs_list", "fs_read", "fs_search", "git_status", "git_file_diff":
			s.handleFSReadMessage(ws, r.Context(), msg)
		case "fs_write", "fs_replace":
			s.handleFSWriteMessage(ws, r.Context(), msg)
		case "list_sessions":
			s.handleWSListSessions(ws)
		case "session_new", "session_resume", "session_delete", "session_fork":
			s.handleWSSessionAction(ws, r.Context(), stream, msg)
		case "list_models":
			s.handleWSListModels(ws, r.Context())
		case "set_model":
			s.handleWSSetModel(ws, r.Context(), stream, msg)
		case "set_mode":
			s.handleWSSetMode(ws, r.Context(), stream, msg)
		case "set_thinking_level":
			s.handleWSSetThinkingLevel(ws, r.Context(), stream, msg)
		case "config":
			s.handleWSConfig(ws, r.Context(), stream, msg)
		case "cancel":
			stream.cancelInFlight()
		case "user_term_input":
			if ut := userTermHolder.get(); ut != nil {
				_ = ut.Write([]byte(msg.Content))
			}
		case "user_term_resize":
			if ut := userTermHolder.get(); ut != nil && msg.Cols > 0 && msg.Rows > 0 {
				_ = ut.Resize(uint16(msg.Cols), uint16(msg.Rows))
			}
		case "user_term_request":
			s.spawnUserTerminal(ws, userTermHolder)
		case "message":
			s.handleWSUserMessage(ws, r, stream, session, msg)
		}
	}
}

func (s *Server) handleWSListSessions(ws *wsConn) {
	// Listing hits the session store on disk (metadata index read, label
	// migration file reads, legacy full-scan fallback). Run it off the WS
	// read loop like handleWSListModels, so a slow store cannot block chat,
	// FS, or editor messages behind the sidebar.
	go func() {
		_, sessions, listErr := s.agent.FormatSessionListForUI()
		if listErr != nil {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", listErr)})
			return
		}
		// Read the current session id after listing so the "current" marker
		// is as fresh as possible.
		var sessionID string
		s.lockAgentRead(func() {
			sessionID = s.agent.SessionID
		})
		_ = ws.writeJSON(WSMessage{
			Type:      "sessions",
			Sessions:  sessionEntries(sessions, sessionID),
			SessionID: sessionID,
		})
	}()
}

func (s *Server) handleWSSessionAction(ws *wsConn, ctx context.Context, stream *wsConnStream, msg WSMessage) {
	if !s.acquireTurnForHandler(ws, stream) {
		return
	}
	if (msg.Type == "session_resume" || msg.Type == "session_delete") && strings.TrimSpace(msg.SessionID) == "" {
		s.turnMu.Unlock()
		_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: sessionId is required"})
		return
	}
	var cmd string
	switch msg.Type {
	case "session_new":
		cmd = "/new"
	case "session_resume":
		cmd = "resume " + strings.TrimSpace(msg.SessionID)
	case "session_delete":
		cmd = "resume del " + strings.TrimSpace(msg.SessionID)
	case "session_fork":
		forkArg := fmt.Sprintf("%d", msg.MessageIndex)
		if msg.MessageIndex < 0 {
			forkArg = "last"
		}
		cmd = "fork " + forkArg
	}

	result, _, err := s.agent.HandleSessionCommand(ctx, cmd, sesspkg.NewID())
	s.turnMu.Unlock()
	s.writeSessionCommandResult(ws, ctx, result, err)
}

func (s *Server) handleWSListModels(ws *wsConn, ctx context.Context) {
	go func() {
		models, err := s.agent.ListModels(ctx)
		if err != nil {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
			return
		}
		var current string
		s.lockAgentRead(func() {
			current = s.agent.CurrentModel()
		})
		_ = ws.writeJSON(WSMessage{
			Type:   "models",
			Model:  current,
			Models: s.modelEntries(models),
		})
	}()
}

func (s *Server) handleWSSetModel(ws *wsConn, ctx context.Context, stream *wsConnStream, msg WSMessage) {
	if !s.acquireTurnForHandler(ws, stream) {
		return
	}
	err := s.agent.SelectModel(ctx, msg.Model)
	cfg := s.agentConfigMsg(ctx)
	s.turnMu.Unlock()
	if err != nil {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: fmt.Sprintf("Error: %v", err)})
		return
	}
	_ = ws.writeJSON(cfg)
}

func (s *Server) handleWSSetMode(ws *wsConn, ctx context.Context, stream *wsConnStream, msg WSMessage) {
	if !s.acquireTurnForHandler(ws, stream) {
		return
	}
	modeSet := false
	var cfg WSMessage
	s.lockAgentWrite(func() {
		if m, ok := agent.ParseMode(msg.Mode); ok {
			s.agent.SetMode(m)
			modeSet = true
			cfg = s.agentConfigMsgBasic()
		}
	})
	s.turnMu.Unlock()
	if modeSet {
		accum := s.agent.SnapshotUsageAccum()
		applyContextStats(&cfg, s.agent.ContextStats(ctx), &accum)
		_ = ws.writeJSON(cfg)
	}
}

func (s *Server) handleWSSetThinkingLevel(ws *wsConn, ctx context.Context, stream *wsConnStream, msg WSMessage) {
	if !s.acquireTurnForHandler(ws, stream) {
		return
	}
	s.lockAgentWrite(func() {
		if level, ok := agent.ParseThinkingLevel(msg.ThinkingLevel); ok {
			s.agent.SetThinkingLevel(level)
		}
	})
	cfg := s.agentConfigMsg(ctx)
	s.turnMu.Unlock()
	_ = ws.writeJSON(cfg)
}

func (s *Server) handleWSConfig(ws *wsConn, ctx context.Context, stream *wsConnStream, msg WSMessage) {
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
	if !s.acquireTurnForHandler(ws, stream) {
		return
	}
	var cfg WSMessage
	s.lockAgentWrite(func() {
		s.agent.SetWorkingDir(absDir)
		s.config.WorkingDir = absDir
		cfg = s.agentConfigMsgBasic()
	})
	s.agent.AfterWorkingDirChange()
	s.turnMu.Unlock()
	accum := s.agent.SnapshotUsageAccum()
	applyContextStats(&cfg, s.agent.ContextStats(ctx), &accum)
	_ = ws.writeJSON(WSMessage{Type: "config", WorkingDir: absDir, Model: cfg.Model, ContextLimit: cfg.ContextLimit, UsedTokens: cfg.UsedTokens, UsedSource: cfg.UsedSource, UsedPercent: cfg.UsedPercent, CompactAt: cfg.CompactAt, MessageCount: cfg.MessageCount, NearCompact: cfg.NearCompact, ToolTruncated: cfg.ToolTruncated, Mode: cfg.Mode})
}

func (s *Server) handleWSUserMessage(ws *wsConn, r *http.Request, stream *wsConnStream, session *wsSession, msg WSMessage) {
	stream.cancelInFlight()

	if out, handled := agent.HandleHelpCommand(msg.Content, true, false); handled {
		_ = ws.writeJSON(WSMessage{Type: "response", Content: out})
		return
	}

	if sel, isModels := agent.ParseModelsCommand(msg.Content); isModels && sel == "" {
		go func(content string) {
			out, _, err := s.agent.HandleModelsCommand(r.Context(), content)
			resp := WSMessage{Type: "response", Content: out}
			if err != nil {
				resp.Content = fmt.Sprintf("Error: %v", err)
				_ = ws.writeJSON(resp)
				return
			}
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
			_ = ws.writeJSON(resp)
		}(msg.Content)
		return
	}

	if !s.tryAcquireTurn(wsTurnAcquireWait) {
		// Cancel may have timed out while a tool was still exiting; wait once more.
		stream.cancelInFlight()
		if !s.tryAcquireTurn(wsStreamDrainWait) {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
			return
		}
	}

	var modeOut string
	var modeHandled bool
	var modeCfg WSMessage
	s.lockAgentWrite(func() {
		modeOut, modeHandled = s.agent.HandleModeCommand(msg.Content)
		if modeHandled {
			modeCfg = s.agentConfigMsgBasic()
		}
	})
	if modeHandled {
		s.turnMu.Unlock()
		accum := s.agent.SnapshotUsageAccum()
		applyContextStats(&modeCfg, s.agent.ContextStats(r.Context()), &accum)
		_ = ws.writeJSON(modeCfg)
		_ = ws.writeJSON(WSMessage{Type: "response", Content: modeOut})
		return
	}

	var thinkOut string
	var thinkHandled bool
	s.lockAgentWrite(func() {
		thinkOut, thinkHandled = s.agent.HandleThinkingCommand(msg.Content)
	})
	if thinkHandled {
		s.turnMu.Unlock()
		cfg := s.agentConfigMsg(r.Context())
		cfg.ThinkingLevel = string(s.agent.ThinkingLevel)
		_ = ws.writeJSON(cfg)
		_ = ws.writeJSON(WSMessage{Type: "response", Content: thinkOut})
		return
	}

	var ctxOut string
	var ctxHandled bool
	ctxOut, ctxHandled = s.agent.HandleContextCommand(r.Context(), msg.Content)
	if ctxHandled {
		s.turnMu.Unlock()
		ctxMsg := s.contextMsg(r.Context())
		_ = ws.writeJSON(ctxMsg)
		_ = ws.writeJSON(WSMessage{Type: "response", Content: ctxOut})
		return
	}

	var sessResult agent.SessionCommandResult
	var sessHandled bool
	var sessErr error
	sessResult, sessHandled, sessErr = s.agent.HandleSessionCommand(r.Context(), msg.Content, sesspkg.NewID())
	if sessHandled {
		s.turnMu.Unlock()
		s.writeSessionCommandResult(ws, r.Context(), sessResult, sessErr)
		return
	}

	if sel, isModels := agent.ParseModelsCommand(msg.Content); isModels && sel != "" {
		out, _, modelErr := s.agent.HandleModelsCommand(r.Context(), msg.Content)
		cfg := s.agentConfigMsg(r.Context())
		s.turnMu.Unlock()
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
		return
	}

	s.startAsyncStreamingTurn(ws, r, stream, session, msg.Content)
}

func (s *Server) startAsyncStreamingTurn(ws *wsConn, r *http.Request, stream *wsConnStream, session *wsSession, content string) {
	streamCtx, streamCancel := context.WithCancel(r.Context())
	llmCtx, llmCancel := context.WithCancel(streamCtx)
	errCh := stream.begin(streamCancel, llmCancel)
	go func(content string, llmCtx context.Context, done chan error) {
		defer stream.end()
		defer func() { done <- nil }()
		defer s.turnMu.Unlock()
		defer func() {
			_ = ws.writeJSON(WSMessage{Type: "turn_end"})
		}()
		ctx := agent.ContextWithDeleteApprover(llmCtx, session.deleteApprover())
		var writeFailed atomic.Bool
		failWrite := sync.Once{}
		write := func(v WSMessage) {
			if ctx.Err() != nil {
				return
			}
			if err := ws.writeJSON(v); err != nil {
				writeFailed.Store(true)
				failWrite.Do(func() {
					llmCancel()
					_ = ws.conn.Close()
				})
				return
			}
		}
		tokens := streamutil.NewTokenBatcher(func(think bool, text string) {
			if think {
				write(WSMessage{Type: "thinking_token", Content: text})
			} else {
				write(WSMessage{Type: "stream", Content: text})
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

		handlers := &llm.StreamHandlers{
			OnStart: func() {
				// Tell the client the server-side index of the user message
				// that StreamProcessInput just appended (for edit/resend).
				// Index goes in Content because WSMessage.Index has omitempty
				// and the first message is index 0.
				userIdx := s.agent.MessageCount() - 1
				if userIdx >= 0 {
					write(WSMessage{Type: "user_acked", Content: fmt.Sprintf("%d", userIdx)})
				}
				write(WSMessage{Type: "thinking"})
				if ctx.Err() != nil {
					return
				}
				write(s.contextMsg(ctx))
			},
			OnRoundStart: func() {
				write(WSMessage{Type: "thinking"})
				if ctx.Err() != nil {
					return
				}
				write(s.contextMsg(ctx))
			},
			OnStreamOpened: func() {
				write(WSMessage{Type: "waiting"})
			},
			OnStreamActivity: func() {},
			OnThinkingToken:  tokens.ThinkToken,
			OnToken:          tokens.StreamToken,
			OnStreamEnd: func() {
				tokens.Flush()
				write(WSMessage{Type: "stream_end"})
			},
			OnToolCallStart: func(index int, id, name string) {
				tokens.Flush()
				write(WSMessage{
					Type:       "tool_call_start",
					Tool:       name,
					ToolCallID: id,
					Index:      index,
				})
			},
			OnToolCallArgsDelta: func(index int, id, name, argsDelta string) {
				tokens.Flush()
				write(WSMessage{
					Type:       "tool_call_delta",
					Tool:       name,
					ToolCallID: id,
					Index:      index,
					ArgsDelta:  argsDelta,
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
		var persistErr error
		var ctxMsg WSMessage
		if err == nil {
			s.lockAgentWrite(func() {
				persistErr = s.agent.ConsumePersistError()
			})
			ctxMsg = s.contextMsg(r.Context())
		}
		if err != nil {
			if ctx.Err() != nil {
				tokens.Flush()
				if !writeFailed.Load() {
					_ = ws.writeJSON(WSMessage{Type: "cancelled", Content: "Cancelled."})
				}
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
		write(WSMessage{Type: "stream_end"})
		if ctxMsg.Type != "" {
			write(ctxMsg)
		}
	}(content, llmCtx, errCh)
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
		// index.html is compressed regardless of size (it is the bootstrap
		// page); everything else follows the staticGzipMime + 512-byte rule.
		s.serveEmbedded(w, r, "web/index.html", "text/html; charset=utf-8", "no-cache", true)
		return
	}

	// Serve embedded assets under /monaco/... (and future static paths).
	rel := strings.TrimPrefix(path, "/")
	if strings.Contains(rel, "..") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := "web/" + rel
	ct := contentTypeForExt(filepath.Ext(name))
	s.serveEmbedded(w, r, name, ct, "public, max-age=86400", false)
}

// serveEmbedded serves an embedded asset from the static asset cache, reading
// and gzip-compressing it on first use only. Repeated requests reuse the
// cached bytes and revalidate via ETag/If-None-Match (304), so a page reload
// never re-compresses the multi-MB Monaco bundle.
func (s *Server) serveEmbedded(w http.ResponseWriter, r *http.Request, name, contentType, cacheControl string, gzipAlways bool) {
	asset, ok := s.staticAssets.peek(name)
	if !ok {
		content, err := webAssets.ReadFile(name)
		if err != nil {
			if gzipAlways {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				http.Error(w, "not found", http.StatusNotFound)
			}
			return
		}
		minGzipSize := 512
		if gzipAlways {
			minGzipSize = -1
		}
		asset = s.staticAssets.get(name, content, contentType, staticGzipMime(contentType), minGzipSize)
	}

	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", asset.etag)

	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match != "" && etagMatches(match, asset.etag) {
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if asset.gzip != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		_, _ = w.Write(asset.gzip)
		return
	}
	_, _ = w.Write(asset.raw)
}

// staticGzipMime reports whether content of the given type is worth gzip-ing
// on the fly. Binary/already-compressed assets (fonts, images) are excluded.
func staticGzipMime(ct string) bool {
	switch ct {
	case "text/html; charset=utf-8", "text/css; charset=utf-8",
		"text/javascript; charset=utf-8", "application/javascript",
		"application/json; charset=utf-8", "image/svg+xml":
		return true
	}
	return false
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

func IsLoopbackBind(addr string) bool {
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

// registerWSConn adds conn to the tracked set so the server can close it
// on graceful shutdown.
func (s *Server) registerWSConn(conn *websocket.Conn) {
	s.wsConnsMu.Lock()
	s.wsConns = append(s.wsConns, conn)
	s.wsConnsMu.Unlock()
}

// unregisterWSConn removes conn from the tracked set so shutdown does not
// close a connection that has already been cleaned up.
func (s *Server) unregisterWSConn(conn *websocket.Conn) {
	s.wsConnsMu.Lock()
	defer s.wsConnsMu.Unlock()
	for i, c := range s.wsConns {
		if c == conn {
			s.wsConns = append(s.wsConns[:i], s.wsConns[i+1:]...)
			return
		}
	}
}

// closeWSConns force-closes all tracked WebSocket connections. Safe to call
// concurrently with register/unregister. Never blocks on a single conn.
func (s *Server) closeWSConns() {
	s.wsConnsMu.Lock()
	conns := s.wsConns
	s.wsConns = nil
	s.wsConnsMu.Unlock()
	now := time.Now()
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		c := conn
		go func() {
			_ = c.SetReadDeadline(now)
			_ = c.SetWriteDeadline(now)
			_ = c.Close()
		}()
	}
}

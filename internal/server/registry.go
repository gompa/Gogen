package server

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"gogen/internal/agent"
	"gogen/internal/config"
)

// sessionRuntime is one live chat session in the web server: the agent, the
// per-session turn lock, and the turn machinery that used
// to live per connection: the in-flight stream handles, the pending delete
// approvals, and the set of attached client sockets (fan-out). A turn is
// owned by the runtime, not by any connection: disconnecting the last client
// detaches it but the turn keeps running server-side and persists normally
// (§4 connection-loss continuation).
type sessionRuntime struct {
	agent  *agent.Agent
	turnMu sync.RWMutex // turns take Lock; config/history reads take RLock

	// stream owns the in-flight stream cancel handles for this session
	// (moved from per-connection).
	stream *wsConnStream

	// liveMu guards live, the current round's in-flight LLM output buffer
	// (see liveTurnState). Leaf lock: never held while acquiring turnMu.
	liveMu sync.Mutex
	live   liveTurnState

	// approvals are pending delete-approval requests, keyed by approvalID
	// (per-session, so two sessions' approvals cannot collide, E5). Each
	// entry keeps the original broadcast payload (paths/reason) so a
	// reconnecting client can be re-notified of the pending request (F2).
	approvalMu sync.Mutex
	approvals  map[string]*pendingApproval

	// approvalHold is how long pending delete approvals survive the last
	// attached client detaching before they are auto-denied. Zero (the
	// default) denies immediately on detach (D10); a positive value gives a
	// reconnecting client a window to answer before the hold expires.
	approvalHold time.Duration

	// clients are the attached sockets; broadcast() fans events out to all
	// of them. A write failure detaches that socket, never cancels
	// the turn.
	clientsMu sync.Mutex
	clients   map[*wsConn]struct{}

	// turnActive backs the session_state reply on attach so a reconnecting
	// client can distinguish "turn running headless" from "idle"; startedAt
	// records when the turn began (retained bookkeeping, not sent to the
	// client). turnOwner is the connection that started the current turn;
	// only it may interrupt via the cancel-then-lock path — a second
	// connection attached to the same session must not kill the turn
	// it does not own.
	stateMu    sync.Mutex
	turnActive bool
	startedAt  time.Time
	turnOwner  *wsConn

	// evicted marks a runtime that has been removed from the registry
	// (registry-cap eviction or session delete). It is set while the eviction
	// holds the runtime's turnMu and checked by message handlers AFTER they
	// acquire turnMu, so a handler that resolved the runtime just before the
	// eviction cannot start a turn on a runtime that is no longer registered
	// (invisible to cancel/prune/shutdown). Monotonic: an evicted runtime is
	// never re-registered — resume/attach build a fresh one.
	evicted atomic.Bool

	// registry is the owning sessionRegistry. Set by register(); used by
	// detach and the turn-end hook to evict the runtime when it becomes an
	// orphan — no attached clients and no running turn (a warm runtime
	// nobody is viewing should read as a plain saved session, not a stale
	// "resume to continue" row). Nil for runtimes built outside a registry
	// (unit tests): the orphan checks then no-op.
	registry *sessionRegistry
}

// pendingApproval is one in-flight delete-approval request: the channel the
// turn's deleteApprover waits on, plus the original broadcast payload so a
// re-attaching client can be re-notified of the pending request.
type pendingApproval struct {
	ch     chan bool
	paths  []string
	reason string
}

// newSessionRuntime builds a runtime with the per-session turn machinery
// initialized. The agent must be non-nil.
func newSessionRuntime(a *agent.Agent) *sessionRuntime {
	return &sessionRuntime{
		agent:     a,
		stream:    &wsConnStream{},
		approvals: make(map[string]*pendingApproval),
		clients:   make(map[*wsConn]struct{}),
	}
}

// newSessionRuntimeWithHold builds a runtime whose pending delete approvals
// are auto-denied `hold` after the last attached client detaches (instead of
// immediately). Tests and non-server embeddings keep using newSessionRuntime
// (hold = 0, immediate deny).
func newSessionRuntimeWithHold(a *agent.Agent, hold time.Duration) *sessionRuntime {
	rt := newSessionRuntime(a)
	rt.approvalHold = hold
	return rt
}

// liveTurnState accumulates the current round's in-flight LLM output so an
// attach/resume mid-turn can include it in the history snapshot: the
// assistant reply is only appended to a.Messages when a round completes, so
// without this buffer a switch to a running session would show "Resuming…"
// with no context until the turn ends. Content and thinking are accumulated
// verbatim (the same strings the client receives) along with the streaming
// tool calls. Every access goes through sessionRuntime.liveMu: the turn
// goroutine appends, attach goroutines snapshot, and the token-batcher flush
// reads the sent-position markers.
//
// The *Sent counters track how much of each stream has been flushed to the
// wire (the batcher lags the buffer by up to one flush interval), so stream
// frames can carry the exact end offset of their chunk. Combined with the
// buffer lengths in the rewind payload, a client can merge an attach rewind
// with live content exactly — never duplicating or dropping a character.
type liveTurnState struct {
	thinking      strings.Builder
	thinkingSent  int
	thinkingUnits int
	content       strings.Builder
	contentSent   int
	contentUnits  int
	toolNames     map[int]string
	toolIDs       map[int]string
	toolArgs      map[int]*strings.Builder
	toolArgsUnits map[int]int
}

// liveToolCallState is one in-progress tool call carried in a rewind payload.
type liveToolCallState struct {
	Index   int    `json:"index"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Args    string `json:"args,omitempty"`
	ArgsPos int    `json:"argsPos,omitempty"`
}

// liveRewindState is the in-flight turn's partial output, attached to the
// history payload of a mid-turn attach/resume. The client renders it through
// the normal stream machinery and continues the live stream after it, using
// the positions to trim the boundary exactly.
type liveRewindState struct {
	Content     string              `json:"content,omitempty"`
	ContentPos  int                 `json:"contentPos,omitempty"`
	Thinking    string              `json:"thinking,omitempty"`
	ThinkingPos int                 `json:"thinkingPos,omitempty"`
	ToolCalls   []liveToolCallState `json:"toolCalls,omitempty"`
}

// liveTurnBegin resets the buffer for a new turn (OnStart).
func (rt *sessionRuntime) liveTurnBegin() {
	rt.liveMu.Lock()
	rt.resetLiveLocked()
	rt.liveMu.Unlock()
}

// liveRoundBegin resets the buffer for a new round (OnRoundStart).
func (rt *sessionRuntime) liveRoundBegin() {
	rt.liveMu.Lock()
	rt.resetLiveLocked()
	rt.liveMu.Unlock()
}

// liveRoundEnd clears the buffer (OnStreamEnd). The round's assistant
// message is appended to a.Messages immediately after, so the buffer only
// ever carries content a history snapshot would otherwise miss.
func (rt *sessionRuntime) liveRoundEnd() {
	rt.liveMu.Lock()
	rt.resetLiveLocked()
	rt.liveMu.Unlock()
}

func (rt *sessionRuntime) resetLiveLocked() {
	rt.live.thinking.Reset()
	rt.live.thinkingSent = 0
	rt.live.thinkingUnits = 0
	rt.live.content.Reset()
	rt.live.contentSent = 0
	rt.live.contentUnits = 0
	rt.live.toolNames = nil
	rt.live.toolIDs = nil
	rt.live.toolArgs = nil
	rt.live.toolArgsUnits = nil
}

// liveUTF16Len returns the number of UTF-16 code units in s — the unit the
// browser's JS string length and slice operate in. Positions stamped on the
// wire use this (not byte counts) so the client's rewind trim can slice
// exactly, including multi-byte (emoji/CJK) content.
func liveUTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if l := utf16.RuneLen(r); l > 0 {
			n += l
		} else {
			n++
		}
	}
	return n
}

// liveAppendContent records a streamed content token (OnToken).
func (rt *sessionRuntime) liveAppendContent(text string) {
	if text == "" {
		return
	}
	rt.liveMu.Lock()
	rt.live.content.WriteString(text)
	rt.live.contentUnits += liveUTF16Len(text)
	rt.liveMu.Unlock()
}

// liveAppendThinking records a streamed thinking token (OnThinkingToken).
func (rt *sessionRuntime) liveAppendThinking(text string) {
	if text == "" {
		return
	}
	rt.liveMu.Lock()
	rt.live.thinking.WriteString(text)
	rt.live.thinkingUnits += liveUTF16Len(text)
	rt.liveMu.Unlock()
}

// liveContentSegmentEnd advances the sent-content marker by one flushed
// segment and returns the segment's end offset in the round's content
// stream. Called from the token batcher's send callback per content segment.
func (rt *sessionRuntime) liveContentSegmentEnd(text string) int {
	rt.liveMu.Lock()
	rt.live.contentSent += liveUTF16Len(text)
	pos := rt.live.contentSent
	rt.liveMu.Unlock()
	return pos
}

// liveThinkingSegmentEnd is liveContentSegmentEnd for thinking segments.
func (rt *sessionRuntime) liveThinkingSegmentEnd(text string) int {
	rt.liveMu.Lock()
	rt.live.thinkingSent += liveUTF16Len(text)
	pos := rt.live.thinkingSent
	rt.liveMu.Unlock()
	return pos
}

// liveToolStart records the start of a streamed tool call (OnToolCallStart).
func (rt *sessionRuntime) liveToolStart(index int, id, name string) {
	rt.liveMu.Lock()
	if rt.live.toolNames == nil {
		rt.live.toolNames = make(map[int]string)
		rt.live.toolIDs = make(map[int]string)
		rt.live.toolArgs = make(map[int]*strings.Builder)
	}
	rt.live.toolNames[index] = name
	rt.live.toolIDs[index] = id
	rt.live.toolArgs[index] = &strings.Builder{}
	if rt.live.toolArgsUnits == nil {
		rt.live.toolArgsUnits = make(map[int]int)
	}
	rt.live.toolArgsUnits[index] = 0
	rt.liveMu.Unlock()
}

// liveToolArgsAppend records one args delta for a streaming tool call
// (OnToolCallArgsDelta) and returns the call's accumulated args length.
func (rt *sessionRuntime) liveToolArgsAppend(index int, delta string) int {
	rt.liveMu.Lock()
	if rt.live.toolArgs == nil {
		rt.live.toolArgs = make(map[int]*strings.Builder)
		rt.live.toolArgsUnits = make(map[int]int)
	} else if rt.live.toolArgsUnits == nil {
		rt.live.toolArgsUnits = make(map[int]int)
	}
	b := rt.live.toolArgs[index]
	if b == nil {
		b = &strings.Builder{}
		rt.live.toolArgs[index] = b
		rt.live.toolArgsUnits[index] = 0
	}
	b.WriteString(delta)
	rt.live.toolArgsUnits[index] += liveUTF16Len(delta)
	pos := rt.live.toolArgsUnits[index]
	rt.liveMu.Unlock()
	return pos
}

// liveRewind snapshots the current round's in-flight output for an attach
// history payload. Returns nil when nothing has streamed yet (or the turn is
// between rounds — completed content lives in a.Messages already).
func (rt *sessionRuntime) liveRewind() *liveRewindState {
	rt.liveMu.Lock()
	defer rt.liveMu.Unlock()
	if rt.live.content.Len() == 0 && rt.live.thinking.Len() == 0 && len(rt.live.toolNames) == 0 {
		return nil
	}
	rw := &liveRewindState{
		Content:     rt.live.content.String(),
		ContentPos:  rt.live.contentUnits,
		Thinking:    rt.live.thinking.String(),
		ThinkingPos: rt.live.thinkingUnits,
	}
	for idx, name := range rt.live.toolNames {
		argsStr := ""
		argsPos := 0
		if b := rt.live.toolArgs[idx]; b != nil {
			argsStr = b.String()
			argsPos = rt.live.toolArgsUnits[idx]
		}
		rw.ToolCalls = append(rw.ToolCalls, liveToolCallState{
			Index: idx, ID: rt.live.toolIDs[idx], Name: name, Args: argsStr, ArgsPos: argsPos,
		})
	}
	sort.Slice(rw.ToolCalls, func(i, j int) bool { return rw.ToolCalls[i].Index < rw.ToolCalls[j].Index })
	return rw
}

// attach registers a socket as a viewer of this session. Broadcasts reach all
// attached sockets; the first client to send a message starts the turn.
func (rt *sessionRuntime) attach(ws *wsConn) {
	if ws == nil {
		return
	}
	rt.clientsMu.Lock()
	rt.clients[ws] = struct{}{}
	rt.clientsMu.Unlock()
	// A reconnecting client missed the original delete_approval broadcast.
	// Re-notify it of every pending approval (F2: the hold window is only
	// actionable if the reconnected client knows the approval ids). The
	// response routes by approvalId, so duplicate notifications are harmless.
	rt.approvalMu.Lock()
	pending := make([]struct {
		id     string
		paths  []string
		reason string
	}, 0, len(rt.approvals))
	for id, pa := range rt.approvals {
		pending = append(pending, struct {
			id     string
			paths  []string
			reason string
		}{id: id, paths: pa.paths, reason: pa.reason})
	}
	rt.approvalMu.Unlock()
	for _, p := range pending {
		_ = ws.writeJSON(WSMessage{
			Type:       "delete_approval",
			ApprovalID: p.id,
			Paths:      p.paths,
			Reason:     p.reason,
			SessionID:  rt.agent.SessionID,
		})
	}
}

// detach removes a socket from the session. It never cancels the turn: the
// turn belongs to the runtime, not the connection. When the last
// attached client leaves, pending delete approvals are auto-denied so
// an unattended turn cannot hang forever on a destructive prompt.
func (rt *sessionRuntime) detach(ws *wsConn) {
	if ws == nil {
		return
	}
	rt.clientsMu.Lock()
	_, ok := rt.clients[ws]
	if ok {
		delete(rt.clients, ws)
	}
	empty := len(rt.clients) == 0
	rt.clientsMu.Unlock()
	if ok && empty {
		if rt.approvalHold > 0 {
			// F2: give a reconnecting client a window to answer pending
			// approvals. The timer is idempotent: if a client re-attaches and
			// answers first, completeApproval consumes the channel and the
			// eventual auto-deny finds nothing pending; if they re-attach but
			// do not answer, the hold still expires into auto-deny so an
			// unattended turn cannot hang forever.
			rt.scheduleAutoDenyAfter(rt.approvalHold)
		} else {
			rt.autoDenyPendingApprovals()
		}
		// Last viewer left: an idle runtime is now an orphan — flush and
		// unregister it so it reads as a plain saved session instead of a
		// stale "resume to continue" row. A running turn keeps it
		// registered; the turn-end hook evicts it when the turn completes.
		rt.evictOrphanedIfPossible()
	}
}

// scheduleAutoDenyAfter denies every pending approval after `after` unless a
// client answered them first. See detach for the idempotency argument.
func (rt *sessionRuntime) scheduleAutoDenyAfter(after time.Duration) {
	rt.approvalMu.Lock()
	if len(rt.approvals) == 0 {
		rt.approvalMu.Unlock()
		return
	}
	rt.approvalMu.Unlock()
	time.AfterFunc(after, func() {
		rt.autoDenyPendingApprovals()
	})
}

// clientCount returns the number of attached sockets (for tests/shutdown).
func (rt *sessionRuntime) clientCount() int {
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	return len(rt.clients)
}

// broadcast writes a message to every attached socket. A socket whose write
// fails is detached (write failure detaches, never cancels);
// the stream goroutine's enqueue is bounded by the send-queue timeout, so a
// dead socket cannot stall the turn.
func (rt *sessionRuntime) broadcast(msg WSMessage) {
	rt.clientsMu.Lock()
	clients := make([]*wsConn, 0, len(rt.clients))
	for c := range rt.clients {
		clients = append(clients, c)
	}
	rt.clientsMu.Unlock()
	for _, c := range clients {
		if err := c.writeJSON(msg); err != nil {
			rt.detach(c)
		}
	}
}

// detachAllClients removes every attached socket from the session. Used when
// a runtime is leaving the registry (session_delete, eviction): every
// attachment must be released — not just one connection's — or the removed
// runtime's clients set would leak the other sockets until their connection
// teardown, and teardown's detachAll only sweeps REGISTERED sessions, so it
// would never reach this runtime. detach is idempotent and never cancels a
// turn; callers broadcast the session_detached/session_removed notification
// BEFORE sweeping so the message reaches every attached socket first.
func (rt *sessionRuntime) detachAllClients() {
	rt.clientsMu.Lock()
	clients := make([]*wsConn, 0, len(rt.clients))
	for c := range rt.clients {
		clients = append(clients, c)
	}
	rt.clientsMu.Unlock()
	for _, c := range clients {
		rt.detach(c)
	}
}

// evictOrphanedIfPossible asks the owning registry to evict this runtime if
// it became an orphan (no attached clients, no running turn). No-op for
// runtimes built outside a registry (registry field nil) and for runtimes
// that are already evicted.
func (rt *sessionRuntime) evictOrphanedIfPossible() {
	if rt.registry == nil {
		return
	}
	rt.registry.evictOrphaned(rt)
}

// setTurnActive records whether a turn is running (session_state payload) and
// which connection owns it.
func (rt *sessionRuntime) setTurnActive(active bool, startedAt time.Time, owner *wsConn) {
	rt.stateMu.Lock()
	rt.turnActive = active
	rt.startedAt = startedAt
	rt.turnOwner = owner
	rt.stateMu.Unlock()
}

// turnState returns the current turn-active flag and start time.
func (rt *sessionRuntime) turnState() (bool, time.Time) {
	rt.stateMu.Lock()
	defer rt.stateMu.Unlock()
	return rt.turnActive, rt.startedAt
}

// ownsTurn reports whether ws started the currently running turn. Only the
// owner may interrupt it via the cancel-then-lock path (E3/E29); everyone
// else waits and gets the busy rejection.
func (rt *sessionRuntime) ownsTurn(ws *wsConn) bool {
	if ws == nil {
		return false
	}
	rt.stateMu.Lock()
	defer rt.stateMu.Unlock()
	return rt.turnOwner == ws
}

// completeApproval resolves a pending delete approval. Later responses for
// the same approvalId are no-ops (the channel is already consumed, §3.2).
func (rt *sessionRuntime) completeApproval(id string, approved bool) {
	rt.approvalMu.Lock()
	ch := rt.approvals[id]
	delete(rt.approvals, id)
	rt.approvalMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch.ch <- approved:
	default:
		// Waiter already left (cancel) or buffer full — don't block the reader.
	}
}

// autoDenyPendingApprovals denies every pending approval on this session.
// Used when the last attached client detaches: the turn continues with
// the "not approved" tool result instead of hanging.
func (rt *sessionRuntime) autoDenyPendingApprovals() {
	rt.approvalMu.Lock()
	for id, ch := range rt.approvals {
		delete(rt.approvals, id)
		select {
		case ch.ch <- false:
		default:
		}
	}
	rt.approvalMu.Unlock()
}

// deleteApprover returns the session-scoped delete approver. Requests are
// broadcast to all attached clients; the first response wins.
func (rt *sessionRuntime) deleteApprover() agent.DeleteApprover {
	return func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		id := newApprovalID()
		ch := make(chan bool, 1)

		rt.approvalMu.Lock()
		rt.approvals[id] = &pendingApproval{ch: ch, paths: req.Paths, reason: req.Reason}
		rt.approvalMu.Unlock()

		defer func() {
			rt.approvalMu.Lock()
			delete(rt.approvals, id)
			rt.approvalMu.Unlock()
		}()

		rt.broadcast(WSMessage{
			Type:       "delete_approval",
			ApprovalID: id,
			Paths:      req.Paths,
			Reason:     req.Reason,
			SessionID:  rt.agent.SessionID,
		})

		select {
		case approved := <-ch:
			return approved, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// sessionRegistry owns the live sessions of the web server. The map itself
// is guarded by mu (short critical sections only); per-session state is
// serialized by the session's own turnMu.
type sessionRegistry struct {
	mu        sync.RWMutex
	agents    map[string]*sessionRuntime
	order     []string // session ids in registration order (LRU eviction, §4)
	maxActive int
}

func newSessionRegistry(maxActive int) *sessionRegistry {
	if maxActive <= 0 {
		maxActive = config.DefaultWebMaxActiveSessions
	}
	return &sessionRegistry{
		agents:    make(map[string]*sessionRuntime),
		maxActive: maxActive,
	}
}

// register adds a runtime under id and returns the ids of any sessions it
// evicted to make room (empty when none). Registering an already-active id is
// a no-op (dedupe, E9) returning nil. Evicted sessions were flushed before
// eviction; their ids are returned so callers that prune the store right
// after (session create/fork) can protect the fresh files (E2/E11).
func (r *sessionRegistry) register(id string, rt *sessionRuntime) []string {
	var victims []*sessionRuntime
	var victimIDs []string
	r.mu.Lock()
	if id == "" || rt == nil {
		r.mu.Unlock()
		return nil
	}
	// Back-pointer for the orphan eviction (detach / turn-end hook). Set
	// before the dedupe no-op below so any runtime that passes through
	// register can evict itself later.
	rt.registry = r
	if _, ok := r.agents[id]; ok {
		r.mu.Unlock()
		return nil
	}
	// Cap the registry (D6/E11): evict the least-recently-active IDLE
	// session (scan from the back of order) until the new registration
	// fits. The default session (front of order) is never evicted —
	// messages without a sessionId and new connections' initial pane
	// depend on it. A session with a running (including headless) turn is
	// never evicted: if every candidate is busy the cap is allowed to be
	// exceeded rather than kill an in-flight turn. Victims are only
	// COLLECTED under the lock (cheap bookkeeping); the flush + notify
	// happen after the lock is released, because broadcast's enqueue can
	// block up to 5s on a stuck socket and must not stall registry
	// lookups. Evicted sessions are flushed (nothing in memory is lost —
	// the client's pane re-attaches from the store, E11) and their
	// attached sockets get session_detached so the client closes the pane
	// instead of sending messages to a stale runtime.
	//
	// The turnState check races a turn that is ABOUT to start: a message
	// handler may have resolved this runtime before the eviction and not
	// yet set turnActive. TryLocking the victim's turnMu closes the window
	// while the eviction holds it (a turn can only run under turnMu, so the
	// flush below cannot race a turn's save), and the evicted flag — set
	// here, before the lock is released — makes the eviction sticky: a
	// handler that acquires turnMu AFTER the eviction sees the flag and
	// drops the message instead of starting a turn on an unregistered
	// runtime. TryLock never blocks: a busy (or starting) session is
	// skipped, exceeding the cap rather than killing the turn.
	//
	// LOCK ORDER (documented invariant): this loop takes r.mu → turnMu, and
	// sessionDelete takes turnMu → r.mu (registry.remove) — the reverse
	// order. That is NOT a deadlock only because neither path ever blocks on
	// the other's inner lock: the victim lock here is a non-blocking TryLock
	// and sessionDelete's turnMu acquisition is also a TryLock. Do not
	// convert either to a blocking Lock()/RLock() without reworking both
	// paths.
	for i := len(r.order) - 1; len(r.agents) >= r.maxActive && i > 0; i-- {
		victimID := r.order[i]
		victim := r.agents[victimID]
		if active, _ := victim.turnState(); active {
			continue
		}
		if !victim.turnMu.TryLock() {
			continue
		}
		victim.evicted.Store(true)
		delete(r.agents, victimID)
		r.order = append(r.order[:i], r.order[i+1:]...)
		victims = append(victims, victim)
		victimIDs = append(victimIDs, victimID)
	}
	r.agents[id] = rt
	r.order = append(r.order, id)
	r.mu.Unlock()
	for i, victim := range victims {
		// Persist any UNSAVED state before eviction (FlushPending writes only
		// when the session is dirty). FlushSession would force a write on a
		// clean session too, re-stamping its Updated timestamp with ~now —
		// the evicted session was evicted for being the LEAST recently
		// active, and stamping it newest would push it to the top of the
		// saved-session list and distort the recency ordering that
		// Store.List/LatestID/Prune rely on.
		victim.agent.FlushPending()
		victim.turnMu.Unlock()
		victim.broadcast(WSMessage{Type: "session_detached", SessionID: victimIDs[i]})
		// Detach every attached socket AFTER the notification (the client
		// closes its pane on session_detached): the victim left the
		// registry under the lock above, and teardown's detachAll only
		// sweeps REGISTERED sessions — an unattended attachment here would
		// leak the socket in the evicted runtime's clients set.
		victim.detachAllClients()
	}
	return victimIDs
}

// evictRuntime is the shared eviction tail: flush the session, remove the
// runtime from the registry, then notify any client that raced in between
// (session_detached — it re-attaches from the store) and detach it. The
// session stays saved on disk; reopening it (session_attach / resume) loads
// a fresh runtime. Callers must hold (or win) the runtime's turnMu so no new
// turn can start on a runtime that is about to leave the registry; the lock
// order is turnMu → r.mu, matching sessionDelete and the cap eviction's
// TryLock.
//
// Removal happens BEFORE the notification so the registry never contains a
// dying-but-registered runtime: a concurrent session_attach/resume of the
// same id can never resolve it — the lookup fails, the caller loads fresh
// from the store and registers cleanly — instead of attaching to a runtime
// that remove() is about to yank out from under it. This mirrors the cap
// eviction in register, which deletes the victim from the map before any
// session_detached is sent.
//
// The flush is FlushPending — it writes only when the session is dirty — for
// every eviction path, including the explicit session_close: the close drain
// (cancelInFlight) waits for the cancelled turn's own final persist, so by
// the time eviction runs a clean session's state is already on disk and the
// forced FlushSession would only re-stamp its Updated timestamp with ~now.
// That stamp pushed a just-closed session to the top of the saved-session
// list (recency ordering is interaction-based) and made "resume latest"
// target a session the user merely closed. A genuinely dirty runtime
// (unsaved turn state) still persists before eviction, so no state is at
// risk (a failed save whose error was consumed by the turn-end warning is
// retried by the next FlushSession/persistSession, exactly as before).
func (r *sessionRegistry) evictRuntime(rt *sessionRuntime) {
	rt.agent.FlushPending()
	// The runtime is leaving memory: its background jobs (execute_command
	// background=true) have no owner left to poll them, so kill them rather
	// than leak orphan processes. Idempotent; turns are unaffected.
	rt.agent.Close()
	r.remove(rt.agent.SessionID)
	rt.broadcast(WSMessage{Type: "session_detached", SessionID: rt.agent.SessionID})
	// Sweep attachments after the notification: a socket that raced in (or
	// was still attached) must not linger in the removed runtime's clients
	// set — teardown's detachAll only sweeps REGISTERED sessions and would
	// never reach it. detach is idempotent and never cancels a turn.
	rt.detachAllClients()
}

// closeRuntime cancels any in-flight turn (draining it ≤ wsStreamDrainWait),
// then evicts the runtime. Used by the explicit session_close message: the
// user pressed ✕ on the pane — stop the session and put it back in the saved
// list. The session stays saved on disk.
func (r *sessionRegistry) closeRuntime(rt *sessionRuntime) {
	if rt == nil || rt.evicted.Load() {
		return
	}
	rt.stream.cancelInFlight()
	// TryLock: if the drain timed out (the cancelled turn's goroutine is
	// still alive, e.g. stuck in a tool), that turn itself holds the lock —
	// proceed without it; the evicted flag prevents new turns regardless.
	if held := rt.turnMu.TryLock(); held {
		defer rt.turnMu.Unlock()
	}
	r.evictRuntime(rt)
}

// evictOrphaned unregisters a runtime with no attached clients and no running
// turn — the "resume to continue" state nobody is viewing (a page refresh, a
// closed tab, a re-keyed-away session, or a headless turn that just
// finished). The session stays saved on disk. Mirrors the cap-eviction
// pipeline: flush before evict, remove from the registry, then notify
// (session_detached) and detach any client that raced in between — see
// evictRuntime for the ordering rationale.
//
// A turn can only run while holding turnMu (startTurn's caller acquires it),
// so winning the TryLock proves the runtime is idle — and a concurrent
// message handler that resolved this runtime before the eviction cannot
// start a turn on it: its tryAcquireTurn loses the race and the evicted flag
// makes the guard sticky. A running turn's goroutine holds the lock, so
// TryLock fails and we skip — the turn-end hook re-checks when the turn
// finishes.
func (r *sessionRegistry) evictOrphaned(rt *sessionRuntime) {
	if rt == nil || rt.evicted.Load() {
		return
	}
	if rt.clientCount() > 0 {
		return
	}
	if active, _ := rt.turnState(); active {
		return
	}
	if !rt.turnMu.TryLock() {
		return
	}
	defer rt.turnMu.Unlock()
	r.evictRuntime(rt)
}

// get returns the runtime for id.
func (r *sessionRegistry) get(id string) (*sessionRuntime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.agents[id]
	return rt, ok
}

// remove evicts the runtime for id.
func (r *sessionRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[id]; !ok {
		return
	}
	r.agents[id].evicted.Store(true)
	delete(r.agents, id)
	for i, oid := range r.order {
		if oid == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// setDefault makes id the default session (first in registration order), so
// messages without a sessionId route to it (D3: typed /new replaces the
// pane's session).
func (r *sessionRegistry) setDefault(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[id]; !ok {
		return
	}
	order := r.order[:0]
	for _, oid := range r.order {
		if oid != id {
			order = append(order, oid)
		}
	}
	r.order = append([]string{id}, order...)
}

// first returns the first registered runtime (the default session).
func (r *sessionRegistry) first() *sessionRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return nil
	}
	return r.agents[r.order[0]]
}

// activeIDs returns the ids of all registered sessions in registration order
// (the full protected set for store.Prune, E2).
func (r *sessionRegistry) activeIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.order))
	out = append(out, r.order...)
	return out
}

// detachAll removes the connection from every registered session it may be
// attached to: the current pane plus any background panes opened via
// session_attach. A killed tab cannot send session_detach for each pane, so
// connection teardown sweeps the registry (detach is idempotent and never
// cancels a turn). Without the sweep, a background pane's dead
// socket would linger in its runtime's clients set until the next broadcast
// failed — and if the connection died while that session awaited a delete
// approval, the turn would hang forever instead of auto-denying (D10: the
// last attached client leaving must deny pending approvals).
func (r *sessionRegistry) detachAll(ws *wsConn) {
	if ws == nil || r == nil {
		return
	}
	for _, id := range r.activeIDs() {
		if rt, ok := r.get(id); ok {
			rt.detach(ws)
		}
	}
}

// ShutdownSessions drains in-flight streams and flushes every registered
// session agent so a multi-session web server exits without losing in-memory
// state. It is safe to call with zero clients attached (turns detach first —
// Start's ForceClose closes sockets before this runs, and closing a socket
// must never cancel its session's turn, or the disconnect-kills-turn bug
// returns). Each cancelInFlight bounds its drain at wsStreamDrainWait; the
// whole sweep is at most N×that, one session at a time.
//
// The flush is FlushPending, not FlushSession: the sweep must persist any
// state a running turn left unsaved, but must NOT force a full write on
// sessions whose last turn already flushed. Forcing a write on every clean
// session re-stamped each one's Updated timestamp with ~now in registry
// order (the focused session first, so it received the OLDEST stamp), which
// destroyed the recency ordering Store.List/LatestID rely on after a
// restart — the saved-session list reshuffled and the session that was
// active at shutdown was demoted instead of restored as current.
func (s *Server) ShutdownSessions() {
	if s == nil || s.registry == nil {
		return
	}
	// Drain + flush LEAST-recently-active first. Registry order has the
	// focused session at the front (setDefault moves it there on attach/
	// new/resume/fork), so a forward sweep stamps the Updated timestamps of
	// sessions that are still DIRTY at quit with ~now oldest-first — the
	// focused session, flushed first, received the OLDEST stamp and was
	// demoted on restart (the same reshuffle FlushPending fixed for clean
	// sessions). Reversing the sweep gives the most recently active session
	// the NEWEST stamp, so Store.List/LatestID keep the pre-shutdown recency
	// order for dirty sessions too.
	ids := s.registry.activeIDs()
	for i := len(ids) - 1; i >= 0; i-- {
		rt, ok := s.registry.get(ids[i])
		if !ok {
			continue
		}
		rt.stream.cancelInFlight()
		rt.agent.FlushPending()
		// Kill background jobs so no command outlives the process.
		rt.agent.Close()
	}
}

// acquireTurnForHandler implements the cancel-then-lock pattern scoped to one
// session: it cancels the in-flight turn only when THIS connection owns
// it (interrupt semantics — typing a new message replaces your own turn). A
// second connection attached to the same session never cancels a turn
// it does not own; it waits and gets the standard busy rejection.
func (rt *sessionRuntime) acquireTurnForHandler(ws *wsConn) bool {
	if rt.ownsTurn(ws) {
		rt.stream.cancelInFlight()
	}
	if !rt.tryAcquireTurn(wsTurnAcquireWait) {
		if rt.ownsTurn(ws) {
			rt.stream.cancelInFlight()
		}
		if !rt.tryAcquireTurn(wsStreamDrainWait) {
			_ = ws.writeJSON(WSMessage{Type: "response", Content: "Error: agent is busy with another client"})
			return false
		}
	}
	// The session may have been evicted from the registry (cap eviction or
	// delete) after the caller resolved it but before the lock was acquired.
	// Starting a turn (or mutating config) on an evicted runtime would be
	// invisible to cancel/prune/shutdown. The flag is set while the eviction
	// holds turnMu, so checking it now — under the lock — is race-free:
	// either this handler acquired first (the eviction skipped the session)
	// or the eviction completed first (the flag is set). Drop silently: the
	// client already received session_detached and closed the pane.
	if rt.evicted.Load() {
		rt.turnMu.Unlock()
		return false
	}
	return true
}

// tryAcquireTurn waits briefly for the session's turnMu (e.g. after
// cancelling our own stream). Returns false if another client still holds the
// session's turn lock.
func (rt *sessionRuntime) tryAcquireTurn(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if rt.turnMu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

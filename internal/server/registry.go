package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
)

// sessionRuntime is one live chat session in the web server: the agent, the
// per-session turn lock, and — since Phase 3 — the turn machinery that used
// to live per connection: the in-flight stream handles, the pending delete
// approvals, and the set of attached client sockets (fan-out). A turn is
// owned by the runtime, not by any connection: disconnecting the last client
// detaches it but the turn keeps running server-side and persists normally
// (§4 connection-loss continuation).
type sessionRuntime struct {
	agent  *agent.Agent
	turnMu sync.RWMutex // turns take Lock; config/history reads take RLock

	// stream owns the in-flight stream cancel handles for this session
	// (moved from per-connection, Phase 3).
	stream *wsConnStream

	// approvals are pending delete-approval channels, keyed by approvalID
	// (per-session, so two sessions' approvals cannot collide, E5).
	approvalMu sync.Mutex
	approvals  map[string]chan bool

	// clients are the attached sockets; broadcast() fans events out to all
	// of them (E29). A write failure detaches that socket, never cancels
	// the turn (E26).
	clientsMu sync.Mutex
	clients   map[*wsConn]struct{}

	// turnActive/startedAt back the session_state reply on attach so a
	// reconnecting client can distinguish "turn running headless" from
	// "idle" (E28). turnOwner is the connection that started the current
	// turn; only it may interrupt via the cancel-then-lock path — a second
	// connection attached to the same session (E29) must not kill the turn
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
	// never re-registered — resume/attach build a fresh one (E9).
	evicted atomic.Bool

	// registry is the owning sessionRegistry. Set by register(); used by
	// detach and the turn-end hook to evict the runtime when it becomes an
	// orphan — no attached clients and no running turn (a warm runtime
	// nobody is viewing should read as a plain saved session, not a stale
	// "resume to continue" row). Nil for runtimes built outside a registry
	// (unit tests): the orphan checks then no-op.
	registry *sessionRegistry
}

// newSessionRuntime builds a runtime with the per-session turn machinery
// initialized. The agent must be non-nil.
func newSessionRuntime(a *agent.Agent) *sessionRuntime {
	return &sessionRuntime{
		agent:     a,
		stream:    &wsConnStream{},
		approvals: make(map[string]chan bool),
		clients:   make(map[*wsConn]struct{}),
	}
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
}

// detach removes a socket from the session. It never cancels the turn: the
// turn belongs to the runtime, not the connection (Phase 3). When the last
// attached client leaves, pending delete approvals are auto-denied (D10) so
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
		rt.autoDenyPendingApprovals()
		// Last viewer left: an idle runtime is now an orphan — flush and
		// unregister it so it reads as a plain saved session instead of a
		// stale "resume to continue" row. A running turn keeps it
		// registered; the turn-end hook evicts it when the turn completes.
		rt.evictOrphanedIfPossible()
	}
}

// clientCount returns the number of attached sockets (for tests/shutdown).
func (rt *sessionRuntime) clientCount() int {
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	return len(rt.clients)
}

// broadcast writes a message to every attached socket. A socket whose write
// fails is detached (write failure detaches, never cancels — Phase 3 §4);
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
	case ch <- approved:
	default:
		// Waiter already left (cancel) or buffer full — don't block the reader.
	}
}

// autoDenyPendingApprovals denies every pending approval on this session.
// Used when the last attached client detaches (D10): the turn continues with
// the "not approved" tool result instead of hanging.
func (rt *sessionRuntime) autoDenyPendingApprovals() {
	rt.approvalMu.Lock()
	for id, ch := range rt.approvals {
		delete(rt.approvals, id)
		select {
		case ch <- false:
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
		rt.approvals[id] = ch
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
// pipeline (E11): flush before evict, remove from the registry, then notify
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
// cancels a turn, Phase 3 §4). Without the sweep, a background pane's dead
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
	}
}

// acquireTurnForHandler implements the cancel-then-lock pattern scoped to one
// session (E33): it cancels the in-flight turn only when THIS connection owns
// it (interrupt semantics — typing a new message replaces your own turn). A
// second connection attached to the same session (E29) never cancels a turn
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

package server

import (
	"time"
)

// defaultDeliverQueueCap bounds pending system message deliveries per
// runtime. Overflow drops the OLDEST delivery (freshness wins — a stale job
// notice is worse than none) and toasts the drop.
const defaultDeliverQueueCap = 5

// deliverToSession injects a system-generated message into the session as a
// user message and runs a turn on it at the next idle boundary. It never
// blocks: the message is queued and a per-runtime worker delivers it.
//
// Producers (job completion notices, scheduled reminders, subagent reports)
// call this from arbitrary goroutines; the worker owns all interaction with
// the turn machinery, so the "Messages are owned by the turn goroutine"
// invariant holds by construction.
//
// Delivery is at-most-once: the item is popped BEFORE the turn starts, so a
// crash between pop and start loses one message rather than duplicating one.
//
// Returns false when the message was NOT accepted (nil runtime, or the
// runtime was evicted between the caller's lookup and this call). Callers
// that must not lose the message can fall back to deliverToParent, which
// queues it for the session's next registration.
func (rt *sessionRuntime) deliverToSession(text string) bool {
	if rt == nil || rt.evicted.Load() {
		return false
	}
	rt.deliverMu.Lock()
	if len(rt.pendingDeliver) >= defaultDeliverQueueCap {
		copy(rt.pendingDeliver, rt.pendingDeliver[1:])
		rt.pendingDeliver = rt.pendingDeliver[:len(rt.pendingDeliver)-1]
		rt.broadcast(WSMessage{Type: "notice", Kind: "delivery", Success: false,
			Content: "A background message was dropped (delivery queue full)."})
	}
	rt.pendingDeliver = append(rt.pendingDeliver, text)
	if !rt.deliverWorker.Swap(true) {
		go rt.deliverLoop()
	}
	rt.deliverMu.Unlock()
	select {
	case rt.deliverNotify <- struct{}{}:
	default:
	}
	return true
}

// --- parent-scoped delivery (survives runtime eviction) ---

// deliverToParent delivers a system message to a live parent session, or —
// when the session's runtime is not live (orphan-evicted, closed) — queues
// it in the registry so it is delivered when the session is next registered
// (reopened from the store). Never blocks; at-most-once per queue entry.
// Used by the continuable-subagent machinery, whose producers must not lose
// a completion notice or report to a parent that merely went idle with no
// viewers.
func (r *sessionRegistry) deliverToParent(parentID, text string) {
	if rt, ok := r.get(parentID); ok {
		if rt.deliverToSession(text) {
			return
		}
		// The runtime was evicted between the lookup and the queue append:
		// fall through and queue for the next registration.
	}
	r.queueParentDelivery(parentID, text)
}

// queueParentDelivery stashes a system message for a session whose runtime
// is not live. Bounded like the per-runtime delivery queue: overflow drops
// the OLDEST entry (freshness wins — a stale notice is worse than none).
// Flushed by register() when the session is next loaded; cleared by
// sessionDelete (a deleted session can never be reopened).
func (r *sessionRegistry) queueParentDelivery(parentID, text string) {
	if parentID == "" {
		return
	}
	r.parentDeliverMu.Lock()
	defer r.parentDeliverMu.Unlock()
	q := r.parentDeliveries[parentID]
	if len(q) >= defaultDeliverQueueCap {
		q = append([]string(nil), q[1:]...) // drop the oldest entry
	}
	r.parentDeliveries[parentID] = append(q, text)
}

// flushParentDeliveries delivers every queued message for id into rt (the
// freshly registered runtime of a session that was evicted/closed while its
// background subagents kept running). No-op when nothing is queued. Must
// NOT be called with r.mu held: deliverToSession can broadcast (bounded by
// the send-queue timeout).
func (r *sessionRegistry) flushParentDeliveries(id string, rt *sessionRuntime) {
	if rt == nil {
		return
	}
	r.parentDeliverMu.Lock()
	pending := r.parentDeliveries[id]
	delete(r.parentDeliveries, id)
	r.parentDeliverMu.Unlock()
	for _, text := range pending {
		rt.deliverToSession(text)
	}
}

// clearParentDeliveries drops the queued deliveries for id. Used when the
// session is DELETED: its file is gone, so it can never be reopened and the
// queue would be dead weight in memory.
func (r *sessionRegistry) clearParentDeliveries(id string) {
	r.parentDeliverMu.Lock()
	delete(r.parentDeliveries, id)
	r.parentDeliverMu.Unlock()
}

// deliverLoop is the per-runtime delivery worker (at most one per runtime,
// guarded by deliverWorker). It pops one queued message, waits for the
// session turn lock (the idle check), hands the lock to startTurn (whose
// goroutine defers the unlock), and repeats until the queue is empty.
//
// The wake-up signal comes from setTurnActive(false), which every turn
// runner (startTurn, runChildTurn, compact) calls on exit — so the worker
// wakes exactly when a turn ends, without polling. A failed acquire does
// not mean a turn is running (brief config handlers also take turnMu), so
// the worker keeps the item at the HEAD (FIFO, peek-not-pop) and waits for
// the signal with a short timeout fallback to cover holders that never
// signal. The item is popped only once the turn lock is acquired, so
// orphan eviction (hasPendingDeliveries) never races the handoff.
func (rt *sessionRuntime) deliverLoop() {
	for {
		rt.deliverMu.Lock()
		if len(rt.pendingDeliver) == 0 {
			rt.deliverWorker.Store(false)
			rt.deliverMu.Unlock()
			return
		}
		// Peek, do NOT pop yet: the item must stay visible to
		// evictOrphaned's hasPendingDeliveries until the delivery turn is
		// actually starting. Popping first opened a handoff window (queue
		// empty, turnMu still free) where the just-ended turn's orphan
		// re-check could evict the runtime and drop the delivery.
		text := rt.pendingDeliver[0]
		rt.deliverMu.Unlock()

		if rt.evicted.Load() {
			rt.deliverMu.Lock()
			rt.pendingDeliver = rt.pendingDeliver[1:]
			rt.deliverMu.Unlock()
			continue // dropped: the runtime left the registry mid-queue
		}
		if !rt.tryAcquireTurn(wsTurnAcquireWait) {
			// Keep the item at the head (FIFO) and wait for the turn-end
			// signal with a short timeout fallback.
			select {
			case <-rt.deliverNotify:
			case <-time.After(wsTurnAcquireWait):
			}
			continue
		}
		// Re-check under the lock: the runtime may have been evicted while
		// we waited (orphan eviction wins the TryLock between our pop and
		// acquire; closeRuntime/delete evict without the lock). Starting a
		// turn on an evicted runtime would stream into the void and
		// persist a message nobody sees — drop it (at-most-once).
		if rt.evicted.Load() {
			rt.deliverMu.Lock()
			rt.pendingDeliver = rt.pendingDeliver[1:]
			rt.deliverMu.Unlock()
			rt.turnMu.Unlock()
			continue
		}
		// The turn lock is ours and the runtime is live: pop and hand the
		// lock to startTurn (its goroutine defers the unlock). The
		// delivery-start hook (if any) fires immediately before, so
		// consumers can arm per-delivery state exactly when the delivered
		// turn begins.
		rt.deliverMu.Lock()
		rt.pendingDeliver = rt.pendingDeliver[1:]
		rt.deliverMu.Unlock()
		if rt.deliverStartHook != nil {
			rt.deliverStartHook()
		}
		rt.startTurn(nil, text, nil)
	}
}

// signalDeliveries wakes the delivery worker after a turn ends. Called from
// setTurnActive's active→false transition, which every turn runner performs
// on exit — a single hook covering startTurn, runChildTurn, and compact
// without touching any defer chain.
func (rt *sessionRuntime) signalDeliveries() {
	select {
	case rt.deliverNotify <- struct{}{}:
	default:
	}
}

// hasPendingDeliveries reports whether the runtime has undelivered queued
// system messages. Consulted by orphan eviction: a runtime with a non-empty
// queue is not idle.
func (rt *sessionRuntime) hasPendingDeliveries() bool {
	if rt == nil {
		return false
	}
	rt.deliverMu.Lock()
	defer rt.deliverMu.Unlock()
	return len(rt.pendingDeliver) > 0
}

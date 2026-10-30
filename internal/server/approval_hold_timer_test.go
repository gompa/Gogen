package server

import (
	"context"
	"testing"
	"time"

	"gogen/internal/agent"
)

// TestApprovalHoldTimerSparesReattachedClient pins the hold timer's scope: it
// exists to answer "nobody is looking", so it must do nothing when a client is
// attached again when it fires. The old blanket deny answered a prompt the
// re-attached client was looking at, and also killed approvals raised AFTER the
// detach that armed the timer.
func TestApprovalHoldTimerSparesReattachedClient(t *testing.T) {
	rt := newTestRuntime(t)
	// Any hold works; the no-deny window below is >2x it, so the timer has
	// certainly fired inside the window.
	rt.approvalHold = 400 * time.Millisecond

	ws := newFakeWSConn()
	rt.clientsMu.Lock()
	rt.clients[ws] = struct{}{}
	rt.clientsMu.Unlock()

	done := make(chan bool, 1)
	go func() {
		approved, err := rt.deleteApprover()(context.Background(), agent.DeleteRequest{Paths: []string{"x"}})
		if err != nil {
			t.Errorf("approver error: %v", err)
		}
		done <- approved
	}()
	id := parkedApprovalID(t, rt)
	// Wait for the prompt to reach the attached socket, so the scenario reads
	// as "prompt delivered, then the tab closed and reopened".
	waitFor(t, 5*time.Second, func() bool { return broadcastApprovalReceived(ws, id) })

	// Last client detaches and somebody re-attaches before the hold expires.
	//
	// The swap is fused under one clientsMu hold, and the hold is armed with
	// the call detach makes, because deleteApprover re-checks clientCount
	// right after its broadcast and DENIES on the spot when it sees nobody
	// attached (fail-closed for a prompt nobody can answer). A bare
	// detach-then-attach leaves the set empty for an instant, and a loaded
	// runner (Windows CI) lands that check in the gap: the recheck denies the
	// request and the timer this test is about never gets a say. The
	// detach→arm→expiry half is TestApprovalHoldTimerDeniesWithNobodyAttached,
	// the end-to-end reattach flow is TestApprovalHoldAllowsReattachToAnswer.
	rt.clientsMu.Lock()
	delete(rt.clients, ws)
	rt.clients[newFakeWSConn()] = struct{}{}
	rt.clientsMu.Unlock()
	rt.scheduleAutoDenyAfter(rt.approvalHold)

	select {
	case approved := <-done:
		t.Fatalf("the hold timer denied a pending approval despite a re-attached client (approved=%v)", approved)
	case <-time.After(900 * time.Millisecond): // > 2x the hold: the timer has certainly expired
	}

	// The re-attached client's answer still decides it.
	rt.completeApproval(id, true)
	select {
	case approved := <-done:
		if !approved {
			t.Fatal("the re-attached client's approval was not honoured")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approver did not return after the client answered")
	}
}

// TestApprovalHoldTimerDeniesWithNobodyAttached is the other half: with nobody
// attached when the hold expires, the pending approval is denied so the turn
// does not hang.
func TestApprovalHoldTimerDeniesWithNobodyAttached(t *testing.T) {
	rt := newTestRuntime(t)
	rt.approvalHold = 50 * time.Millisecond

	ws := newFakeWSConn()
	rt.clientsMu.Lock()
	rt.clients[ws] = struct{}{}
	rt.clientsMu.Unlock()

	done := make(chan bool, 1)
	go func() {
		approved, err := rt.deleteApprover()(context.Background(), agent.DeleteRequest{Paths: []string{"x"}})
		if err != nil {
			t.Errorf("approver error: %v", err)
		}
		done <- approved
	}()
	id := parkedApprovalID(t, rt)
	// Detach only once the prompt was delivered: the deny asserted below must
	// come from the armed hold timer, not from the approver's own
	// nobody-attached recheck answering a request it caught mid-registration.
	waitFor(t, 5*time.Second, func() bool { return broadcastApprovalReceived(ws, id) })
	rt.detach(ws)

	select {
	case approved := <-done:
		if approved {
			t.Fatal("an unattended approval must be denied, not approved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hold timer never denied the parked approval")
	}
}

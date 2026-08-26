package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
)

// recordingSender captures approval requests sent by the approver closure
// (the stream-goroutine side) so the test can answer them from the
// "Update thread".
type recordingSender struct {
	mu  sync.Mutex
	req []*approvalRequest
}

func (r *recordingSender) Send(m tea.Msg) {
	msg, ok := m.(approvalRequestMsg)
	if !ok {
		return
	}
	r.mu.Lock()
	r.req = append(r.req, msg.ar)
	r.mu.Unlock()
}

func (r *recordingSender) take() *approvalRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.req) == 0 {
		return nil
	}
	ar := r.req[0]
	r.req = r.req[1:]
	return ar
}

func newTestApprovalRequest(paths ...string) *approvalRequest {
	p := make([]string, len(paths))
	copy(p, paths)
	return &approvalRequest{
		req:   agent.DeleteRequest{Paths: p, Reason: "test"},
		reply: make(chan bool, 1),
	}
}

func TestApprovalPerRequestReply(t *testing.T) {
	t.Run("dismiss replies on the request's own channel", func(t *testing.T) {
		m := &Model{}
		ar := newTestApprovalRequest("a.txt")
		m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar})
		if m.modal != ModalApproval {
			t.Fatalf("modal = %v, want ModalApproval", m.modal)
		}
		m.dismissApproval(true)
		select {
		case got := <-ar.reply:
			if !got {
				t.Fatal("reply = false, want true")
			}
		default:
			t.Fatal("no reply delivered")
		}
		if m.approvalUI != nil || m.modal != ModalNone {
			t.Fatal("approval state not cleared")
		}
	})

	t.Run("concurrent requests queue and get independent answers", func(t *testing.T) {
		m := &Model{}
		ar1 := newTestApprovalRequest("1.txt")
		ar2 := newTestApprovalRequest("2.txt")
		m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar1})
		m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar2})
		if len(m.pendingApprovals) != 1 || m.pendingApprovals[0] != ar2 {
			t.Fatalf("queue = %v, want [ar2]", m.pendingApprovals)
		}
		// Answer the first (No); the second is promoted to the screen.
		m.dismissApproval(false)
		if m.approvalUI == nil || m.approvalUI.ar != ar2 {
			t.Fatal("second request not promoted")
		}
		if m.modal != ModalApproval {
			t.Fatal("modal must stay up while the queue is non-empty")
		}
		m.dismissApproval(true)
		if got := <-ar1.reply; got {
			t.Fatal("ar1 reply = true, want false")
		}
		if got := <-ar2.reply; !got {
			t.Fatal("ar2 reply = false, want true")
		}
		if m.approvalUI != nil || len(m.pendingApprovals) != 0 {
			t.Fatal("approval state not drained")
		}
	})

	t.Run("approval takes over and restores the previous modal", func(t *testing.T) {
		m := &Model{modal: ModalSessions}
		ar := newTestApprovalRequest("x.txt")
		m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar})
		if m.modal != ModalApproval {
			t.Fatal("approval must take over the modal")
		}
		m.dismissApproval(false)
		if m.modal != ModalSessions {
			t.Fatalf("modal = %v, want restored ModalSessions", m.modal)
		}
		if m.modalBeforeApproval != ModalNone {
			t.Fatal("modalBeforeApproval must reset after restore")
		}
	})

	t.Run("dismiss with nothing on screen is a no-op", func(t *testing.T) {
		m := &Model{}
		m.dismissApproval(true) // must not panic or change state
		if m.approvalUI != nil || m.modal != ModalNone {
			t.Fatal("no-op dismiss mutated state")
		}
	})
}

// The approver closure runs on the stream goroutine while dismissApproval
// runs on the Update thread. Under -race this pins that no Model state is
// shared between the two — the old approvalInFlight bool + shared
// approvalResult channel raced exactly here.
func TestApprovalApproverConcurrentWithDismiss(t *testing.T) {
	m := &Model{}
	send := &recordingSender{}
	approver := m.makeDeleteApprover("s1", send)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan bool, 1)
	errc := make(chan error, 1)
	go func() {
		ok, err := approver(ctx, agent.DeleteRequest{Paths: []string{"f.txt"}, Reason: "test"})
		if err != nil {
			errc <- err
			return
		}
		result <- ok
	}()

	// Wait for the request to arrive, then answer it from the "Update
	// thread" while the approver is blocked on its reply.
	var ar *approvalRequest
	deadline := time.Now().Add(2 * time.Second)
	for ar == nil && time.Now().Before(deadline) {
		ar = send.take()
		if ar == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if ar == nil {
		t.Fatal("approver did not send its request")
	}
	m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar})
	m.dismissApproval(true)

	select {
	case ok := <-result:
		if !ok {
			t.Fatal("approver got false, want true")
		}
	case err := <-errc:
		t.Fatalf("approver error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("approver did not return after dismiss")
	}
}

// A cancelled turn must unblock the approver even if the modal is never
// answered (the reply is then dropped into the buffered channel).
func TestApprovalApproverUnblocksOnCancel(t *testing.T) {
	m := &Model{}
	send := &recordingSender{}
	approver := m.makeDeleteApprover("s1", send)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	errc := make(chan error, 1)
	go func() {
		ok, err := approver(ctx, agent.DeleteRequest{Paths: []string{"f.txt"}, Reason: "test"})
		if err != nil {
			errc <- err
			return
		}
		result <- ok
	}()

	var ar *approvalRequest
	deadline := time.Now().Add(2 * time.Second)
	for ar == nil && time.Now().Before(deadline) {
		ar = send.take()
		if ar == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if ar == nil {
		t.Fatal("approver did not send its request")
	}
	m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar})
	// Turn cancelled before the user answered.
	cancel()

	select {
	case <-errc:
		// Expected: ctx.Err()
	case ok := <-result:
		t.Fatalf("approver returned approval %v instead of a cancel error", ok)
	case <-time.After(2 * time.Second):
		t.Fatal("approver did not unblock on cancel")
	}
	// The modal is still on screen; the turn-end path dismisses it. The
	// reply must be dropped (buffered) without blocking the UI thread.
	m.dismissApproval(false)
	if m.approvalUI != nil || m.modal != ModalNone {
		t.Fatal("dismiss after cancel must clear the approval state")
	}
}

// A background session's delete request names its session in the modal;
// the focused session's request keeps the classic title.
func TestApprovalModalNamesBackgroundSession(t *testing.T) {
	m := &Model{lives: newLiveSessions(&agent.Agent{})}
	m.lives.Add(&agent.Agent{}, "bg")

	ar := newTestApprovalRequest("f.txt")
	ar.sid = "s2"
	m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar})
	if out := m.renderApprovalModal(); !strings.Contains(out, "bg") {
		t.Fatalf("background requester not named in modal:\n%s", out)
	}

	m.dismissApproval(false)

	ar2 := newTestApprovalRequest("f.txt")
	ar2.sid = "s1" // focused
	m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar2})
	if out := m.renderApprovalModal(); strings.Contains(out, "— ") {
		t.Fatalf("focused requester must not be named:\n%s", out)
	}
}

// A queued approval whose requesting turn has terminated can never be
// answered usefully (the approver left via its reply or ctx.Done); promoting
// it later would show a modal whose answer goes nowhere. Turn end prunes
// such requests while keeping a resubmitted successor's and other
// sessions' requests.
func TestTurnEndPrunesDeadQueuedApprovals(t *testing.T) {
	// Fresh model per subtest: the subtests mutate queue/modal state.
	newM := func(t *testing.T) (*Model, *liveSession) {
		t.Helper()
		vp := NewViewport(80, 20)
		vp.Style = ViewportStyle
		a1 := newSwitchTestAgent(t)
		m := &Model{agent: a1, lives: newLiveSessions(a1), viewport: vp, sessionID: a1.SessionID}
		bg := m.lives.Add(newSwitchTestAgent(t), "bg")
		return m, bg
	}

	t.Run("delivery stamps the requesting turn's generation", func(t *testing.T) {
		m, bg := newM(t)
		bg.turnSeq = 4
		ar := newTestApprovalRequest("stamp.txt")
		ar.sid = bg.id
		m.handleApprovalRequestMsg(approvalRequestMsg{ar: ar})
		if ar.turnSeq != 4 {
			t.Fatalf("turnSeq = %d, want the session's current generation 4", ar.turnSeq)
		}
	})

	t.Run("ending turn's queued request is pruned", func(t *testing.T) {
		m, bg := newM(t)
		ar := newTestApprovalRequest("bg.txt")
		ar.sid = bg.id
		ar.turnSeq = 1 // stamped at delivery in production
		m.pendingApprovals = append(m.pendingApprovals, ar)

		m.handleTurnFinishedMsg(bg.id, 1, nil)

		if len(m.pendingApprovals) != 0 {
			t.Fatalf("queue holds %d requests, want the dead one pruned", len(m.pendingApprovals))
		}
	})

	t.Run("successor turn's queued request survives a stale terminal", func(t *testing.T) {
		m, bg := newM(t)
		stale := newTestApprovalRequest("old.txt")
		stale.sid = bg.id
		stale.turnSeq = 2
		live := newTestApprovalRequest("new.txt")
		live.sid = bg.id
		live.turnSeq = 3 // cancel + resubmit queued this one after stamping
		m.pendingApprovals = append(m.pendingApprovals, stale, live)

		// The OLD turn's late terminal arrives after the resubmit: pruning
		// must run on the superseded path too, but keep the successor's.
		m.handleTurnFinishedMsg(bg.id, 2, nil)

		if len(m.pendingApprovals) != 1 || m.pendingApprovals[0] != live {
			t.Fatalf("queue = %v, want only the successor's request", m.pendingApprovals)
		}
	})

	t.Run("other sessions' queued requests survive", func(t *testing.T) {
		m, bg := newM(t)
		focusedReq := newTestApprovalRequest("focused.txt")
		focusedReq.sid = "s1"
		focusedReq.turnSeq = 7
		m.pendingApprovals = append(m.pendingApprovals, focusedReq)

		m.handleTurnFinishedMsg(bg.id, 3, nil)

		if len(m.pendingApprovals) != 1 || m.pendingApprovals[0] != focusedReq {
			t.Fatalf("queue = %v, want the other session's request untouched", m.pendingApprovals)
		}
	})
}

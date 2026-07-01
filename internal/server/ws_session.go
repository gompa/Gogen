package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"gogen/internal/agent"
)

type wsSession struct {
	ws         *wsConn
	approvals  map[string]chan bool
	approvalMu sync.Mutex
}

func newWSSession(ws *wsConn) *wsSession {
	return &wsSession{
		ws:        ws,
		approvals: make(map[string]chan bool),
	}
}

func newApprovalID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("approval-%d", len(b))
	}
	return hex.EncodeToString(b[:])
}

func (s *wsSession) completeApproval(id string, approved bool) {
	s.approvalMu.Lock()
	ch := s.approvals[id]
	delete(s.approvals, id)
	s.approvalMu.Unlock()
	if ch != nil {
		ch <- approved
	}
}

func (s *wsSession) deleteApprover() agent.DeleteApprover {
	return func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		id := newApprovalID()
		ch := make(chan bool, 1)

		s.approvalMu.Lock()
		s.approvals[id] = ch
		s.approvalMu.Unlock()

		defer func() {
			s.approvalMu.Lock()
			delete(s.approvals, id)
			s.approvalMu.Unlock()
		}()

		if err := s.ws.writeJSON(WSMessage{
			Type:       "delete_approval",
			ApprovalID: id,
			Paths:      req.Paths,
			Reason:     req.Reason,
		}); err != nil {
			return false, err
		}

		select {
		case approved := <-ch:
			return approved, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

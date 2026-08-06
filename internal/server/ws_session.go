package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

// approvalIDFallback disambiguates approval ids when crypto/rand fails
// (monotonic counter + timestamp instead of a constant string, which would
// have collided every approval onto one id).
var approvalIDFallback atomic.Uint64

// newApprovalID returns a random approval id. Delete approvals are keyed by
// (sessionID, approvalID) on the session runtime. The per-connection
// wsSession was removed when approvals moved into
// sessionRuntime.deleteApprover.
func newApprovalID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("approval-%d-%d", time.Now().UnixNano(), approvalIDFallback.Add(1))
	}
	return hex.EncodeToString(b[:])
}

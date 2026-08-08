package server

import (
	"gogen/internal/randhex"
)

// newApprovalID returns a random approval id. Delete approvals are keyed by
// (sessionID, approvalID) on the session runtime. The per-connection
// wsSession was removed when approvals moved into
// sessionRuntime.deleteApprover.
func newApprovalID() string {
	return randhex.ID(8, "")
}

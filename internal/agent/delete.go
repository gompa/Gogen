package agent

import "errors"

var (
	ErrDeleteDenied           = errors.New("delete denied by user")
	ErrDeleteApprovalRequired = errors.New("delete blocked: approval is required")
)

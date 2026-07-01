package agent

import "context"

// DeleteRequest describes a file deletion awaiting user approval.
type DeleteRequest struct {
	Paths  []string
	Reason string
}

// DeleteApprover decides whether a delete operation may proceed.
type DeleteApprover func(ctx context.Context, req DeleteRequest) (approved bool, err error)

type deleteApproverKey struct{}
type deleteApprovalPolicyKey struct{}

// ContextWithDeleteApprover attaches a delete approval handler to ctx.
func ContextWithDeleteApprover(ctx context.Context, approver DeleteApprover) context.Context {
	if approver == nil {
		return ctx
	}
	return context.WithValue(ctx, deleteApproverKey{}, approver)
}

// DeleteApproverFromContext returns the delete approver from ctx, if any.
func DeleteApproverFromContext(ctx context.Context) DeleteApprover {
	if ctx == nil {
		return nil
	}
	approver, _ := ctx.Value(deleteApproverKey{}).(DeleteApprover)
	return approver
}

// ContextWithDeleteApprovalRequired sets whether deletes require approval in this ctx.
func ContextWithDeleteApprovalRequired(ctx context.Context, required bool) context.Context {
	return context.WithValue(ctx, deleteApprovalPolicyKey{}, required)
}

func deleteApprovalRequired(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	required, ok := ctx.Value(deleteApprovalPolicyKey{}).(bool)
	if ok {
		return required
	}
	return true
}

func (e *Executor) deleteApprovalRequired(ctx context.Context) bool {
	if !e.RequireDeleteApproval {
		return false
	}
	return deleteApprovalRequired(ctx)
}

func (e *Executor) requireDeleteApproval(ctx context.Context, paths []string, reason string) error {
	if len(paths) == 0 || !e.deleteApprovalRequired(ctx) {
		return nil
	}
	approver := DeleteApproverFromContext(ctx)
	if approver == nil {
		return ErrDeleteApprovalRequired
	}
	ok, err := approver(ctx, DeleteRequest{Paths: paths, Reason: reason})
	if err != nil {
		return err
	}
	if !ok {
		return ErrDeleteDenied
	}
	return nil
}

package agent

import "context"

// ToolOutputSink receives live chunks of a shell command's combined
// stdout/stderr as the command runs, together with the exact command string
// that was executed.
//
// The sink is invoked from the goroutine that copies the child process's
// pipes, so implementations must be safe for concurrent use and should
// return quickly — the child process will stall while the sink runs.
type ToolOutputSink func(command string, chunk string)

type toolOutputSinkKey struct{}

// ContextWithToolOutput returns a copy of ctx carrying sink. Tools that
// shell out (execute_command) deliver live output chunks to the sink via
// Executor.ExecuteCommand.
func ContextWithToolOutput(ctx context.Context, sink ToolOutputSink) context.Context {
	return context.WithValue(ctx, toolOutputSinkKey{}, sink)
}

// ToolOutputFromContext returns the ToolOutputSink attached to ctx, or nil
// when none was set.
func ToolOutputFromContext(ctx context.Context) ToolOutputSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(toolOutputSinkKey{}).(ToolOutputSink)
	return sink
}

// ToolOutputEnd reports that a shell tool's live output stream has
// terminated, with the command's success status. It is invoked exactly
// once per command: for foreground commands when ExecuteCommand returns,
// and for background jobs (execute_command background=true) when the job
// process exits — which may be long after the turn that started it ended.
type ToolOutputEnd func(success bool)

type toolOutputEndKey struct{}

// ContextWithToolOutputEnd returns a copy of ctx carrying end. Tools that
// shell out (execute_command, foreground or background) invoke it when the
// command's output stream terminates so frontends can close the live
// terminal (term_exit) with the real exit status.
func ContextWithToolOutputEnd(ctx context.Context, end ToolOutputEnd) context.Context {
	return context.WithValue(ctx, toolOutputEndKey{}, end)
}

// ToolOutputEndFromContext returns the ToolOutputEnd attached to ctx, or
// nil when none was set.
func ToolOutputEndFromContext(ctx context.Context) ToolOutputEnd {
	if ctx == nil {
		return nil
	}
	end, _ := ctx.Value(toolOutputEndKey{}).(ToolOutputEnd)
	return end
}

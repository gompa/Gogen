package agent

import "context"

// ToolOutputSink receives live chunks of a shell command's combined
// stdout/stderr as the command runs, together with the exact command string
// that was executed (including any wrapper built by run_tests / run_lint).
//
// The sink is invoked from the goroutine that copies the child process's
// pipes, so implementations must be safe for concurrent use and should
// return quickly — the child process will stall while the sink runs.
type ToolOutputSink func(command string, chunk string)

type toolOutputSinkKey struct{}

// ContextWithToolOutput returns a copy of ctx carrying sink. Tools that
// shell out (execute_command, run_tests, run_lint) deliver live output
// chunks to the sink via Executor.ExecuteCommand.
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

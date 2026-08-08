package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/randhex"
)

// BackgroundJob is one shell command running outside a turn. The turn that
// started it (execute_command background=true) returns immediately with the
// job's id; the command keeps running until it exits on its own, is cancelled
// (background_job_cancel), or its owning session is closed (Agent.Close kills
// every job, so a closed web pane or a TUI quit never orphans a process).
// Output is retained as a bounded tail so a long-running job cannot grow
// memory without bound while it is polled. Finished jobs stay registered
// until the session closes so background_job_status can report their exit
// code and output after completion; Close removes everything.
type BackgroundJob struct {
	ID      string
	Command string
	Start   time.Time

	// cancelled records an explicit CancelBackgroundJob/Close kill. A group
	// kill makes Wait return an *exec.ExitError (signaled), not
	// context.Canceled, so the flag is what distinguishes cancel from failure.
	cancelled atomic.Bool

	done   chan struct{} // closed when the process exits
	cancel context.CancelFunc
	mu     sync.Mutex
	output *boundedOutputWriter
	// exitErr is nil on success, context.Canceled when the job was cancelled
	// (or the session closed), an *exec.ExitError for a non-zero exit, or
	// another error for launch/IO failures.
	exitErr error
}

// newBackgroundJobID returns a random hex job id.
func newBackgroundJobID() string {
	return randhex.ID(8, "job-")
}

// StartBackgroundCommand validates the command against the same command
// guard and sandbox as execute_command, launches it detached from the turn,
// and returns its job id. The job runs with no turn-bound timeout: it is
// cancelled via CancelBackgroundJob or killed when the session closes
// (Agent.Close). Finished jobs stay registered for status polling.
func (a *Agent) StartBackgroundCommand(command string) (string, error) {
	if a.Executor == nil {
		return "", fmt.Errorf("no executor configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := a.Executor.BuildCommand(ctx, command)
	if err != nil {
		cancel()
		return "", err
	}
	job := &BackgroundJob{
		ID:      newBackgroundJobID(),
		Command: command,
		Start:   time.Now(),
		done:    make(chan struct{}),
		cancel:  cancel,
		output:  newBoundedOutputWriter(defaultBackgroundOutputCap),
	}
	cmd.Stdout = job.output
	cmd.Stderr = job.output

	a.bgMu.Lock()
	if a.bgJobs == nil {
		a.bgJobs = make(map[string]*BackgroundJob)
	}
	a.bgJobs[job.ID] = job
	a.bgMu.Unlock()

	if err := cmd.Start(); err != nil {
		a.bgMu.Lock()
		delete(a.bgJobs, job.ID)
		a.bgMu.Unlock()
		cancel()
		return "", fmt.Errorf("execution error: %w", err)
	}
	go func() {
		waitErr := cmd.Wait()
		job.mu.Lock()
		job.exitErr = waitErr
		job.mu.Unlock()
		close(job.done)
	}()
	return job.ID, nil
}

// backgroundJob returns the live job for id, or nil.
func (a *Agent) backgroundJob(jobID string) *BackgroundJob {
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	return a.bgJobs[jobID]
}

// BackgroundJobStatus reports the state of a background job: whether it is
// still running, its exit code when finished, and the tail of its output.
// Finished jobs stay registered until the session closes (Agent.Close), so
// the agent can poll for the result after completion. "Unknown job" only
// happens for ids that never existed or whose session was closed.
func (a *Agent) BackgroundJobStatus(jobID string) (string, error) {
	job := a.backgroundJob(jobID)
	if job == nil {
		return "", fmt.Errorf("unknown background job %q (jobs are scoped to this session; the session may have been closed)", jobID)
	}
	elapsed := time.Since(job.Start).Round(time.Millisecond)

	running := true
	exitCode := -1
	var exitErr error
	select {
	case <-job.done:
		running = false
		job.mu.Lock()
		exitErr = job.exitErr
		job.mu.Unlock()
	default:
	}
	switch {
	case running:
		return fmt.Sprintf("Job %s is RUNNING (%s elapsed).\nCommand: %s\n%s", job.ID, elapsed, job.Command, formatJobOutput(job)), nil
	case job.cancelled.Load():
		return fmt.Sprintf("Job %s was CANCELLED after %s.\nCommand: %s\n%s", job.ID, elapsed, job.Command, formatJobOutput(job)), nil
	case exitErr == nil:
		exitCode = 0
	case errors.Is(exitErr, context.DeadlineExceeded):
		return fmt.Sprintf("Job %s TIMED OUT after %s.\nCommand: %s\n%s", job.ID, elapsed, job.Command, formatJobOutput(job)), nil
	default:
		var ee *exec.ExitError
		if errors.As(exitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return fmt.Sprintf("Job %s FINISHED in %s with exit code %d.\nCommand: %s\n%s", job.ID, elapsed, exitCode, job.Command, formatJobOutput(job)), nil
}

// formatJobOutput returns the job's retained output tail, trimmed and
// labelled, or a note when the job produced none.
func formatJobOutput(job *BackgroundJob) string {
	out := job.output.String()
	if out == "" {
		return "Output: (none so far)"
	}
	const maxShow = 8 * 1024
	if len(out) > maxShow {
		return fmt.Sprintf("Output (last %d bytes of %d):\n%s", maxShow, len(out), out[len(out)-maxShow:])
	}
	return "Output:\n" + out
}

// CancelBackgroundJob cancels a running background job, killing its process
// group (the same group-kill execute_command cancellation uses).
func (a *Agent) CancelBackgroundJob(jobID string) (string, error) {
	job := a.backgroundJob(jobID)
	if job == nil {
		return "", fmt.Errorf("unknown background job %q (it may have already finished)", jobID)
	}
	select {
	case <-job.done:
		return "", fmt.Errorf("background job %s already finished", jobID)
	default:
	}
	job.cancelled.Store(true)
	job.cancel()
	return fmt.Sprintf("Cancelled background job %s (command: %s).", jobID, job.Command), nil
}

// Close kills every background job of this session. Called when a session is
// closed (web pane close / eviction / shutdown) and at process exit, so no
// command is ever orphaned by a session that no longer exists. Idempotent.
func (a *Agent) Close() {
	a.bgMu.Lock()
	jobs := a.bgJobs
	a.bgJobs = nil
	a.bgMu.Unlock()
	for _, job := range jobs {
		job.cancelled.Store(true)
		job.cancel()
	}
}

const defaultBackgroundOutputCap = 256 * 1024

// boundedOutputWriter accumulates a command's combined stdout+stderr but
// keeps only the last max bytes, so a long-running background job cannot
// grow memory without bound. Write may be called concurrently by exec's
// pipe-copy goroutines, so the buffer is mutex-guarded.
type boundedOutputWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func newBoundedOutputWriter(max int) *boundedOutputWriter {
	return &boundedOutputWriter{max: max}
}

func (w *boundedOutputWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if overflow := w.buf.Len() - w.max; overflow > 0 {
		w.buf.Next(overflow)
	}
	return len(p), nil
}

func (w *boundedOutputWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/randhex"
)

// BackgroundJob is one shell command running outside a turn. The turn that
// started it (execute_command background=true) returns immediately with the
// job's id; the command keeps running until it exits on its own, is cancelled
// (background_job action=cancel), or its owning session is closed (Agent.Close kills
// every job, so a closed web pane or a TUI quit never orphans a process).
// Output is retained as a bounded tail so a long-running job cannot grow
// memory without bound while it is polled. Finished jobs stay registered
// for a retention window (defaultBackgroundJobRetain) so background_job
// action=status can report their exit code and output after completion, then
// the reaper removes them; a finished-job cap
// (defaultMaxFinishedBackgroundJobs) bounds how many can accumulate within
// that window. Close removes everything.
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
	// unread holds output produced since the last action=input drain, so
	// input can return the delta since the previous read instead of the
	// full tail. Fed by the same chunks as output (both buffers are written
	// under their own locks); drained by BackgroundJobInput. action=status
	// keeps showing the full tail.
	unread *boundedOutputWriter
	// stdin is the job's stdin pipe, created by cmd.StdinPipe before the
	// process starts. stdinMu serializes writes so logical lines stay
	// atomic; it also guards the pipe against being written while the wait
	// goroutine's cmd.Wait is closing it.
	stdin   io.WriteCloser
	stdinMu sync.Mutex
	// finishedAt is when the process exited; set under mu before done is
	// closed. The finished-job cap uses it to reap the oldest finished jobs
	// first.
	finishedAt time.Time
	// reaper is the retention timer that removes this finished job from the
	// registry after the retention window. Stopped by Close and by cap
	// reaping so a reaped job cannot keep the Agent alive. Guarded by mu.
	reaper *time.Timer
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
// (Agent.Close). Finished jobs stay registered for a bounded retention
// window, then the reaper removes them.
func (a *Agent) StartBackgroundCommand(command string) (string, error) {
	if a.Executor == nil {
		return "", fmt.Errorf("no executor configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &BackgroundJob{
		ID:      newBackgroundJobID(),
		Command: command,
		Start:   time.Now(),
		done:    make(chan struct{}),
		cancel:  cancel,
		output:  newBoundedOutputWriter(defaultBackgroundOutputCap),
		unread:  newBoundedOutputWriter(defaultBackgroundUnreadCap),
	}
	a.bgMu.Lock()
	if a.bgJobs == nil {
		a.bgJobs = make(map[string]*BackgroundJob)
	}
	a.bgJobs[job.ID] = job
	a.bgMu.Unlock()

	// Launch through the platform runner: sh -c with an OS stdin pipe on
	// Unix (buffered, EPIPE semantics), the embedded POSIX interpreter with
	// an in-memory stdin pipe on Windows (no external sh required). Every
	// output chunk lands in both the tail and the unread delta buffer; the
	// MultiWriter preserves per-stream write order, and stdout/stderr
	// interleaving follows the child's write order.
	stdin, wait, err := a.Executor.launchBackground(ctx, command,
		io.MultiWriter(job.output, job.unread), io.MultiWriter(job.output, job.unread))
	if err != nil {
		a.bgMu.Lock()
		delete(a.bgJobs, job.ID)
		a.bgMu.Unlock()
		cancel()
		return "", err
	}
	job.stdin = stdin
	go func() {
		waitErr := wait()
		job.mu.Lock()
		job.exitErr = waitErr
		job.finishedAt = time.Now()
		job.mu.Unlock()
		close(job.done)
		a.onJobFinished(job)
	}()
	return job.ID, nil
}

// maxBackgroundInputBytes caps a single action=input write. The kernel pipe
// buffer absorbs roughly 64 KB before a writer blocks, so a cap this small
// bounds how long a write can wait behind a job that never reads stdin: the
// write still blocks once the buffer fills (until the job reads or exits —
// documented in the tool description), but never with more than a few
// outstanding writes.
const maxBackgroundInputBytes = 16 * 1024

// defaultBackgroundUnreadCap bounds the per-job output delta retained for
// action=input, mirroring the output tail cap so a long-running job cannot
// grow memory without bound.
const defaultBackgroundUnreadCap = 64 * 1024

// BackgroundJobInput writes text to a running background job's stdin and
// returns the output produced since the previous read (the delta), so REPL
// loops can poll cheaply without re-reading the whole tail. The write is
// serialized per job. Input is not a shell command: it goes to the process's
// stdin, so the command guard deliberately does not apply.
func (a *Agent) BackgroundJobInput(jobID, input string, appendNewline bool) (string, error) {
	job := a.backgroundJob(jobID)
	if job == nil {
		return "", fmt.Errorf("unknown background job %q (jobs are scoped to this session; the session may have been closed)", jobID)
	}
	if len(input) > maxBackgroundInputBytes {
		return "", fmt.Errorf("input is %d bytes; background_job input accepts at most %d bytes per call", len(input), maxBackgroundInputBytes)
	}
	select {
	case <-job.done:
		return "", fmt.Errorf("background job %s already finished", jobID)
	default:
	}
	if job.stdin == nil {
		return "", fmt.Errorf("background job %s has no stdin pipe", jobID)
	}

	payload := input
	if appendNewline {
		payload += "\n"
	}
	job.stdinMu.Lock()
	defer job.stdinMu.Unlock()
	n, err := job.stdin.Write([]byte(payload))
	if err == nil && n < len(payload) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if isStdinClosedErr(err) {
			return "", fmt.Errorf("background job %s closed its stdin (the process exited)", jobID)
		}
		return "", fmt.Errorf("writing to background job %s stdin: %w", jobID, err)
	}

	delta := job.unread.drain()
	if delta == "" {
		return fmt.Sprintf("Sent %d bytes to job %s stdin. No new output since the last read.", len(payload), jobID), nil
	}
	return fmt.Sprintf("Sent %d bytes to job %s stdin.\nOutput:\n%s", len(payload), jobID, delta), nil
}

// backgroundJob returns the live job for id, or nil.
func (a *Agent) backgroundJob(jobID string) *BackgroundJob {
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	return a.bgJobs[jobID]
}

// BackgroundJobStatus reports the state of a background job: whether it is
// still running, its exit code when finished, and the tail of its output.
// Finished jobs stay registered for a bounded retention window, so the agent
// can poll for the result after completion. "Unknown job" only happens for
// ids that never existed, were already reaped, or whose session was closed.
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
		return "", fmt.Errorf("unknown background job %q (it may have finished and been reaped)", jobID)
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
// Retention timers are stopped so a reaped job cannot keep the (closing)
// Agent alive; the wait goroutine of a cancelled job finds an empty registry
// afterwards and does not arm a new timer.
func (a *Agent) Close() {
	a.bgMu.Lock()
	jobs := a.bgJobs
	a.bgJobs = nil
	for _, job := range jobs {
		job.cancelled.Store(true)
		job.cancel()
		job.mu.Lock()
		if job.reaper != nil {
			job.reaper.Stop()
		}
		job.mu.Unlock()
	}
	a.bgMu.Unlock()
}

const defaultBackgroundOutputCap = 256 * 1024

// defaultBackgroundJobRetain is how long a finished background job stays
// registered after it exits, so background_job_status can still report its
// exit code and output. After the window the reaper removes it, bounding
// memory on long-lived sessions (each job keeps an outputBuffer tail). The
// Agent.bgRetain field overrides it (0 = this default).
const defaultBackgroundJobRetain = 5 * time.Minute

// defaultMaxFinishedBackgroundJobs caps how many finished jobs stay
// registered at once: when a job finishes and the cap is exceeded, the
// oldest finished jobs are reaped immediately, so a burst of short-lived
// jobs cannot pile up during the retention window. The Agent.bgMaxFinished
// field overrides it (0 = this default).
const defaultMaxFinishedBackgroundJobs = 32

// maxJobNoticeCommandLen caps the command echoed in a job-completion
// notice: long commands (and any secrets embedded in them) must not be
// dumped into the transcript.
const maxJobNoticeCommandLen = 120

// jobExitSummary renders the one-line job-completion notice: id, truncated
// command, and exit code (-1 for unusual failures such as I/O errors).
// Called from the job's wait goroutine after exitErr is final.
func jobExitSummary(job *BackgroundJob) string {
	code := 0
	if job.exitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(job.exitErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}
	cmd := job.Command
	if len(cmd) > maxJobNoticeCommandLen {
		// Truncate on a rune boundary: the byte-slice version could split
		// a multi-byte character and emit invalid UTF-8 into the transcript.
		cmd = string([]rune(cmd)[:maxJobNoticeCommandLen]) + "…"
	}
	return fmt.Sprintf("[job] %s (%s) exited with code %d", job.ID, cmd, code)
}

// onJobFinished runs in the job's wait goroutine once the process has
// exited: it enforces the finished-job cap (reaping the oldest finished jobs
// when the cap is exceeded), arms the retention timer that reaps THIS job
// once its result has been pollable long enough, and — when the job finished
// naturally (not cancelled) and a notice hook is installed — fires the
// completion notice so the session is told without polling.
func (a *Agent) onJobFinished(job *BackgroundJob) {
	a.enforceFinishedJobCap(job)
	a.armJobReaper(job)
	if !job.cancelled.Load() {
		a.fireJobNotice(jobExitSummary(job))
	}
}

// enforceFinishedJobCap reaps the oldest finished jobs when more than
// bgMaxFinished jobs have finished, so the registry stays bounded even when
// many short-lived jobs finish inside the retention window. The job that
// just finished (keep) is excluded — its caller is the one most likely to
// poll it next. Caller is the job's wait goroutine.
func (a *Agent) enforceFinishedJobCap(keep *BackgroundJob) {
	maxFinished := a.bgMaxFinished
	if maxFinished <= 0 {
		maxFinished = defaultMaxFinishedBackgroundJobs
	}
	type finishedJob struct {
		job *BackgroundJob
		at  time.Time
	}
	var finished []finishedJob
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	for _, j := range a.bgJobs {
		if j == keep {
			continue
		}
		select {
		case <-j.done:
			j.mu.Lock()
			at := j.finishedAt
			j.mu.Unlock()
			finished = append(finished, finishedJob{job: j, at: at})
		default:
		}
	}
	// keep just finished, so the total finished count is len(finished)+1.
	overflow := len(finished) + 1 - maxFinished
	if overflow <= 0 {
		return
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].at.Before(finished[j].at) })
	for i := 0; i < overflow && i < len(finished); i++ {
		j := finished[i]
		delete(a.bgJobs, j.job.ID)
		j.job.mu.Lock()
		if j.job.reaper != nil {
			j.job.reaper.Stop()
		}
		j.job.mu.Unlock()
	}
}

// armJobReaper arms the retention timer that removes job from the registry
// after the retention window. It is a no-op when the job is no longer
// registered (session closed — Close cleared the registry — or already
// reaped), so a reaper can never hold the Agent alive after Close.
// Lock order: bgMu is held while setting reaper under job.mu, matching
// Close and enforceFinishedJobCap (bgMu -> job.mu, never the reverse).
func (a *Agent) armJobReaper(job *BackgroundJob) {
	retain := a.bgRetain
	if retain <= 0 {
		retain = defaultBackgroundJobRetain
	}
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	if a.bgJobs[job.ID] != job || job.reaper != nil {
		return
	}
	job.reaper = time.AfterFunc(retain, func() {
		a.reapBackgroundJob(job)
	})
}

// reapBackgroundJob removes a finished job from the registry once its
// retention window has elapsed. A job that was already reaped (cap
// enforcement or Close) or that is still running is a no-op.
func (a *Agent) reapBackgroundJob(job *BackgroundJob) {
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	if a.bgJobs[job.ID] == job {
		delete(a.bgJobs, job.ID)
	}
}

// boundedOutputWriter accumulates a command's combined stdout+stderr but
// keeps only the last max bytes, so a long-running background job cannot
// grow memory without bound. Write may be called concurrently by exec's
// pipe-copy goroutines, so the buffer is mutex-guarded.
type boundedOutputWriter struct {
	outputBuffer
	max int
}

func newBoundedOutputWriter(max int) *boundedOutputWriter {
	return &boundedOutputWriter{max: max}
}

func (w *boundedOutputWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.append(p, w.max, nil)
	return len(p), nil
}

func (w *boundedOutputWriter) String() string {
	return w.string()
}

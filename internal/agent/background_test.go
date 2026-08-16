package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestBackgroundCommandLifecycle covers the happy path: start, poll while
// running, poll after finish (exit code + output), and reap.
func TestBackgroundCommandLifecycle(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("echo hello-background")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if id == "" {
		t.Fatal("expected a job id")
	}

	// Poll until the job finishes (it is a fast echo).
	deadline := time.Now().Add(10 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		status, err = a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if strings.Contains(status, "FINISHED") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(status, "FINISHED") {
		t.Fatalf("job did not finish in time; last status: %s", status)
	}
	if !strings.Contains(status, "exit code 0") {
		t.Fatalf("expected exit code 0, got: %s", status)
	}
	if !strings.Contains(status, "hello-background") {
		t.Fatalf("expected output in status, got: %s", status)
	}
	// The default retention window (5m) keeps the finished job registered,
	// so the status is FINISHED. A job whose window already elapsed (a very
	// slow runner, or a test that shortened bgRetain) reports "unknown"
	// instead — both are correct reaper outcomes.
	if s, err := a.BackgroundJobStatus(id); err != nil && !strings.Contains(err.Error(), "unknown background job") {
		t.Fatalf("unexpected error after finish: %v", err)
	} else if err == nil && !strings.Contains(s, "FINISHED") {
		t.Fatalf("unexpected status after finish: %s", s)
	}
}

// TestBackgroundCommandGuardApplies verifies background commands go through
// the same command guard as foreground commands.
func TestBackgroundCommandGuardApplies(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	// Default guard is blocklist mode; rm -rf / is blocked.
	a := NewAgent(nil, exec, nil)
	defer a.Close()
	if _, err := a.StartBackgroundCommand("rm -rf /"); err == nil {
		t.Fatal("expected blocklisted background command to be rejected")
	}
	if len(a.bgJobs) != 0 {
		t.Fatal("no job should be registered for a rejected command")
	}
}

// TestBackgroundJobCancel verifies CancelBackgroundJob (background_job
// action=cancel) kills a running job
// and the status reports it as cancelled.
func TestBackgroundJobCancel(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("sleep 60")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := a.CancelBackgroundJob(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		status, err = a.BackgroundJobStatus(id)
		if err != nil && strings.Contains(err.Error(), "unknown background job") {
			// Reaped (retention/cap) after cancel — also acceptable.
			return
		}
		if err != nil {
			t.Fatalf("status after cancel: %v", err)
		}
		if strings.Contains(status, "CANCELLED") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job was not reported cancelled; last status: %s", status)
}

// TestBackgroundJobExitCode verifies a failing command reports its non-zero
// exit code.
func TestBackgroundJobExitCode(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("exit 7")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		status, err = a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if strings.Contains(status, "FINISHED") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(status, "exit code 7") {
		t.Fatalf("expected exit code 7, got: %s", status)
	}
}

// TestCloseKillsBackgroundJobs verifies Agent.Close kills every running job
// (the session-close / shutdown path must never orphan a process).
func TestCloseKillsBackgroundJobs(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)

	marker := filepath.Join(dir, "marker")
	id, err := a.StartBackgroundCommand("sleep 30; echo done > " + marker)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	job := a.bgJobs[id]
	if job == nil {
		t.Fatal("started job not registered")
	}
	a.Close()
	// The job must be killed promptly: its wait goroutine closes done as
	// soon as the killed process exits (Close cancels the command context,
	// which SIGKILLs the whole process group).
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("background job survived Agent.Close")
	}
	// The kill must have cut the sleep short: the marker is never written.
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background job survived Agent.Close and completed its work")
	}
}

// TestBackgroundJobReapedAfterRetention verifies the reaper: a finished job
// stays registered for the retention window (status FINISHED, so the model
// can still poll exit code + output), then is removed (status "unknown"), so
// a long-lived session cannot accumulate finished jobs and their output
// tails without bound.
func TestBackgroundJobReapedAfterRetention(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()
	a.bgRetain = 50 * time.Millisecond

	id, err := a.StartBackgroundCommand("echo reaped-soon")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	// The job must first be reported FINISHED (retention window active)...
	for time.Now().Before(deadline) {
		s, err := a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status before reaping: %v", err)
		}
		if strings.Contains(s, "FINISHED") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// ...then reaped after the window: status becomes "unknown".
	for time.Now().Before(deadline) {
		_, err := a.BackgroundJobStatus(id)
		if err != nil && strings.Contains(err.Error(), "unknown background job") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job was not reaped within the retention window")
}

// TestFinishedJobCapReapsOldest verifies the finished-job cap: when a job
// finishes over the cap, the OLDEST finished jobs are reaped immediately
// (their status becomes "unknown"), so a burst of short-lived jobs cannot
// pile up during the retention window. Finish order is taken from each
// job's finishedAt — the same timestamp the cap sorts by — instead of
// assuming the sleep durations serialize reliably (wall-clock order drifted
// on macOS CI, and on Windows the sleeps are effectively instantaneous, so
// processes exit within milliseconds of each other and goroutine wake-up
// order diverges from exit order). The cap must reap the first two
// finishers and keep the last three registered.
func TestFinishedJobCapReapsOldest(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()
	a.bgMaxFinished = 3

	sleeps := []string{"0.05", "0.04", "0.03", "0.02", "0.01"}
	jobs := make([]*BackgroundJob, 0, len(sleeps))
	for i, s := range sleeps {
		id, err := a.StartBackgroundCommand("sleep " + s)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		// Read the job under bgMu: the cap can reap finished jobs (delete
		// from bgJobs) as soon as the first sleep completes, racing a bare
		// map read from the test goroutine.
		a.bgMu.Lock()
		job := a.bgJobs[id]
		a.bgMu.Unlock()
		if job == nil {
			t.Fatalf("started job %s not registered", id)
		}
		jobs = append(jobs, job)
	}

	// Wait until every process has exited (done closes in the wait
	// goroutine after cmd.Wait returns).
	allDone := make(chan struct{}, len(jobs))
	for _, job := range jobs {
		job := job
		go func() {
			<-job.done
			allDone <- struct{}{}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < len(jobs); i++ {
		select {
		case <-allDone:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("only %d of %d jobs finished within the deadline", i, len(jobs))
		}
	}

	// Build the finish order from finishedAt — set under job.mu immediately
	// before done closes, and the exact key enforceFinishedJobCap sorts by —
	// so the assertion matches the cap's ordering even when processes exit
	// within milliseconds of each other (Windows: sleeps are effectively
	// instantaneous, so exit order follows spawn order, not sleep duration).
	type finisher struct {
		id string
		at time.Time
	}
	finished := make([]finisher, 0, len(jobs))
	for _, job := range jobs {
		job.mu.Lock()
		at := job.finishedAt
		job.mu.Unlock()
		finished = append(finished, finisher{id: job.ID, at: at})
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].at.Before(finished[j].at) })

	// The cap reaps as jobs finish: the first finisher is reaped when the
	// fourth finisher's enforcement runs, and the second finisher when the
	// fifth's does. Wait until BOTH reaps have been processed before
	// asserting the survivors.
	settled := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err0 := a.BackgroundJobStatus(finished[0].id)
		_, err1 := a.BackgroundJobStatus(finished[1].id)
		if err0 != nil && err1 != nil && strings.Contains(err0.Error(), "unknown background job") && strings.Contains(err1.Error(), "unknown background job") {
			settled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !settled {
		t.Fatal("cap reaping did not settle within 5s")
	}
	for i, f := range finished {
		id := f.id
		s, err := a.BackgroundJobStatus(id)
		if i < 2 {
			// The first two finishers exceed the cap: reaped immediately.
			if err == nil || !strings.Contains(err.Error(), "unknown background job") {
				t.Fatalf("job %s (finisher %d) should have been reaped; status=%q err=%v", id, i+1, s, err)
			}
		} else {
			// The last three finishers are within the cap: still registered.
			if err != nil || !strings.Contains(s, "FINISHED") {
				t.Fatalf("job %s (finisher %d) should still be registered as finished; status=%q err=%v", id, i+1, s, err)
			}
		}
	}
}

// TestBackgroundJobInputEcho verifies the stdin pipe end-to-end: input
// written to a running job reaches the process, and the process's response
// appears in the job output.
func TestBackgroundJobInputEcho(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	// No nested sh -c: the executor wraps the command in its own shell on
	// Unix and runs it through the embedded interpreter on Windows.
	id, err := a.StartBackgroundCommand("while read line; do echo \"got: $line\"; done")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, err := a.BackgroundJobInput(id, "hello", true)
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	if !strings.Contains(out, "Sent 6 bytes to job "+id) {
		t.Fatalf("input ack = %q", out)
	}

	// The echoed line must appear in the job output (poll the tail; status
	// never drains).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, err := a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if strings.Contains(s, "got: hello") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("echoed line never appeared in job output")
}

// TestBackgroundJobInputAppendNewline verifies append_newline=false: a line
// reader must NOT complete on the payload (no trailing newline), and the
// next newline-terminated input completes the same read with both payloads
// concatenated.
func TestBackgroundJobInputAppendNewline(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("read -r line; echo \"got:$line\"")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := a.BackgroundJobInput(id, "Z", false); err != nil {
		t.Fatalf("input: %v", err)
	}
	// Without a trailing newline the read must stay blocked: the job is
	// still running and has produced no output.
	deadline := time.Now().Add(10 * time.Second)
	time.Sleep(300 * time.Millisecond)
	s, err := a.BackgroundJobStatus(id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// "got:Z" would only appear in the OUTPUT section; the command string
	// contains "got:$line", which must not trigger the assertion.
	if strings.Contains(s, "FINISHED") || strings.Contains(s, "got:Z") {
		t.Fatalf("append_newline=false terminated the read: %q", s)
	}
	// The next input carries the newline: the same read completes with both
	// payloads ("Z" + "Y\n" → "got:ZY").
	if _, err := a.BackgroundJobInput(id, "Y", true); err != nil {
		t.Fatalf("input 2: %v", err)
	}
	for time.Now().Before(deadline) {
		s, err := a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if strings.Contains(s, "got:ZY") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected read to complete as got:ZY; last status: %s", s)
}

// TestBackgroundJobInputDelta verifies the unread-buffer semantics: the
// first input returns everything produced so far; later inputs return only
// output produced since the last drain.
func TestBackgroundJobInputDelta(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("echo marker; sleep 30")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Wait until the marker is in the output tail (status shows the tail
	// and does not drain, so the marker stays unread for the first input).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, err := a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		// The command string also contains "marker"; only the Output
		// section proves the echo actually ran.
		if strings.Contains(s, "Output:\nmarker") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	delta1, err := a.BackgroundJobInput(id, "x", false)
	if err != nil {
		t.Fatalf("input 1: %v", err)
	}
	if !strings.Contains(delta1, "marker") {
		t.Fatalf("first delta = %q, want the marker (output since job start)", delta1)
	}

	delta2, err := a.BackgroundJobInput(id, "y", false)
	if err != nil {
		t.Fatalf("input 2: %v", err)
	}
	if strings.Contains(delta2, "marker") {
		t.Fatalf("second delta = %q, marker reappeared (drain failed)", delta2)
	}
}

// TestBackgroundJobInputFinished verifies input to a finished job fails
// with a clear error.
func TestBackgroundJobInputFinished(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("echo done")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, err := a.BackgroundJobStatus(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if strings.Contains(s, "FINISHED") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := a.BackgroundJobInput(id, "x", false); err == nil || !strings.Contains(err.Error(), "already finished") {
		t.Fatalf("expected already-finished error, got %v", err)
	}
}

// TestBackgroundJobInputOversized verifies the per-write size cap.
func TestBackgroundJobInputOversized(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("sleep 5")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := a.BackgroundJobInput(id, strings.Repeat("x", maxBackgroundInputBytes+1), false); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
}

// TestBackgroundJobInputStdinClosed verifies the EPIPE translation: writing
// to a job whose process closed its stdin reports "closed its stdin" (the
// process may still be running, so the done-channel check cannot catch it).
func TestBackgroundJobInputStdinClosed(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	if runtime.GOOS == "windows" {
		// The embedded interpreter feeds commands an in-memory pipe, not an
		// OS pipe: there is no fd the child can close, so the EPIPE
		// translation cannot be observed (the scenario is Unix-only).
		t.Skip("stdin-close EPIPE is a Unix OS-pipe scenario")
	}
	if exec.SandboxMode() != "off" {
		// A sandbox wrapper (bwrap) is the direct child and holds its own
		// copy of the stdin pipe's read end, so the child closing fd 0 can
		// never surface as EPIPE — the scenario is unobservable here.
		t.Skip("stdin-close EPIPE is not observable through a sandbox wrapper")
	}

	// The command runs under BuildCommand's own `sh -c` wrapper, so no
	// nested shell here: `exec 0<&-` closes the DIRECT child's stdin — the
	// pipe's read end. (A nested `sh -c '...'` would fork an inner shell
	// and the outer shell's copy of the read end would stay open, so
	// writes would never EPIPE and the test would spin until `sleep`
	// exits.) The shell then keeps running (sleep), so the job is alive
	// while its stdin is gone.
	id, err := a.StartBackgroundCommand("exec 0<&-; sleep 3")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, err := a.BackgroundJobInput(id, "x", false)
		if err != nil && strings.Contains(err.Error(), "closed its stdin") {
			return
		}
		if err != nil && !strings.Contains(err.Error(), "closed its stdin") {
			t.Fatalf("unexpected input error: %v", err)
		}
		// The first write may succeed before the shell closes fd 0; retry
		// until the pipe is provably closed.
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stdin close was never observed")
}

// TestBackgroundJobInputUnknown verifies the unknown-job error.
func TestBackgroundJobInputUnknown(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	if _, err := a.BackgroundJobInput("job-nope", "x", false); err == nil || !strings.Contains(err.Error(), "unknown background job") {
		t.Fatalf("expected unknown-job error, got %v", err)
	}
}

// TestFormatJobOutputTailRuneSafe pins the status tail cut to a rune
// boundary: with output longer than maxShow, the raw byte cut
// (out[len(out)-maxShow:]) can land inside a multi-byte UTF-8 rune and start
// the shown tail with an invalid partial character. The cut must back off to
// the next rune boundary, and the reported byte count must be the
// actually-shown length (a few bytes smaller than maxShow).
func TestFormatJobOutputTailRuneSafe(t *testing.T) {
	// maxShowBytes = 8192; "界" + 8190 ASCII bytes = 8193 bytes, so the raw
	// cut at byte 1 splits the leading 界 (bytes 0-2). The rune-safe cut
	// starts at byte 3: the tail is the 8190 ASCII bytes, valid UTF-8.
	out := "界" + strings.Repeat("a", maxShowBytes-2)
	job := &BackgroundJob{output: newBoundedOutputWriter(defaultBackgroundOutputCap)}
	if _, err := job.output.Write([]byte(out)); err != nil {
		t.Fatal(err)
	}
	got := formatJobOutput(job, false)
	if !utf8.ValidString(got) {
		t.Fatalf("status output is not valid UTF-8: %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("Output (last %d bytes of %d)", len(out)-3, len(out))) {
		t.Fatalf("expected rune-safe tail length in status, got: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("a", maxShowBytes-2)) {
		t.Fatalf("tail must start on a rune boundary (leading 界 dropped whole), got: %q", got)
	}
}

// TestBackgroundJobToolInput verifies the background_job handler routes the
// input action (schema enum + arg parsing), including the missing-input
// error.
func TestBackgroundJobToolInput(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	id, err := a.StartBackgroundCommand("sleep 5")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out, err := handleBackgroundJob(context.Background(), a, map[string]interface{}{
		"action":         "input",
		"job_id":         id,
		"input":          "hi",
		"append_newline": false,
	})
	if err != nil {
		t.Fatalf("input via handler: %v", err)
	}
	if !strings.Contains(out, "Sent 2 bytes to job "+id) {
		t.Fatalf("handler output = %q", out)
	}

	if _, err := handleBackgroundJob(context.Background(), a, map[string]interface{}{
		"action": "input",
		"job_id": id,
	}); err == nil || !strings.Contains(err.Error(), "missing required argument") {
		t.Fatalf("expected missing-input error, got %v", err)
	}
}

package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
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

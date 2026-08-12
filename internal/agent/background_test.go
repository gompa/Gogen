package agent

import (
	"os"
	"path/filepath"
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

	id, err := a.StartBackgroundCommand("sleep 30; echo done > " + filepath.Join(dir, "marker"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	a.Close()
	// The job must be cancelled (not allowed to complete its sleep and write
	// the marker).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "marker")); err == nil {
			t.Fatal("background job survived Agent.Close and completed its work")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The job should now be reaped or reported cancelled.
	status, err := a.BackgroundJobStatus(id)
	if err == nil && !strings.Contains(status, "CANCELLED") {
		t.Fatalf("unexpected status after Close: %s", status)
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
// pile up during the retention window. Finish order is forced with distinct
// sleep durations: the longest sleep finishes last and is the most recent
// finisher, so it must survive.
func TestFinishedJobCapReapsOldest(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()
	a.bgMaxFinished = 3

	// Finish order: job 5 (sleep 0.01) first, ..., job 1 (sleep 0.05) last.
	// Cap enforcement when job 2 finishes reaps job 5; when job 1 finishes
	// it reaps job 4 — jobs 1-3 stay registered.
	sleeps := []string{"0.05", "0.04", "0.03", "0.02", "0.01"}
	ids := make([]string, len(sleeps))
	for i, s := range sleeps {
		id, err := a.StartBackgroundCommand("sleep " + s)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		ids[i] = id
	}

	// Wait until every job has settled (finished or reaped).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		settled := true
		for _, id := range ids {
			if s, err := a.BackgroundJobStatus(id); err == nil && strings.Contains(s, "RUNNING") {
				settled = false
			}
		}
		if settled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i, id := range ids {
		s, err := a.BackgroundJobStatus(id)
		if i < 3 {
			// Jobs 1-3 are the most recent finishers: still registered.
			if err != nil || !strings.Contains(s, "FINISHED") {
				t.Fatalf("job %d should still be registered as finished; status=%q err=%v", i+1, s, err)
			}
		} else {
			// Jobs 4-5 finished first: reaped by the cap.
			if err == nil || !strings.Contains(err.Error(), "unknown background job") {
				t.Fatalf("job %d should have been reaped; status=%q err=%v", i+1, s, err)
			}
		}
	}
}

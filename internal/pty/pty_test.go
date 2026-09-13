//go:build unix

package pty

import (
	"strings"
	"testing"
	"time"
)

// TestSendAndRead covers the open/send/read lifecycle: a send returns the
// output it produced and a later read returns only newer output.
func TestSendAndRead(t *testing.T) {
	s, err := Start(Options{Shell: "cat", Args: nil, Env: []string{"TERM=dumb"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close()

	res, err := s.Send("hello", true, 3*time.Second)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Fatalf("send output = %q, want it to contain %q", res.Output, "hello")
	}
	if res.Exited {
		t.Fatalf("session exited unexpectedly: %+v", res)
	}

	// The send consumed the output: a read with nothing new is empty.
	if out, _ := s.Read(); strings.Contains(out, "hello") {
		t.Fatalf("read after send = %q, want no duplicate of the send output", out)
	}

	res2, err := s.Send("world", true, 3*time.Second)
	if err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !strings.Contains(res2.Output, "world") {
		t.Fatalf("send 2 output = %q, want it to contain %q", res2.Output, "world")
	}
}

// TestIncrementalReads verifies that Read returns output as it arrives: a
// script printing with delays yields distinct increments on successive reads.
func TestIncrementalReads(t *testing.T) {
	s, err := Start(Options{
		Shell: "/bin/sh",
		Args:  []string{"-c", "printf a; sleep 0.4; printf b; sleep 0.4; printf c; sleep 30"},
		Env:   []string{"TERM=dumb"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close()

	readUntil := func(want string, within time.Duration) string {
		deadline := time.Now().Add(within)
		var acc string
		for time.Now().Before(deadline) {
			out, _ := s.Read()
			acc += out
			if strings.Contains(acc, want) {
				return acc
			}
			time.Sleep(20 * time.Millisecond)
		}
		return acc
	}

	if got := readUntil("a", 2*time.Second); !strings.Contains(got, "a") {
		t.Fatalf("first increment = %q, want a", got)
	}
	if got := readUntil("b", 2*time.Second); !strings.Contains(got, "b") {
		t.Fatalf("second increment = %q, want b", got)
	}
	if got := readUntil("c", 2*time.Second); !strings.Contains(got, "c") {
		t.Fatalf("third increment = %q, want c", got)
	}
}

// TestCloseKillsProcess verifies Close reaps the process group so no orphan
// survives, and that Done closes promptly.
func TestCloseKillsProcess(t *testing.T) {
	s, err := Start(Options{Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Env: []string{"TERM=dumb"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := s.PID()
	if pid <= 0 {
		t.Fatal("expected a positive pid")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close after Close")
	}
	if s.Status().Running {
		t.Fatal("status still running after Close")
	}
	// Close is idempotent.
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestNaturalExit verifies the exit code is retained when the process exits on
// its own.
func TestNaturalExit(t *testing.T) {
	s, err := Start(Options{Shell: "/bin/sh", Args: []string{"-c", "exit 7"}, Env: []string{"TERM=dumb"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close()
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit")
	}
	if got := s.ExitCode(); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}
	if _, err := s.Send("x", true, time.Second); err != ErrExited {
		t.Fatalf("send after exit err = %v, want ErrExited", err)
	}
}

// TestSignal verifies a signal reaches the child's process group and ends the
// process.
func TestSignal(t *testing.T) {
	s, err := Start(Options{Shell: "/bin/sh", Args: []string{"-c", "sleep 30"}, Env: []string{"TERM=dumb"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close()
	if err := s.Signal("SIGTERM"); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
	if err := s.Signal("SIGBOGUS"); err == nil {
		t.Fatal("expected an error for an unknown signal")
	}
}

// TestTruncation verifies the scrollback bound keeps memory finite while the
// read cursor still reports how far behind the reader is.
func TestTruncation(t *testing.T) {
	s, err := Start(Options{
		Shell:     "/bin/sh",
		Args:      []string{"-c", "yes 0123456789 | head -c 20000; sleep 30"},
		MaxScroll: 1024,
		Env:       []string{"TERM=dumb"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close()

	deadline := time.Now().Add(3 * time.Second)
	truncated := false
	for time.Now().Before(deadline) {
		_, tr := s.Read()
		if tr {
			truncated = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !truncated {
		t.Fatal("expected a truncated read once the scrollback bound was exceeded")
	}
	scroll, dropped := s.Scrollback()
	if !dropped {
		t.Fatal("scrollback should report dropped bytes")
	}
	if len(scroll) > 2048 {
		t.Fatalf("scrollback retained %d bytes, want <= ~1024", len(scroll))
	}
}

package agent

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestJobNoticeFiresOnNaturalExit pins the notice hook: a background job
// finishing naturally fires the hook with a one-line summary carrying the
// exit code.
func TestJobNoticeFiresOnNaturalExit(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	notices := make(chan string, 4)
	a.SetJobNoticeHook(func(summary string) {
		select {
		case notices <- summary:
		default:
		}
	})

	if _, err := a.StartBackgroundCommand(context.Background(), "echo notice-me"); err != nil {
		t.Fatal(err)
	}
	select {
	case summary := <-notices:
		if !strings.Contains(summary, "[job]") || !strings.Contains(summary, "exited with code 0") {
			t.Fatalf("summary = %q", summary)
		}
		if !strings.Contains(summary, "echo notice-me") {
			t.Fatalf("summary missing command: %q", summary)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("notice hook was not fired")
	}
}

// TestJobNoticeNotOnCancel pins the cancelled-job exclusion: a job killed
// via CancelBackgroundJob must NOT fire the notice hook.
func TestJobNoticeNotOnCancel(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)
	defer a.Close()

	fired := make(chan struct{}, 1)
	a.SetJobNoticeHook(func(string) {
		fired <- struct{}{}
	})

	id, err := a.StartBackgroundCommand(context.Background(), "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CancelBackgroundJob(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
		t.Fatal("notice hook must not fire for a cancelled job")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestJobNoticeSummaryTruncation pins the command cap in the notice
// summary: long commands are truncated so secrets/long args never dump
// into the transcript.
func TestJobNoticeSummaryTruncation(t *testing.T) {
	job := &BackgroundJob{
		ID:      "job-x",
		Command: strings.Repeat("a", maxJobNoticeCommandLen+50),
	}
	summary := jobExitSummary(job)
	if len(summary) > maxJobNoticeCommandLen+64 {
		t.Fatalf("summary too long: %d bytes", len(summary))
	}
	if !strings.Contains(summary, "…") {
		t.Fatalf("summary missing truncation marker: %q", summary)
	}
	if !strings.Contains(summary, "job-x") {
		t.Fatalf("summary missing id: %q", summary)
	}
}

// TestJobNoticeSummaryTruncationRuneSafe pins the rune-boundary
// truncation: a multi-byte character straddling the cap must not be split
// (the summary stays valid UTF-8).
func TestJobNoticeSummaryTruncationRuneSafe(t *testing.T) {
	cmd := strings.Repeat("a", maxJobNoticeCommandLen-1) + "日本語"
	job := &BackgroundJob{ID: "job-x", Command: cmd}
	summary := jobExitSummary(job)
	if !utf8.ValidString(summary) {
		t.Fatalf("summary is not valid UTF-8: %q", summary)
	}
	if !strings.Contains(summary, "…") {
		t.Fatalf("summary missing truncation marker: %q", summary)
	}
}

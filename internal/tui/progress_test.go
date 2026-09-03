package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
)

func TestProgressAnimating(t *testing.T) {
	m := Model{
		streaming:     true,
		progressPhase: progressThinking,
		spinner:       newProgressSpinner(),
	}
	if !m.progressAnimating() {
		t.Fatal("thinking should animate")
	}
	m.progressPhase = progressTool
	if !m.progressAnimating() {
		t.Fatal("tool should animate")
	}
	m.progressPhase = progressActive
	if m.progressAnimating() {
		t.Fatal("active streaming should not animate")
	}
	m.progressPhase = progressThinking
	m.streaming = false
	if m.progressAnimating() {
		t.Fatal("stopped stream should not animate")
	}
}

func TestSetProgressRestartsTick(t *testing.T) {
	m := Model{
		streaming:     true,
		progressPhase: progressActive,
		spinner:       newProgressSpinner(),
	}
	cmd := m.setProgress(progressThinking, "thinking")
	if cmd == nil {
		t.Fatal("expected tick cmd when re-entering thinking")
	}
	if m.progressLabel != "thinking" {
		t.Fatalf("label=%q", m.progressLabel)
	}
	cmd = m.setProgress(progressThinking, "thinking")
	if cmd != nil {
		t.Fatal("already animating should not restart tick")
	}
}

func TestActiveToolNameLifecycle(t *testing.T) {
	m := newStreamTestModel()

	m.handleStreamToolCall(0, "tc0", "read_file")
	if m.activeToolName != "read_file" {
		t.Fatalf("activeToolName after announce=%q, want read_file", m.activeToolName)
	}
	m.handleStreamToolResult("tc0", "read_file", "ok", true)
	if m.activeToolName != "" {
		t.Fatalf("activeToolName after result=%q, want empty", m.activeToolName)
	}

	m.handleStreamToolCall(1, "tc1", "patch_file")
	if m.activeToolName != "patch_file" {
		t.Fatalf("activeToolName after second announce=%q, want patch_file", m.activeToolName)
	}
	m.resetStreamState(false)
	if m.activeToolName != "" {
		t.Fatalf("activeToolName after reset=%q, want empty", m.activeToolName)
	}
}

func TestRenderProgressInput(t *testing.T) {
	ta := textarea.New()
	ta.SetHeight(3)
	m := Model{
		streaming:     true,
		progressPhase: progressActive,
		spinner:       newProgressSpinner(),
		textarea:      ta,
	}
	got := m.renderProgressInput()
	if !strings.Contains(got, "streaming") {
		t.Fatalf("active render=%q", got)
	}
	if h := lipgloss.Height(got); h != 3 {
		t.Fatalf("progress input height=%d, want 3 (match textarea)", h)
	}
	// A tool whose arguments are streaming in should be named, not generic.
	m.progressPhase = progressActive
	m.activeToolName = "patch_file"
	got = m.renderProgressInput()
	if !strings.Contains(got, "patch_file") || !strings.Contains(got, "preparing") {
		t.Fatalf("active tool render=%q", got)
	}
	m.progressPhase = progressTool
	m.progressLabel = "running read_file"
	got = m.renderProgressInput()
	if !strings.Contains(got, "read_file") {
		t.Fatalf("tool render=%q", got)
	}
	if h := lipgloss.Height(got); h != 3 {
		t.Fatalf("tool progress height=%d, want 3", h)
	}
}

func TestStreamStatsProgressLine(t *testing.T) {
	m := newStreamTestModel()

	// Not streaming: stats are ignored (a superseded turn's straggler).
	m.handleStreamStatsMsg(streamStatsMsg{toksPerSec: 42})
	if m.streamSpeedLine != "" {
		t.Fatalf("stats applied while not streaming: %q", m.streamSpeedLine)
	}

	m.streaming = true
	m.progressPhase = progressActive
	m.handleStreamStatsMsg(streamStatsMsg{toksPerSec: 42})
	if m.streamSpeedLine == "" {
		t.Fatal("streamSpeedLine not set after stats")
	}
	if got := m.renderProgressInput(); !strings.Contains(got, "42 tok/s") {
		t.Fatalf("progress line missing the rate: %q", got)
	}

	// The rate shows while a tool's arguments stream in too (the args
	// count toward the shared meter).
	m.activeToolName = "patch_file"
	if got := m.renderProgressInput(); !strings.Contains(got, "42 tok/s") {
		t.Fatalf("preparing line missing the rate: %q", got)
	}

	// The rate shows on the thinking indicator too (thinking tokens
	// feed the shared meter).
	m.activeToolName = ""
	m.progressPhase = progressThinking
	m.progressLabel = "thinking"
	m.spinner = newProgressSpinner()
	if got := m.renderProgressInput(); !strings.Contains(got, "42 tok/s") {
		t.Fatalf("thinking line missing the rate: %q", got)
	}

	// Turn end / cancel clears the rate with the progress.
	m.clearProgress()
	if m.streamSpeedLine != "" {
		t.Fatalf("streamSpeedLine not cleared: %q", m.streamSpeedLine)
	}
}

func TestPadInputBand(t *testing.T) {
	if got := padInputBand("hi", 1); got != "hi" {
		t.Fatalf("height 1: %q", got)
	}
	if got := padInputBand("hi", 3); got != "hi\n\n" {
		t.Fatalf("height 3: %q", got)
	}
	if got := padInputBand("hi", 0); got != "hi" {
		t.Fatalf("height 0 clamps to 1: %q", got)
	}
}

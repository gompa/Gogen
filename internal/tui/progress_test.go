package tui

import (
	"strings"
	"testing"
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

func TestRenderProgressInput(t *testing.T) {
	m := Model{
		streaming:     true,
		progressPhase: progressActive,
		spinner:       newProgressSpinner(),
	}
	if got := m.renderProgressInput(); !strings.Contains(got, "streaming") {
		t.Fatalf("active render=%q", got)
	}
	m.progressPhase = progressTool
	m.progressLabel = "running read_file"
	if got := m.renderProgressInput(); !strings.Contains(got, "read_file") {
		t.Fatalf("tool render=%q", got)
	}
}

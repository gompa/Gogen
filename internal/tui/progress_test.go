package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/lipgloss"
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

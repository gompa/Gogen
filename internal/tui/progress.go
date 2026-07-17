package tui

import (
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// progressPhase controls the input-area wait indicator.
// Spinner animates only for idle waits; token/tool-arg streaming is the progress.
type progressPhase int

const (
	progressHidden   progressPhase = iota // not in a turn
	progressThinking                      // waiting on the model
	progressActive                        // tokens / tool args flowing in chat
	progressTool                          // a tool is executing
)

func newProgressSpinner() spinner.Model {
	return spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(DimStyle),
	)
}

func (m *Model) progressAnimating() bool {
	return m.streaming && (m.progressPhase == progressThinking || m.progressPhase == progressTool)
}

// setProgress updates the wait indicator. Returns a spinner tick when animation
// should (re)start after being stopped.
func (m *Model) setProgress(phase progressPhase, label string) tea.Cmd {
	wasAnimating := m.progressAnimating()
	m.progressPhase = phase
	m.progressLabel = label
	if m.progressAnimating() && !wasAnimating {
		return m.spinner.Tick
	}
	return nil
}

func (m *Model) clearProgress() {
	m.progressPhase = progressHidden
	m.progressLabel = ""
}

func (m *Model) renderProgressInput() string {
	switch m.progressPhase {
	case progressThinking:
		label := m.progressLabel
		if label == "" {
			label = "thinking"
		}
		return DimStyle.Render("  " + m.spinner.View() + " " + label)
	case progressTool:
		label := m.progressLabel
		if label == "" {
			label = "running tool"
		}
		return DimStyle.Render("  " + m.spinner.View() + " " + label)
	case progressActive:
		return DimStyle.Render("  streaming…")
	default:
		return DimStyle.Render("  … processing …")
	}
}

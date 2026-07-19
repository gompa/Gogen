package tui

import (
	"strings"

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

// Pre-rendered static progress lines so DimStyle.Render is not called every
// frame for content that never changes between renders.
var (
	progressStreamingLine  = DimStyle.Render("  streaming…")
	progressProcessingLine = DimStyle.Render("  … processing …")
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

// renderProgressInput draws the wait indicator in the input band.
// It is padded to the textarea height so the layout does not jump when a turn
// starts or ends (viewport height is sized for the textarea, not 1 line).
func (m *Model) renderProgressInput() string {
	var line string
	switch m.progressPhase {
	case progressThinking:
		label := m.progressLabel
		if label == "" {
			label = "thinking"
		}
		line = DimStyle.Render("  " + m.spinner.View() + " " + label)
	case progressTool:
		label := m.progressLabel
		if label == "" {
			label = "running tool"
		}
		line = DimStyle.Render("  "+m.spinner.View()+" "+label)
	case progressActive:
		line = progressStreamingLine
	default:
		line = progressProcessingLine
	}
	return padInputBand(line, m.textarea.Height())
}

// padInputBand ensures the input area occupies exactly height rows.
func padInputBand(line string, height int) string {
	if height < 1 {
		height = 1
	}
	if height == 1 {
		return line
	}
	return line + strings.Repeat("\n", height-1)
}

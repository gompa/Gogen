package tui

import (
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
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
	progressStreamingLine = DimStyle.Render("  streaming…")
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

// focusedSession returns the focused live session, or nil when the Model
// has no registry (unit-test constructions). Progress mirroring is a
// no-op in that case.
func (m *Model) focusedSession() *liveSession {
	if m.lives == nil {
		return nil
	}
	return m.lives.Active()
}

// setProgress updates the wait indicator. Returns a spinner tick when animation
// should (re)start after being stopped.
//
// The phase/label are mirrored onto the focused live session so the state
// survives a focus switch: switchToLive restores the target session's
// recorded phase instead of hardcoding "thinking".
func (m *Model) setProgress(phase progressPhase, label string) tea.Cmd {
	wasAnimating := m.progressAnimating()
	m.progressPhase = phase
	m.progressLabel = label
	if s := m.focusedSession(); s != nil {
		s.progressPhase = phase
		s.progressLabel = label
	}
	if m.progressAnimating() && !wasAnimating {
		return m.spinner.Tick
	}
	return nil
}

// setActiveTool names the tool being prepared/executed for the progress
// indicator; mirrored onto the focused session like setProgress.
func (m *Model) setActiveTool(name string) {
	m.activeToolName = name
	if s := m.focusedSession(); s != nil {
		s.activeTool = name
	}
}

func (m *Model) clearProgress() {
	m.progressPhase = progressHidden
	m.progressLabel = ""
	// No tool is being prepared/executed any more (turn end, cancel, error).
	m.activeToolName = ""
	m.streamSpeedLine = ""
	if s := m.focusedSession(); s != nil {
		s.resetProgress()
	}
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
		// Token rate from the shared SpeedMeter (thinking tokens count
		// toward it too); empty until the first stats message of the
		// round, so "waiting for the model" never shows a stale rate.
		if m.streamSpeedLine != "" {
			line += " " + m.streamSpeedLine
		}
	case progressTool:
		label := m.progressLabel
		if label == "" {
			label = "running tool"
		}
		line = DimStyle.Render("  " + m.spinner.View() + " " + label)
	case progressActive:
		// Name the tool whose arguments are streaming in, so the long
		// pre-execution stretch of tools like patch_file is not opaque.
		if m.activeToolName != "" {
			line = DimStyle.Render("  preparing " + m.activeToolName + "…")
		} else {
			line = progressStreamingLine
		}
		// Token rate from the shared SpeedMeter (rendered once per stats
		// message on the Update thread, not per frame).
		if m.streamSpeedLine != "" {
			line += " " + m.streamSpeedLine
		}
	default:
		// Fallback for progressHidden (renderProgressInput is only called
		// when streaming is true, but handle defensively).
		line = ""
	}
	return padInputBand(line, m.textarea.Height())
}

// renderCompactingInput draws the /compact wait indicator in the input
// band. The compaction runs off the Update thread; the spinner animates
// via handleSpinnerTick, which also ticks while compacting.
func (m *Model) renderCompactingInput() string {
	line := DimStyle.Render("  " + m.spinner.View() + " compacting history…")
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

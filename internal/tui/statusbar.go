package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m *Model) renderStatusBar() string {
	if m.width <= 0 {
		return ""
	}

	var leftParts []string

	// Mode
	modeStr := m.agent.Mode.String()
	if modeStr == "plan" {
		leftParts = append(leftParts, StatusBarPlanStyle.Render("[plan]"))
	} else {
		leftParts = append(leftParts, StatusBarActStyle.Render("[act]"))
	}

	// Model
	if model := m.agent.CurrentModel(); model != "" {
		leftParts = append(leftParts, StatusBarDimStyle.Render(model))
	}

	// Working directory
	if wd := m.agent.WorkingDir; wd != "" {
		leftParts = append(leftParts, StatusBarDimStyle.Render(wd))
	}

	// Verbose indicator
	if m.verbose {
		leftParts = append(leftParts, StatusBarWarningStyle.Render("[verbose]"))
	}

	left := strings.Join(leftParts, " ")

	// Context line (right-aligned)
	right := ""
	if m.contextLine != "" {
		pct := 0
		if m.contextStats.Snapshot.Limit > 0 {
			pct = int(m.contextStats.Snapshot.Percent * 100)
		}
		style := StatusBarDimStyle
		if pct >= 90 {
			style = StatusBarDangerStyle
		} else if pct >= 75 || m.contextStats.Snapshot.NearCompact {
			style = StatusBarWarningStyle
		}
		right = style.Render(m.contextLine)
	}

	// Layout: left and right with padding between
	availWidth := m.width - 2 // -2 for padding
	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)

	middleWidth := availWidth - leftWidth - rightWidth
	if middleWidth < 1 {
		middleWidth = 1
	}

	content := left + strings.Repeat(" ", middleWidth) + right

	return StatusBarStyle.Copy().Width(m.width).Render(content)
}

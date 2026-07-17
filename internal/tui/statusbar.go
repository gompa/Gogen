package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) renderStatusBar() string {
	if m.width <= 0 {
		return ""
	}

	var leftParts []string

	// Transient status message (e.g. "Copied N chars") takes priority
	if m.statusMsg != "" {
		content := StatusBarDimStyle.Render(m.statusMsg)
		// Center the message
		msgWidth := lipgloss.Width(content)
		padLeft := max(0, (m.width-msgWidth)/2)
		result := strings.Repeat(" ", padLeft) + content
		return StatusBarStyle.Copy().Width(m.width).Render(result)
	}

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

	// Layout: left and right with padding between. Prefer keeping the context
	// indicator visible — truncate the left side first when the bar is tight.
	availWidth := m.width - 2 // -2 for padding
	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)

	middleWidth := availWidth - leftWidth - rightWidth
	if middleWidth < 1 {
		keepRight := rightWidth
		if keepRight > availWidth-2 {
			keepRight = max(0, availWidth-2)
			right = ansi.Cut(right, 0, keepRight)
			rightWidth = lipgloss.Width(right)
		}
		maxLeft := max(0, availWidth-rightWidth-1)
		if leftWidth > maxLeft {
			left = ansi.Cut(left, 0, maxLeft)
			leftWidth = lipgloss.Width(left)
		}
		middleWidth = max(1, availWidth-leftWidth-rightWidth)
	}

	content := left + strings.Repeat(" ", middleWidth) + right

	return StatusBarStyle.Copy().Width(m.width).Render(content)
}

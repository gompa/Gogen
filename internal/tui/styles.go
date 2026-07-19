package tui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

var (
	noColor bool
)

func init() {
	noColor = os.Getenv("NO_COLOR") != ""
	if noColor {
		ansiHighlightOn = ""
		ansiDimOn = ""
		ansiReset = ""
	} else {
		ansiHighlightOn = "\x1b[1m\x1b[38;2;0;170;170m"
		ansiDimOn = "\x1b[38;2;136;136;136m"
		// Only reset foreground + bold, never background.
		// This keeps the overlay's #1a1a1a background alive past
		// the end of styled text, preventing black gaps and bleed.
		ansiReset = "\x1b[39m\x1b[22m"
	}
}

// ansiHighlightOn / ansiDimOn are the start of each modal-row style.
// ansiReset reverts only foreground/bold so that the modal overlay's
// background (set by ModalOverlayBackground) is never killed mid-line.
var (
	ansiHighlightOn string
	ansiDimOn       string
	ansiReset       string
)

func lipglossColor(hex string) lipgloss.Color {
	if noColor {
		return lipgloss.Color("")
	}
	return lipgloss.Color(hex)
}

// Shared lipgloss styles for the TUI.
var (
	// Base
	BaseStyle = lipgloss.NewStyle()

	// Chat viewport
	ViewportStyle = lipgloss.NewStyle().
			PaddingLeft(1).
			PaddingRight(1)

	// Message roles
	AssistantStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipglossColor("#00AAAA"))

	SystemStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#888888"))

	DimStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#888888"))

	UserStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipglossColor("#CCAA00"))

	// Thinking blocks
	ThinkingTagStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	ThinkingStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#888888"))

	// Tool calls
	ToolCallStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#CCAA00"))

	ToolCallArgsStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	// Tool results
	ToolResultOKStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#00AA00"))

	ToolResultFailStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#AA0000"))

	ToolResultBodyStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	ToolResultMarkStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	// Status bar
	StatusBarStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#FFFFFF")).
			Background(lipglossColor("#333333")).
			Padding(0, 1).
			Width(0) // filled by layout

	StatusBarDimStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#AAAAAA")).
				Background(lipglossColor("#333333"))

	StatusBarWarningStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#CCAA00")).
				Background(lipglossColor("#333333"))

	StatusBarDangerStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#AA0000")).
				Background(lipglossColor("#333333"))

	StatusBarPlanStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#CCAA00")).
				Background(lipglossColor("#333333"))

	StatusBarActStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#00AA00")).
				Background(lipglossColor("#333333"))

	// Modals
	ModalOverlayStyle = lipgloss.NewStyle().
				Background(lipglossColor("#000000")).
				Foreground(lipglossColor("#FFFFFF"))

	// Background fill for modal overlay (cached to avoid allocation per frame).
	ModalOverlayBackground = lipgloss.NewStyle().
				Background(lipglossColor("#1a1a1a"))

	ModalBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipglossColor("#555555")).
				Padding(1, 2)

	ModalTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipglossColor("#00AAAA"))

	ModalDimStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#888888"))

	ModalHighlightStyle = lipgloss.NewStyle().
				// Uses foreground+bold instead of background to avoid
				// color bleeding into the border padding of modal boxes.
				Foreground(lipglossColor("#00AAAA")).
				Bold(true)

	ModalPromptStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#CCAA00"))

	// Help
	HelpKeyStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#00AAAA"))

	HelpDescStyle = lipgloss.NewStyle()

	// Error
	ErrorStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#AA0000"))

	// Input
	InputPromptStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#00AAAA"))

	// Divider
	DividerStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#555555"))

	// Context line (right-aligned, dim)
	ContextLineStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	// Diff rendering
	DiffAddStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#00AA00"))

	DiffDelStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#AA0000"))

	DiffHunkStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#00AAAA"))

	DiffMetaStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#AAAA00")).
			Bold(true)
)

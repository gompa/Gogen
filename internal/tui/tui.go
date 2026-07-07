package tui

import (
	"context"
	"fmt"
	"os"

	"gogen/internal/agent"
	"gogen/internal/config"

	tea "github.com/charmbracelet/bubbletea"
)

const assistantLabel = "GoGen:"
const userLabel = "You:"

// TUI runs the terminal UI.
type TUI struct {
	agent *agent.Agent
	cfg   *config.Config
}

// New creates a new TUI runner.
func New(a *agent.Agent, cfg *config.Config) *TUI {
	return &TUI{agent: a, cfg: cfg}
}

// Run starts the Bubble Tea program loop.
// No alt-screen and no mouse capture means the terminal handles
// native text selection with the mouse, and scrollback is preserved.
// Viewport navigation is keyboard-driven (PgUp/PgDown/j/k/Home/End).
func (t *TUI) Run(ctx context.Context) {
	m := NewModel(t.agent, t.cfg)
	m.ctx = ctx

	p := tea.NewProgram(
		&m,
		tea.WithContext(ctx),
	)

	m.program = p

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}
}

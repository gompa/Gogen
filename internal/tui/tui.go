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
// Alt-screen is off so terminal scrollback and Shift+click text selection work.
// Mouse reporting is on so the viewport handles wheel scrolls.
func (t *TUI) Run(ctx context.Context) {
	m := NewModel(t.agent, t.cfg)
	m.ctx = ctx

	p := tea.NewProgram(
		&m,
		tea.WithContext(ctx),
		tea.WithMouseCellMotion(),
	)

	m.program = p

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}
}

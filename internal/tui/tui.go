package tui

import (
	"context"

	"gogen/internal/agent"
	"gogen/internal/config"

	tea "github.com/charmbracelet/bubbletea"
)

const assistantLabel = "GoGen:"

// NewTUI creates a new TUI runner.
type TUI struct {
	agent *agent.Agent
	cfg   *config.Config
}

// NewTUI creates a new TUI runner.
func New(a *agent.Agent, cfg *config.Config) *TUI {
	return &TUI{agent: a, cfg: cfg}
}

// Run starts the Bubble Tea program loop.
func (t *TUI) Run(ctx context.Context) {
	m := NewModel(t.agent, t.cfg)
	m.ctx = ctx

	p := tea.NewProgram(
		&m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	m.program = p

	// Store cleanup so we restore terminal on exit.
	if _, err := p.Run(); err != nil {
		// p.Run already restored terminal; just report.
		panic(err)
	}
}

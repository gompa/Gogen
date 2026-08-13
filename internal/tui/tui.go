package tui

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"

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

	// program is the running Bubble Tea program, stored atomically: it is
	// written in Run and read by ForceRender from the background
	// validation goroutine.
	program atomic.Pointer[tea.Program]
	// modelChanged records a background model change (restore validation)
	// that arrived before Run started, so the first render reflects it.
	modelChanged atomic.Bool
}

// New creates a new TUI runner.
func New(a *agent.Agent, cfg *config.Config) *TUI {
	t := &TUI{agent: a, cfg: cfg}
	// Background validation (ValidateRestoredModel) may clear or
	// auto-select a restored session's model after the UI opened; hook it
	// so the status bar re-renders even while the terminal is idle.
	if a != nil {
		a.OnModelChanged = t.ForceRender
	}
	return t
}

// ForceRender schedules a re-render after a background state change (e.g.
// model validation) altered what the status bar shows. Safe to call from any
// goroutine; when the program is not running yet the change is drained once
// Run starts.
func (t *TUI) ForceRender() {
	t.modelChanged.Store(true)
	if p := t.program.Load(); p != nil {
		p.Send(modelChangedMsg{})
	}
}

// Run starts the Bubble Tea program loop.
// Alt-screen is off so terminal scrollback and Shift+click text selection work.
// Mouse reporting is on so the viewport handles wheel scrolls.
func (t *TUI) Run(ctx context.Context) {
	m := NewModel(t.agent, t.cfg)
	m.ctx = ctx

	p := tea.NewProgram(
		m,
		tea.WithContext(ctx),
		tea.WithMouseCellMotion(),
	)

	m.program = p
	t.program.Store(p)
	// A validation finishing before Run started would have missed the
	// program; drain the pending flag so the first render already reflects
	// the change.
	if t.modelChanged.Swap(false) {
		p.Send(modelChangedMsg{})
	}

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}
}

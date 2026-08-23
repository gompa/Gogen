package tui

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/server"

	tea "charm.land/bubbletea/v2"
)

const assistantLabel = "GoGen:"
const userLabel = "You:"

// noticeLabel prefixes system-delivered messages (job notices, subagent
// reports): they are injected as user messages in the transcript, but
// rendering them as "You:" would imply the user typed them.
const noticeLabel = "Notice:"

// TUI runs the terminal UI.
type TUI struct {
	agent *agent.Agent
	cfg   *config.Config

	// workspace is the shared web lifecycle core (server.NewWorkspaceForHost):
	// additional live sessions are spawned through its NewSessionAgent so
	// both frontends drive one implementation. May be nil (tests/embedders).
	workspace *server.Workspace

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
	return NewWithWorkspace(a, cfg, nil)
}

// NewWithWorkspace is New with the shared web lifecycle core attached;
// /open spawns additional live sessions through it. A nil workspace keeps
// single-session behavior (New callers, tests).
func NewWithWorkspace(a *agent.Agent, cfg *config.Config, ws *server.Workspace) *TUI {
	t := &TUI{agent: a, cfg: cfg, workspace: ws}
	// Background validation (ValidateRestoredModel) may clear or
	// auto-select a restored session's model after the UI opened; hook it
	// so the status bar re-renders even while the terminal is idle.
	if a != nil {
		a.OnModelChanged = t.ForceRender
	}
	// Job-completion notices: queue a delivery message on the tea loop
	// (deliveryRequestMsg) so the notice renders and runs a turn at the
	// next idle boundary. The program may not have started yet (a job
	// finishing before Run) — then the notice is dropped (at-most-once).
	if a != nil && cfg != nil && cfg.JobNoticesEnabled() {
		a.SetJobNoticeHook(func(summary string) {
			if p := t.program.Load(); p != nil {
				p.Send(deliveryRequestMsg{text: summary})
			}
		})
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
	m.workspace = t.workspace

	// v2: mouse reporting is requested declaratively from View()
	// (MouseModeCellMotion), not via a program option.
	p := tea.NewProgram(
		m,
		tea.WithContext(ctx),
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

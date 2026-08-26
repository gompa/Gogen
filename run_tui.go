package main

import (
	"context"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/server"
	"gogen/internal/tui"
)

// runTUI runs the interactive Bubble Tea interface until it quits. Model
// validation runs in the background so the TUI can open immediately.
// notices are the setup-phase messages (model selection, session restore):
// they are surfaced through the TUI's managed render path instead of raw
// stderr, whose untracked lines ahead of the inline-rendered frame desync
// the renderer from the terminal.
func runTUI(ctx context.Context, a *agent.Agent, cfg *config.Config, restoredModel string, notices []string) {
	// Attach the shared web lifecycle core so /open can spawn additional
	// live sessions (same seeding as web panes); nil-safe for tests.
	c := tui.NewWithWorkspace(a, cfg, server.NewWorkspaceForHost(a, cfg))
	c.SetStartupNotices(notices)
	// tui.New installs the model-change hook (ForceRender), so a background
	// ValidateRestoredModel that clears or auto-selects a restored model
	// re-renders the status bar even while the terminal is idle.
	go a.ValidateRestoredModel(context.Background(), restoredModel)
	c.Run(ctx)
}

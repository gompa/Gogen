package main

import (
	"context"
	"log"

	"gogen/internal/agent"
	"gogen/internal/automation"
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
	// Automation scheduler (automations feature flag): sweeps the global
	// store and fires due automations as headless `-p` subprocesses while
	// the TUI runs. Stopped on ctx cancel (signal shutdown) or TUI quit.
	stopAutomations := automation.StartHostScheduler(ctx, cfg != nil && cfg.AutomationsEnabled(), log.Printf)
	defer stopAutomations()
	// tui.New installs the model-change hook (ForceRender), so a background
	// validation that clears or auto-selects a restored model re-renders the
	// status bar even while the terminal is idle — including later
	// /resume-driven validations. The nil callback keeps that hook (the
	// agent helper overwrites it only when given a callback).
	a.ValidateRestoredModelAsync(restoredModel, nil)
	c.Run(ctx)
}

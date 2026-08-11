package main

import (
	"context"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/tui"
)

// runTUI runs the interactive Bubble Tea interface until it quits. Model
// validation runs in the background so the TUI can open immediately.
func runTUI(ctx context.Context, a *agent.Agent, cfg *config.Config, restoredModel string) {
	c := tui.New(a, cfg)
	// tui.New installs the model-change hook (ForceRender), so a background
	// ValidateRestoredModel that clears or auto-selects a restored model
	// re-renders the status bar even while the terminal is idle.
	go a.ValidateRestoredModel(context.Background(), restoredModel)
	c.Run(ctx)
}

package tui

import (
	"context"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// subagentRecord is one finished nested session shown by /subagents.
type subagentRecord struct {
	label  string
	report string
	err    error
}

// tuiSubagentSpawner runs nested sessions for the TUI: a fresh agent over a
// fresh provider, sharing the parent's executor, store, and project context
// (D9 — the shared session factory). Children are ephemeral (no registry,
// no panes); their final reports land in the parent's tool result AND in
// the /subagents modal list. The existing turn spinner is the running
// indicator; nesting follows subagent_max_depth (default 1 = children
// cannot spawn).
type tuiSubagentSpawner struct {
	cfg *config.Config
	m   *Model // for the /subagents list (nil in tests)
}

func (sp *tuiSubagentSpawner) Spawn(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	cfg := sp.cfg
	// The child must inherit the PARENT's current model, not the startup
	// config value: the user may have switched models at runtime (/model),
	// or the model may only have been selected interactively (cfg
	// OpenAIModel empty) — a child seeded from cfg would fail
	// requireModelSelected or run a different model than its parent. The
	// provider working dir follows the parent's live working dir too. An
	// explicit model argument on the subagent tool wins over inheritance.
	// The child carries ALL registered OpenAI-compatible profiles (the
	// legacy fields form the implicit default), like the parent: a model
	// inherited from a secondary provider must route to its owning endpoint,
	// not fall back to the default client (D9).
	prov := llm.NewOpenAIProviderWithProfiles(
		llm.ProviderProfiles(cfg.OpenAIKey, cfg.OpenAIModel, cfg.OpenAIURL, cfg.OpenAIProviders),
		cfg.OpenAIModel, parent.WorkingDir, nil)
	if model != "" {
		_ = prov.SetModel(model)
	} else if m := cfg.SubagentModel; m != "" {
		// The configured default subagent model (config file / env) beats
		// parent-model inheritance, mirroring the web spawner.
		_ = prov.SetModel(m)
	} else if m := parent.CurrentModel(); m != "" {
		_ = prov.SetModel(m)
	}
	child := agent.NewSessionAgent(agent.SessionAgentOptions{
		Provider:             prov,
		Executor:             parent.Executor,
		Store:                parent.SessionStore,
		Config:               cfg,
		GlobalMode:           parent.GlobalMode,
		ProjectFilePath:      parent.ProjectFilePath,
		ProjectGuidelines:    parent.ProjectGuidelines,
		TestCommand:          parent.TestCommand,
		LintCommand:          parent.LintCommand,
		WorkingDir:           parent.WorkingDir,
		MCPRegistry:          parent.MCPRegistry,
		DebugCompareMessages: parent.DebugCompareMessages,
		BoardEnabled:         parent.BoardEnabled(),
		SubagentsEnabled:     parent.SubagentsEnabled(),
		SubagentMaxDepth:     parent.SubagentMaxDepth(),
		BoardManager:         parent.BoardManager(),
		SubagentSpawner:      sp, // nesting allowed up to the configured depth
	}, nil, session.NewID())
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	label := agent.SubagentLabel(job)
	_, _ = child.RenameSession(label)
	// The job wrapper applies AFTER label derivation, so the /subagents
	// list keeps the original job the parent wrote; only the child's first
	// message carries the wrapped job.
	report, err := child.StreamProcessInputWithImages(ctx, agent.FormatSubagentJob(job), nil, nil)
	if sp.m != nil {
		sp.m.recordSubagent(subagentRecord{label: label, report: agent.TruncateSubagentReport(report), err: err})
	}
	return report, err
}

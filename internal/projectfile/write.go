package projectfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gogen/internal/config"
	"gopkg.in/yaml.v3"
)

// SaveConfig writes effective configuration to cfgPath as pure YAML
// and guidelines to guidelinesPath as markdown. When guidelinesPath is empty,
// only the config file is written (used for global config saves).
func SaveConfig(cfgPath, guidelinesPath string, cfg *config.Config, guidelines string, opts WriteOptions) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	yamlBody, err := buildConfigYAML(cfg, opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o644); err != nil {
		return err
	}

	if guidelinesPath == "" {
		return nil
	}

	if strings.TrimSpace(guidelines) == "" {
		guidelines = defaultGuidelinesPlaceholder
	}
	mdBody := strings.TrimRight(strings.TrimLeft(guidelines, "\n"), "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(guidelinesPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(guidelinesPath, []byte(mdBody), 0o644); err != nil {
		return err
	}
	return nil
}

// SaveGlobalConfig writes the effective configuration to the global config
// location (~/.config/gogen/config.yaml). No guidelines file is written.
func SaveGlobalConfig(cfg *config.Config, opts WriteOptions) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	path := GlobalConfigPath()
	yamlBody, err := buildConfigYAML(cfg, opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(yamlBody), 0o644); err != nil {
		return err
	}
	return nil
}

// configYAML is the YAML projection of the effective config written by
// --save-config. Field order defines output order.
//
// The four context settings (CompactThreshold, CompactKeepRecentMessages,
// MaxToolResultBytes, CompactReserveTokens) deliberately have no omitempty:
// their zero value is a real setting (auto-compaction off, no recent messages
// kept, no truncation cap, no reserved tokens), so an explicit 0 must survive
// regeneration. All other fields follow the file convention that an empty or
// zero value means "use the default", so they may be omitted when empty.
type configYAML struct {
	OpenAIAPIKey              string                   `yaml:"openai_api_key,omitempty"`
	OpenAIModel               string                   `yaml:"openai_model"`
	OpenAIBaseURL             string                   `yaml:"openai_base_url"`
	WorkingDir                string                   `yaml:"working_dir"`
	ContextLimit              int                      `yaml:"context_limit"`
	CompactThreshold          float64                  `yaml:"compact_threshold"`
	CompactKeepRecentMessages int                      `yaml:"compact_keep_recent_messages"`
	MaxToolResultBytes        int                      `yaml:"max_tool_result_bytes"`
	CompactReserveTokens      int                      `yaml:"compact_reserve_tokens"`
	CommandSafety             string                   `yaml:"command_safety"`
	CommandAllowlist          string                   `yaml:"command_allowlist,omitempty"`
	DeleteApproval            string                   `yaml:"delete_approval"`
	TreeSitter                string                   `yaml:"treesitter"`
	TreeSitterLangs           string                   `yaml:"treesitter_langs,omitempty"`
	TestCommand               string                   `yaml:"test_command,omitempty"`
	LintCommand               string                   `yaml:"lint_command,omitempty"`
	CLIVerbose                bool                     `yaml:"cli_verbose"`
	DebugLog                  string                   `yaml:"debug_log,omitempty"`
	DebugSession              string                   `yaml:"debug_session,omitempty"`
	MCP                       string                   `yaml:"mcp"`
	PreserveReasoning         string                   `yaml:"preserve_reasoning,omitempty"`
	MCPServers                []config.MCPServerConfig `yaml:"mcp_servers,omitempty"`
}

// buildConfigYAML renders the effective config as a YAML document. Secrets
// (openai_api_key, MCP server env) are included only when opts.IncludeSecrets
// is set; preserve_reasoning is omitted when it is the default "auto".
func buildConfigYAML(cfg *config.Config, opts WriteOptions) (string, error) {
	out := configYAML{
		OpenAIModel:               cfg.OpenAIModel,
		OpenAIBaseURL:             cfg.OpenAIURL,
		WorkingDir:                cfg.WorkingDir,
		ContextLimit:              cfg.ContextLimit,
		CompactThreshold:          cfg.CompactThreshold,
		CompactKeepRecentMessages: cfg.CompactKeepRecentMessages,
		MaxToolResultBytes:        cfg.MaxToolResultBytes,
		CompactReserveTokens:      cfg.CompactReserveTokens,
		CommandSafety:             cfg.CommandSafetyMode,
		CommandAllowlist:          cfg.CommandAllowlist,
		DeleteApproval:            cfg.DeleteApproval,
		TreeSitter:                cfg.TreeSitter,
		TreeSitterLangs:           cfg.TreeSitterLangs,
		TestCommand:               cfg.TestCommand,
		LintCommand:               cfg.LintCommand,
		CLIVerbose:                cfg.CLIVerbose,
		DebugLog:                  cfg.DebugLog,
		DebugSession:              cfg.DebugSession,
		MCP:                       cfg.MCP,
		MCPServers:                cfg.MCPServers,
	}
	if opts.IncludeSecrets && cfg.OpenAIKey != "" {
		out.OpenAIAPIKey = cfg.OpenAIKey
	}
	if mode := strings.ToLower(strings.TrimSpace(cfg.PreserveReasoning)); mode != "" && mode != "auto" {
		out.PreserveReasoning = mode
	}
	if !opts.IncludeSecrets && len(cfg.MCPServers) > 0 {
		// Copy without env so secrets are never persisted. The slice shares
		// its backing array with cfg, so a fresh copy is required.
		out.MCPServers = make([]config.MCPServerConfig, len(cfg.MCPServers))
		for i, s := range cfg.MCPServers {
			out.MCPServers[i] = config.MCPServerConfig{
				Name:    s.Name,
				Command: s.Command,
				Args:    append([]string(nil), s.Args...),
				// Env intentionally nil: persisted only with --save-config-secrets.
			}
		}
	}

	body, err := yaml.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("marshal config YAML: %w", err)
	}
	return string(body), nil
}

package projectfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gogen/internal/config"
)

// SaveConfig writes effective configuration to cfgPath as pure YAML
// and guidelines to guidelinesPath as markdown.
func SaveConfig(cfgPath, guidelinesPath string, cfg *config.Config, guidelines string, opts WriteOptions) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if strings.TrimSpace(guidelines) == "" {
		guidelines = defaultGuidelinesPlaceholder
	}

	yamlBody := strings.TrimRight(buildFrontMatter(cfg, opts), "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o644); err != nil {
		return err
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

func buildFrontMatter(cfg *config.Config, opts WriteOptions) string {
	var b strings.Builder
	b.WriteString("---\n")
	if opts.IncludeSecrets && cfg.OpenAIKey != "" {
		writeYAMLString(&b, "openai_api_key", cfg.OpenAIKey)
	} else if cfg.OpenAIKey != "" {
		b.WriteString("# openai_api_key: use OPENAI_API_KEY env var\n")
	}
	writeYAMLString(&b, "openai_model", cfg.OpenAIModel)
	writeYAMLString(&b, "openai_base_url", cfg.OpenAIURL)
	writeYAMLString(&b, "working_dir", cfg.WorkingDir)
	writeYAMLInt(&b, "context_limit", cfg.ContextLimit)
	writeYAMLFloat(&b, "compact_threshold", cfg.CompactThreshold)
	writeYAMLInt(&b, "keep_recent_messages", cfg.KeepRecentMessages)
	writeYAMLInt(&b, "max_tool_result_bytes", cfg.MaxToolResultBytes)
	writeYAMLInt(&b, "compact_reserve_tokens", cfg.CompactReserveTokens)
	writeYAMLString(&b, "command_safety", cfg.CommandSafetyMode)
	if cfg.CommandAllowlist != "" {
		writeYAMLString(&b, "command_allowlist", cfg.CommandAllowlist)
	}
	writeYAMLString(&b, "delete_approval", cfg.DeleteApproval)
	writeYAMLString(&b, "treesitter", cfg.TreeSitter)
	if cfg.TreeSitterLangs != "" {
		writeYAMLString(&b, "treesitter_langs", cfg.TreeSitterLangs)
	}
	if cfg.TestCommand != "" {
		writeYAMLString(&b, "test_command", cfg.TestCommand)
	}
	if cfg.LintCommand != "" {
		writeYAMLString(&b, "lint_command", cfg.LintCommand)
	}
	writeYAMLBool(&b, "cli_verbose", cfg.CLIVerbose)
	if cfg.DebugLog != "" {
		writeYAMLString(&b, "debug_log", cfg.DebugLog)
	}
	if cfg.DebugSession != "" {
		writeYAMLString(&b, "debug_session", cfg.DebugSession)
	}
	writeYAMLString(&b, "mcp", cfg.MCP)
	if len(cfg.MCPServers) > 0 {
		b.WriteString("mcp_servers:\n")
		for _, s := range cfg.MCPServers {
			b.WriteString(fmt.Sprintf("  - name: %q\n", s.Name))
			b.WriteString(fmt.Sprintf("    command: %q\n", s.Command))
			if len(s.Args) > 0 {
				b.WriteString("    args:\n")
				for _, arg := range s.Args {
					b.WriteString(fmt.Sprintf("      - %q\n", arg))
				}
			}
			if len(s.Env) > 0 {
				b.WriteString("    env:\n")
				for k, v := range s.Env {
					b.WriteString(fmt.Sprintf("      %s: %q\n", k, v))
				}
			}
		}
	}
	return b.String()
}

func writeYAMLString(b *strings.Builder, key, val string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(strconvQuote(val))
	b.WriteByte('\n')
}

func writeYAMLInt(b *strings.Builder, key string, val int) {
	fmt.Fprintf(b, "%s: %d\n", key, val)
}

func writeYAMLFloat(b *strings.Builder, key string, val float64) {
	fmt.Fprintf(b, "%s: %g\n", key, val)
}

func writeYAMLBool(b *strings.Builder, key string, val bool) {
	fmt.Fprintf(b, "%s: %t\n", key, val)
}

func strconvQuote(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := false
	for _, r := range s {
		if r == ':' || r == '#' || r == '\n' || r == '"' || r == '\'' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	return fmt.Sprintf("%q", s)
}

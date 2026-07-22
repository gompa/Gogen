package projectfile

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"gogen/internal/config"
)

// Merge builds the effective runtime config: flags > env > file > defaults.
func Merge(pf *ProjectFile, flags FlagOverrides) *config.Config {
	def := config.Defaults()
	var file FileConfig
	if pf != nil {
		file = pf.Config
	}

	cfg := &config.Config{
		OpenAIKey:            mergeString("OPENAI_API_KEY", file, "openai_api_key", file.OpenAIAPIKey, def.OpenAIKey),
		OpenAIModel:          mergeString("OPENAI_MODEL", file, "openai_model", file.OpenAIModel, def.OpenAIModel),
		OpenAIURL:            mergeString("OPENAI_BASE_URL", file, "openai_base_url", file.OpenAIBaseURL, def.OpenAIURL),
		WorkingDir:           mergeString("GOGEN_WORKING_DIR", file, "working_dir", file.WorkingDir, def.WorkingDir),
		ContextLimit:         mergeInt("GOGEN_CONTEXT_LIMIT", file, "context_limit", file.ContextLimit, def.ContextLimit),
		CompactThreshold:     mergeFloat("GOGEN_COMPACT_THRESHOLD", file, "compact_threshold", file.CompactThreshold, def.CompactThreshold),
		KeepRecentMessages:   mergeInt("GOGEN_KEEP_RECENT_MESSAGES", file, "keep_recent_messages", file.KeepRecentMessages, def.KeepRecentMessages),
		MaxToolResultBytes:   mergeInt("GOGEN_MAX_TOOL_RESULT_BYTES", file, "max_tool_result_bytes", file.MaxToolResultBytes, def.MaxToolResultBytes),
		CompactReserveTokens: mergeInt("GOGEN_COMPACT_RESERVE_TOKENS", file, "compact_reserve_tokens", file.CompactReserveTokens, def.CompactReserveTokens),
		CommandSafetyMode:    mergeString("GOGEN_COMMAND_SAFETY", file, "command_safety", file.CommandSafety, def.CommandSafetyMode),
		CommandAllowlist:     mergeString("GOGEN_COMMAND_ALLOWLIST", file, "command_allowlist", file.CommandAllowlist, def.CommandAllowlist),
		DeleteApproval:       mergeString("GOGEN_DELETE_APPROVAL", file, "delete_approval", file.DeleteApproval, def.DeleteApproval),
		TreeSitter:           mergeString("GOGEN_TREESITTER", file, "treesitter", file.TreeSitter, def.TreeSitter),
		TreeSitterLangs:      mergeString("GOGEN_TREESITTER_LANGS", file, "treesitter_langs", file.TreeSitterLangs, def.TreeSitterLangs),
		CLIVerbose:           mergeBool("GOGEN_CLI_VERBOSE", file, "cli_verbose", file.CLIVerbose, def.CLIVerbose),
		DebugLog:             mergeString("GOGEN_DEBUG_LOG", file, "debug_log", file.DebugLog, def.DebugLog),
		DebugSession:         mergeString("GOGEN_DEBUG_SESSION", file, "debug_session", file.DebugSession, def.DebugSession),
		MCP:                  mergeString("GOGEN_MCP", file, "mcp", file.MCP, def.MCP),
		DebugCompareMessages: mergeBool("GOGEN_DEBUG_COMPARE_MESSAGES", file, "debug_compare_messages", file.DebugCompareMessages, def.DebugCompareMessages),
		MCPServers:           mergeMCPServers(file),
		TestCommand:          mergeString("", file, "test_command", file.TestCommand, ""),
		LintCommand:          mergeString("", file, "lint_command", file.LintCommand, ""),
		WebBind:              mergeString("GOGEN_WEB_BIND", file, "", "", def.WebBind),
		WebAllowedOrigins:    mergeString("GOGEN_WEB_ALLOWED_ORIGINS", file, "", "", def.WebAllowedOrigins),
		WebAuthToken:         mergeString("GOGEN_WEB_TOKEN", file, "web_auth_token", file.WebAuthToken, def.WebAuthToken),
		WebTLSCertFile:       mergeString("GOGEN_WEB_TLS_CERT", file, "web_tls_cert_file", file.WebTLSCertFile, def.WebTLSCertFile),
		WebTLSKeyFile:        mergeString("GOGEN_WEB_TLS_KEY", file, "web_tls_key_file", file.WebTLSKeyFile, def.WebTLSKeyFile),
		SessionMaxCount:      mergeInt("GOGEN_SESSION_MAX_COUNT", file, "session_max_count", file.SessionMaxCount, def.SessionMaxCount),
		SessionMaxAgeDays:    mergeInt("GOGEN_SESSION_MAX_AGE_DAYS", file, "session_max_age_days", file.SessionMaxAgeDays, def.SessionMaxAgeDays),
		WebFetch:             mergeString("GOGEN_WEB_FETCH", file, "web_fetch", file.WebFetch, def.WebFetch),
		WebSearch:            mergeString("GOGEN_WEB_SEARCH", file, "web_search", file.WebSearch, def.WebSearch),
		WebSearchBackend:     mergeString("GOGEN_WEB_SEARCH_BACKEND", file, "web_search_backend", file.WebSearchBackend, def.WebSearchBackend),
		WebSearchAPIKey:      mergeString("GOGEN_WEB_SEARCH_API_KEY", file, "web_search_api_key", file.WebSearchAPIKey, def.WebSearchAPIKey),
		WebAllowedDomains:    mergeString("GOGEN_WEB_ALLOWED_DOMAINS", file, "web_allowed_domains", file.WebAllowedDomains, def.WebAllowedDomains),
		WebFetchMode:         mergeString("GOGEN_WEB_FETCH_MODE", file, "web_fetch_mode", file.WebFetchMode, def.WebFetchMode),
		CommandSandbox:       mergeString("GOGEN_COMMAND_SANDBOX", file, "command_sandbox", file.CommandSandbox, def.CommandSandbox),
		CommandTimeoutSecs:   mergeInt("GOGEN_COMMAND_TIMEOUT_SECS", file, "command_timeout_secs", file.CommandTimeoutSecs, def.CommandTimeoutSecs),
	}

	if flags.WorkingDir != "" {
		if _, ok := os.LookupEnv("GOGEN_WORKING_DIR"); !ok {
			cfg.WorkingDir = flags.WorkingDir
		}
	}
	if flags.OpenAIURL != "" {
		if _, ok := os.LookupEnv("OPENAI_BASE_URL"); !ok {
			cfg.OpenAIURL = flags.OpenAIURL
		}
	}
	if flags.CLIVerbose != nil {
		if _, ok := os.LookupEnv("GOGEN_CLI_VERBOSE"); !ok {
			cfg.CLIVerbose = *flags.CLIVerbose
		}
	}
	if flags.WebBind != "" {
		if _, ok := os.LookupEnv("GOGEN_WEB_BIND"); !ok {
			cfg.WebBind = flags.WebBind
		}
	}

	return cfg
}

func mergeMCPServers(file FileConfig) []config.MCPServerConfig {
	if _, ok := os.LookupEnv("GOGEN_MCP_SERVERS"); ok {
		raw := os.Getenv("GOGEN_MCP_SERVERS")
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		var servers []config.MCPServerConfig
		if err := json.Unmarshal([]byte(raw), &servers); err != nil {
			return nil
		}
		return servers
	}
	if _, ok := file.Keys["mcp_servers"]; ok {
		out := make([]config.MCPServerConfig, len(file.MCPServers))
		for i, s := range file.MCPServers {
			out[i] = config.MCPServerConfig{
				Name:    s.Name,
				Command: s.Command,
				Args:    append([]string(nil), s.Args...),
				Env:     cloneStringMap(s.Env),
			}
		}
		return out
	}
	return nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func fileHasKey(file FileConfig, key string) bool {
	if file.Keys == nil {
		return false
	}
	_, ok := file.Keys[key]
	return ok
}

func mergeString(envKey string, file FileConfig, fileKey, fileVal, def string) string {
	if _, ok := os.LookupEnv(envKey); ok {
		return os.Getenv(envKey)
	}
	if fileHasKey(file, fileKey) {
		return fileVal
	}
	return def
}

func mergeInt(envKey string, file FileConfig, fileKey string, fileVal, def int) int {
	if _, ok := os.LookupEnv(envKey); ok {
		if n, err := strconv.Atoi(os.Getenv(envKey)); err == nil {
			return n
		}
		return def
	}
	if fileHasKey(file, fileKey) {
		return fileVal
	}
	return def
}

func mergeFloat(envKey string, file FileConfig, fileKey string, fileVal, def float64) float64 {
	if _, ok := os.LookupEnv(envKey); ok {
		if f, err := strconv.ParseFloat(os.Getenv(envKey), 64); err == nil {
			return f
		}
		return def
	}
	if fileHasKey(file, fileKey) {
		return fileVal
	}
	return def
}

func mergeBool(envKey string, file FileConfig, fileKey string, fileVal, def bool) bool {
	if _, ok := os.LookupEnv(envKey); ok {
		v := strings.TrimSpace(strings.ToLower(os.Getenv(envKey)))
		return v == "1" || v == "true"
	}
	if fileHasKey(file, fileKey) {
		return fileVal
	}
	return def
}

package projectfile

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads and parses a project file from disk.
func Load(path string) (*ProjectFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".conf") {
		return ParseConfigFile(path, string(data))
	}
	return ParseContent(path, string(data))
}

// LoadFromWorkingDir discovers and loads project config and guidelines
// separately.  .conf files (pure YAML) take precedence over front-matter
// in .md files.  Guidelines come from whichever .md file has body content.
func LoadFromWorkingDir(workingDir string) (*ProjectFile, error) {
	pf := &ProjectFile{}

	// Load config: .conf first, then front matter from .md as fallback.
	cfgPath, cfgOK := DiscoverConfigPath(workingDir)
	if cfgOK {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(cfgPath, ".conf") {
			cfg, err := parseYAMLConfig(string(data))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", cfgPath, err)
			}
			pf.Config = cfg
			pf.HasConfig = true
		} else {
			// .md with front matter or plain guidelines
			parsed, err := ParseContent(cfgPath, string(data))
			if err != nil {
				return nil, err
			}
			if parsed.HasConfig {
				pf.Config = parsed.Config
				pf.HasConfig = true
			}
			if parsed.Guidelines != "" {
				pf.Guidelines = parsed.Guidelines
				pf.Path = parsed.Path
			}
		}
	}

	// Load guidelines: separate .md file, falling back to body of config .md.
	gPath, gOK := DiscoverGuidelinesPath(workingDir)
	if gOK && (pf.Guidelines == "" || gPath != cfgPath) {
		data, err := os.ReadFile(gPath)
		if err != nil {
			return nil, err
		}
		body := string(data)
		if strings.HasPrefix(strings.TrimRight(body, "\n"), "---") {
			body = extractMarkdownBody(body)
		}
		body = strings.TrimSpace(body)
		if body != "" {
			pf.Guidelines = body
			pf.Path = gPath
		}
	}

	if !pf.HasConfig && pf.Guidelines == "" {
		return nil, nil
	}
	return pf, nil
}

// ParseConfigFile parses a pure-YAML .conf file (no front-matter delimiters).
func ParseConfigFile(path, content string) (*ProjectFile, error) {
	cfg, err := parseYAMLConfig(content)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &ProjectFile{Path: path, HasConfig: true, Config: cfg}, nil
}

// parseYAMLConfig unmarshals YAML into a FileConfig (shared by .conf and front matter).
func parseYAMLConfig(yamlText string) (FileConfig, error) {
	return parseYAMLFrontMatter(yamlText)
}

// ParseContent parses project file content (front matter + body).
func ParseContent(path, content string) (*ProjectFile, error) {
	pf := &ProjectFile{Path: path}
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return pf, nil
	}
	if !strings.HasPrefix(trimmed, "---") {
		pf.Guidelines = trimmed
		return pf, nil
	}
	rest := trimmed[3:]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	} else {
		return nil, fmt.Errorf("%s: front matter must start with --- on line 1 followed by a newline", path)
	}

	closeAt, closeLen, err := findClosingDelimiter(rest)
	if err != nil {
		return nil, err
	}

	yamlText := rest[:closeAt]
	body := strings.TrimLeft(rest[closeAt+closeLen:], "\n")

	cfg, err := parseYAMLFrontMatter(yamlText)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	pf.HasConfig = true
	pf.Config = cfg
	pf.Guidelines = body
	return pf, nil
}

func findClosingDelimiter(s string) (index int, length int, err error) {
	for _, sep := range []string{"\n---\n", "\n---\r\n", "\r\n---\r\n", "\r\n---\n"} {
		if idx := strings.Index(s, sep); idx >= 0 {
			return idx, len(sep), nil
		}
	}
	if idx := strings.LastIndex(s, "\n---"); idx >= 0 && strings.TrimSpace(s[idx:]) == "---" {
		return idx, len(s) - idx, nil
	}
	return 0, 0, fmt.Errorf("front matter opened with --- but no closing --- delimiter found")
}

func parseYAMLFrontMatter(yamlText string) (FileConfig, error) {
	var raw map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlText), &raw); err != nil {
		return FileConfig{}, fmt.Errorf("invalid YAML front matter: %w", err)
	}
	if raw == nil {
		raw = map[string]interface{}{}
	}

	cfg := FileConfig{Keys: make(map[string]struct{}, len(raw))}
	for k := range raw {
		cfg.Keys[k] = struct{}{}
	}

	if v, ok := raw["openai_api_key"]; ok {
		cfg.OpenAIAPIKey = asString(v)
	}
	if v, ok := raw["openai_model"]; ok {
		cfg.OpenAIModel = asString(v)
	}
	if v, ok := raw["openai_base_url"]; ok {
		cfg.OpenAIBaseURL = asString(v)
	}
	if v, ok := raw["working_dir"]; ok {
		cfg.WorkingDir = asString(v)
	}
	if v, ok := raw["context_limit"]; ok {
		n, err := asInt(v)
		if err != nil {
			return FileConfig{}, fmt.Errorf("context_limit: %w", err)
		}
		cfg.ContextLimit = n
	}
	if v, ok := raw["compact_threshold"]; ok {
		f, err := asFloat(v)
		if err != nil {
			return FileConfig{}, fmt.Errorf("compact_threshold: %w", err)
		}
		cfg.CompactThreshold = f
	}
	if v, ok := raw["keep_recent_messages"]; ok {
		n, err := asInt(v)
		if err != nil {
			return FileConfig{}, fmt.Errorf("keep_recent_messages: %w", err)
		}
		cfg.KeepRecentMessages = n
	}
	if v, ok := raw["max_tool_result_bytes"]; ok {
		n, err := asInt(v)
		if err != nil {
			return FileConfig{}, fmt.Errorf("max_tool_result_bytes: %w", err)
		}
		cfg.MaxToolResultBytes = n
	}
	if v, ok := raw["compact_reserve_tokens"]; ok {
		n, err := asInt(v)
		if err != nil {
			return FileConfig{}, fmt.Errorf("compact_reserve_tokens: %w", err)
		}
		cfg.CompactReserveTokens = n
	}
	if v, ok := raw["command_safety"]; ok {
		cfg.CommandSafety = asString(v)
	}
	if v, ok := raw["command_allowlist"]; ok {
		cfg.CommandAllowlist = joinListOrString(v)
	}
	if v, ok := raw["delete_approval"]; ok {
		cfg.DeleteApproval = asString(v)
	}
	if v, ok := raw["treesitter"]; ok {
		cfg.TreeSitter = asOnOffString(v)
	}
	if v, ok := raw["treesitter_langs"]; ok {
		cfg.TreeSitterLangs = joinListOrString(v)
	}
	if v, ok := raw["cli_verbose"]; ok {
		b, err := asBool(v)
		if err != nil {
			return FileConfig{}, fmt.Errorf("cli_verbose: %w", err)
		}
		cfg.CLIVerbose = b
	}
	if v, ok := raw["debug_log"]; ok {
		cfg.DebugLog = asString(v)
	}
	if v, ok := raw["debug_session"]; ok {
		cfg.DebugSession = asString(v)
	}
	if v, ok := raw["mcp"]; ok {
		cfg.MCP = asOnOffString(v)
	}
	if v, ok := raw["mcp_servers"]; ok {
		servers, err := parseMCPServers(v)
		if err != nil {
			return FileConfig{}, err
		}
		cfg.MCPServers = servers
	}
	if v, ok := raw["test_command"]; ok {
		cfg.TestCommand = asString(v)
	}
	if v, ok := raw["lint_command"]; ok {
		cfg.LintCommand = asString(v)
	}
	if v, ok := raw["web_auth_token"]; ok {
		cfg.WebAuthToken = asString(v)
	}
	if v, ok := raw["web_tls_cert_file"]; ok {
		cfg.WebTLSCertFile = asString(v)
	}
	if v, ok := raw["web_tls_key_file"]; ok {
		cfg.WebTLSKeyFile = asString(v)
	}
	if v, ok := raw["session_max_count"]; ok {
		n, err := asInt(v)
		if err == nil {
			cfg.SessionMaxCount = n
		}
	}
	if v, ok := raw["session_max_age_days"]; ok {
		n, err := asInt(v)
		if err == nil {
			cfg.SessionMaxAgeDays = n
		}
	}
	if v, ok := raw["command_sandbox"]; ok {
		cfg.CommandSandbox = asString(v)
	}
	if v, ok := raw["command_timeout_secs"]; ok {
		n, err := asInt(v)
		if err == nil {
			cfg.CommandTimeoutSecs = n
		}
	}
	if v, ok := raw["web_fetch"]; ok {
		cfg.WebFetch = asOnOffString(v)
	}
	if v, ok := raw["web_search"]; ok {
		cfg.WebSearch = asOnOffString(v)
	}
	if v, ok := raw["web_search_backend"]; ok {
		cfg.WebSearchBackend = asString(v)
	}
	if v, ok := raw["web_search_api_key"]; ok {
		cfg.WebSearchAPIKey = asString(v)
	}
	if v, ok := raw["web_allowed_domains"]; ok {
		cfg.WebAllowedDomains = joinListOrString(v)
	}
	if v, ok := raw["web_fetch_mode"]; ok {
		cfg.WebFetchMode = asString(v)
	}

	return cfg, nil
}

func parseMCPServers(v interface{}) ([]MCPServerEntry, error) {
	list, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("mcp_servers must be a list")
	}
	out := make([]MCPServerEntry, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("mcp_servers[%d] must be an object", i)
		}
		entry := MCPServerEntry{Name: asString(m["name"]), Command: asString(m["command"])}
		if entry.Name == "" || entry.Command == "" {
			return nil, fmt.Errorf("mcp_servers[%d] requires name and command", i)
		}
		if args, ok := m["args"]; ok {
			arr, ok := args.([]interface{})
			if !ok {
				return nil, fmt.Errorf("mcp_servers[%d].args must be a list", i)
			}
			for j, a := range arr {
				s, ok := a.(string)
				if !ok {
					return nil, fmt.Errorf("mcp_servers[%d].args[%d] must be a string", i, j)
				}
				entry.Args = append(entry.Args, s)
			}
		}
		if env, ok := m["env"]; ok {
			em, ok := env.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("mcp_servers[%d].env must be an object", i)
			}
			entry.Env = make(map[string]string, len(em))
			for k, val := range em {
				entry.Env[k] = asString(val)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func asString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case int, int64, float64, bool:
		return fmt.Sprint(t)
	default:
		return fmt.Sprint(v)
	}
}

func asInt(v interface{}) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		return int(t), nil
	default:
		return 0, fmt.Errorf("expected integer, got %T", v)
	}
}

func asFloat(v interface{}) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}

func asBool(v interface{}) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "on", "yes":
			return true, nil
		case "false", "0", "off", "no":
			return false, nil
		default:
			return false, fmt.Errorf("invalid boolean %q", t)
		}
	default:
		return false, fmt.Errorf("expected boolean, got %T", v)
	}
}

func asOnOffString(v interface{}) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "on"
		}
		return "off"
	default:
		return strings.ToLower(strings.TrimSpace(asString(v)))
	}
}

func joinListOrString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, strings.TrimSpace(asString(item)))
		}
		return strings.Join(parts, ",")
	default:
		return asString(v)
	}
}

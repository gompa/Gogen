package projectfile

import (
	"errors"
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
	yamlText, body, hasFM, err := splitFrontMatter(content)
	if err != nil {
		if errors.Is(err, errFrontMatterNoNewline) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		// Unclosed front matter: findClosingDelimiter's error is returned
		// without the path prefix, matching the historical message.
		return nil, err
	}
	if !hasFM {
		pf.Guidelines = body
		return pf, nil
	}
	cfg, err := parseYAMLFrontMatter(yamlText)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	pf.HasConfig = true
	pf.Config = cfg
	pf.Guidelines = body
	return pf, nil
}

// errFrontMatterNoNewline is returned by splitFrontMatter when the opening
// --- is not followed by a newline. ParseContent wraps it with the file path
// to match the historical error message.
var errFrontMatterNoNewline = errors.New("front matter must start with --- on line 1 followed by a newline")

// splitFrontMatter splits content into YAML front matter (yamlText) and body.
// hasFM is false when the content has no front matter, in which case body is
// the whole content with trailing newlines stripped. err is non-nil when a
// front matter block is opened but malformed (missing newline after the
// opening ---, or no closing --- delimiter).
func splitFrontMatter(content string) (yamlText, body string, hasFM bool, err error) {
	trimmed := strings.TrimRight(content, "\n")
	if !strings.HasPrefix(trimmed, "---") {
		return "", trimmed, false, nil
	}
	rest := trimmed[3:]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	} else {
		return "", "", true, errFrontMatterNoNewline
	}

	closeAt, closeLen, err := findClosingDelimiter(rest)
	if err != nil {
		return "", "", true, err
	}
	return rest[:closeAt], strings.TrimLeft(rest[closeAt+closeLen:], "\n"), true, nil
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
	var cfg FileConfig
	if err := yaml.Unmarshal([]byte(yamlText), &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid YAML front matter: %w", err)
	}
	if err := validateMCPServers(cfg.MCPServers); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// validateMCPServers enforces the required fields on each MCP server entry.
// Typed YAML decoding handles structure and scalar types; this catches empty
// name/command values, which decode without error.
func validateMCPServers(servers []MCPServerEntry) error {
	for i, s := range servers {
		if s.Name == "" || s.Command == "" {
			return fmt.Errorf("mcp_servers[%d] requires name and command", i)
		}
	}
	return nil
}

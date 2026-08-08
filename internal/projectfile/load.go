package projectfile

import (
	"os"
	"strings"
)

// LoadGlobalConfig loads the global config file (~/.config/gogen/config.yaml).
// Returns nil if the file does not exist or cannot be parsed (non-fatal).
func LoadGlobalConfig() *ProjectFile {
	path := GlobalConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// Global config is pure YAML (same format as .gogen/gogen.conf).
	cfg, err := parseYAMLConfig(string(data))
	if err != nil {
		return nil
	}
	pf := &ProjectFile{Path: path, HasConfig: true, Config: cfg}

	// Load global guidelines from ~/.config/gogen/gogen.md (optional).
	gPath := GlobalGuidelinesPath()
	if gData, gErr := os.ReadFile(gPath); gErr == nil {
		body := strings.TrimSpace(string(gData))
		if body != "" {
			pf.Guidelines = body
		}
	}
	return pf
}

// GuidelinesHeader formats project guidelines for the system prompt.
func GuidelinesHeader(path, guidelines string) string {
	if guidelines == "" {
		return ""
	}
	name := path
	if name == "" {
		name = "project file"
	}
	return "\n\nProject rules (" + name + "):\n" + guidelines
}

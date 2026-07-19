package projectfile

import (
	"os"
	"path/filepath"
	"strings"
)

// Config files (searched in order).  .conf files are pure YAML,
// .md files are parsed for YAML front matter as a fallback.
var configSearchPaths = []string{
	".gogen/gogen.conf",
	"GOGEN.conf",
	".gogen/gogen.md",
	"GOGEN.md",
}

// Guideline files (searched in order).
var guidelineSearchPaths = []string{
	".gogen/gogen.md",
	"GOGEN.md",
	".gogen/rules.md",
	".cursor/rules/gogen.md",
}

// DiscoverConfigPath returns the first config file (any format) under workingDir.
func DiscoverConfigPath(workingDir string) (string, bool) {
	for _, rel := range configSearchPaths {
		path := filepath.Join(workingDir, rel)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == "" {
			continue
		}
		return path, true
	}
	return "", false
}

// DiscoverGuidelinesPath returns the first non-empty guidelines file under workingDir.
func DiscoverGuidelinesPath(workingDir string) (string, bool) {
	for _, rel := range guidelineSearchPaths {
		path := filepath.Join(workingDir, rel)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// .md files with front matter: skip the front matter, check if body exists.
		body := string(data)
		if strings.HasPrefix(strings.TrimRight(body, "\n"), "---") {
			body = extractMarkdownBody(body)
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		return path, true
	}
	return "", false
}

// extractMarkdownBody returns the content after YAML front matter (--- … ---).
// Uses the same closing-delimiter rules as ParseContent/findClosingDelimiter so
// discovery and parsing agree (including files that end with --- and no trailing
// newline). Returns "" if front matter is opened but not closed. Without front
// matter, returns the content with trailing newlines stripped (same as ParseContent).
func extractMarkdownBody(content string) string {
	trimmed := strings.TrimRight(content, "\n")
	if !strings.HasPrefix(trimmed, "---") {
		return trimmed
	}
	rest := trimmed[3:]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	} else {
		return ""
	}
	closeAt, closeLen, err := findClosingDelimiter(rest)
	if err != nil {
		return ""
	}
	return strings.TrimLeft(rest[closeAt+closeLen:], "\n")
}

// DefaultSavePath returns the canonical write paths for --save-config.
func DefaultSavePath(workingDir string) string {
	return filepath.Join(workingDir, ".gogen", "gogen.conf")
}

// DefaultGuidelinesSavePath returns the canonical write path for guidelines.
func DefaultGuidelinesSavePath(workingDir string) string {
	return filepath.Join(workingDir, ".gogen", "gogen.md")
}

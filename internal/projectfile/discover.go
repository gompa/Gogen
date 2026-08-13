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

// discoverFirst returns the first existing, non-empty file among rels under
// workingDir. prepare, when non-nil, transforms the raw content before the
// empty check (e.g. stripping YAML front matter). Shared by
// DiscoverConfigPath and DiscoverGuidelinesPath.
func discoverFirst(workingDir string, rels []string, prepare func(string) string) (string, bool) {
	for _, rel := range rels {
		path := filepath.Join(workingDir, rel)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		body := string(data)
		if prepare != nil {
			body = prepare(body)
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		return path, true
	}
	return "", false
}

// DiscoverConfigPath returns the first config file (any format) under workingDir.
func DiscoverConfigPath(workingDir string) (string, bool) {
	return discoverFirst(workingDir, configSearchPaths, nil)
}

// DiscoverGuidelinesPath returns the first non-empty guidelines file under workingDir.
func DiscoverGuidelinesPath(workingDir string) (string, bool) {
	return discoverFirst(workingDir, guidelineSearchPaths, func(body string) string {
		// .md files with front matter: skip the front matter, check if body exists.
		if strings.HasPrefix(strings.TrimRight(body, "\n"), "---") {
			return extractMarkdownBody(body)
		}
		return body
	})
}

// extractMarkdownBody returns the content after YAML front matter (--- … ---).
// It shares splitFrontMatter with ParseContent so discovery and parsing always
// agree (including files that end with --- and no trailing newline). Returns
// "" if front matter is opened but malformed. Without front matter, returns
// the content with trailing newlines stripped (same as ParseContent).
func extractMarkdownBody(content string) string {
	_, body, _, err := splitFrontMatter(content)
	if err != nil {
		return ""
	}
	return body
}

// DefaultSavePath returns the canonical write paths for --save-config.
func DefaultSavePath(workingDir string) string {
	return filepath.Join(workingDir, ".gogen", "gogen.conf")
}

// DefaultGuidelinesSavePath returns the canonical write path for guidelines.
func DefaultGuidelinesSavePath(workingDir string) string {
	return filepath.Join(workingDir, ".gogen", "gogen.md")
}

// GlobalConfigDir returns the platform-appropriate global config directory
// (e.g. ~/.config/gogen/ on Linux, ~/Library/Application Support/gogen/ on macOS).
func GlobalConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "gogen")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gogen")
}

// GlobalDataDir returns the platform-appropriate global data directory
// (e.g. ~/.local/share/gogen/ on Linux, ~/Library/Application Support/gogen/ on macOS).
func GlobalDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "gogen")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "gogen")
}

// GlobalConfigPath returns the path to the global config YAML file.
func GlobalConfigPath() string {
	return filepath.Join(GlobalConfigDir(), "config.yaml")
}

// GlobalGuidelinesPath returns the path to the global guidelines markdown file.
func GlobalGuidelinesPath() string {
	return filepath.Join(GlobalConfigDir(), "gogen.md")
}

// GlobalSessionDir returns the directory for session snapshots in global mode.
func GlobalSessionDir() string {
	return filepath.Join(GlobalDataDir(), "sessions")
}

// GlobalBoardDir returns the directory for the project board in global mode
// (one board shared across working dirs, mirroring GlobalSessionDir).
func GlobalBoardDir() string {
	return filepath.Join(GlobalDataDir(), "board")
}

// GlobalModelsCachePath returns the path for the models.dev registry cache in global mode.
func GlobalModelsCachePath() string {
	return filepath.Join(GlobalDataDir(), "models.json")
}

// HomeDir returns the user's home directory. Returns "." if it cannot be determined.
func HomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

// IsGlobalModeEnv checks whether GOGEN_MODE environment variable requests global mode.
func IsGlobalModeEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GOGEN_MODE"))) {
	case "global", "1", "true":
		return true
	}
	return false
}

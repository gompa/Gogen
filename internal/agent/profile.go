package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ecosystemMarker struct {
	file    string
	label   string
	testCmd string
	lintCmd string
}

var ecosystemMarkers = []ecosystemMarker{
	{file: "go.mod", label: "Go", testCmd: "go test ./...", lintCmd: "go vet ./..."},
	{file: "package.json", label: "Node.js", testCmd: "npm test", lintCmd: "npm run lint"},
	{file: "pyproject.toml", label: "Python", testCmd: "pytest", lintCmd: "ruff check ."},
	{file: "setup.py", label: "Python", testCmd: "pytest", lintCmd: "ruff check ."},
	{file: "requirements.txt", label: "Python", testCmd: "pytest", lintCmd: ""},
	{file: "Cargo.toml", label: "Rust", testCmd: "cargo test", lintCmd: "cargo clippy"},
	{file: "pom.xml", label: "Java (Maven)", testCmd: "mvn test", lintCmd: ""},
	{file: "build.gradle", label: "Java (Gradle)", testCmd: "./gradlew test", lintCmd: ""},
	{file: "build.gradle.kts", label: "Kotlin (Gradle)", testCmd: "./gradlew test", lintCmd: ""},
	{file: "mix.exs", label: "Elixir", testCmd: "mix test", lintCmd: ""},
	{file: "Gemfile", label: "Ruby", testCmd: "bundle exec rake test", lintCmd: "bundle exec rubocop"},
	{file: "composer.json", label: "PHP", testCmd: "composer test", lintCmd: ""},
	{file: "Makefile", label: "Make", testCmd: "make test", lintCmd: ""},
	{file: "CMakeLists.txt", label: "CMake", testCmd: "ctest", lintCmd: ""},
	{file: "deno.json", label: "Deno", testCmd: "deno test", lintCmd: "deno lint"},
	{file: "deno.jsonc", label: "Deno", testCmd: "deno test", lintCmd: "deno lint"},
}

// DetectProjectProfile returns a compact auto-detected project summary for the system prompt.
func DetectProjectProfile(workingDir, testCmdOverride, lintCmdOverride string) string {
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		abs = workingDir
	}

	var markers []string
	testCmd := strings.TrimSpace(testCmdOverride)
	lintCmd := strings.TrimSpace(lintCmdOverride)
	var ecosystems []string

	for _, m := range ecosystemMarkers {
		if _, err := os.Stat(filepath.Join(abs, m.file)); err != nil {
			continue
		}
		markers = append(markers, m.file)
		if testCmd == "" && m.testCmd != "" {
			testCmd = m.testCmd
		}
		if lintCmd == "" && m.lintCmd != "" {
			lintCmd = m.lintCmd
		}
		ecosystems = append(ecosystems, m.label)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Working directory: %s\n", filepath.ToSlash(abs))
	if len(ecosystems) > 0 {
		b.WriteString("Ecosystem markers: " + strings.Join(markers, ", ") + "\n")
		b.WriteString("Detected stacks: " + strings.Join(ecosystems, ", ") + "\n")
	} else {
		b.WriteString("Ecosystem markers: (none detected)\n")
	}

	if top := topLevelLayout(abs); top != "" {
		b.WriteString(top)
	}
	if testCmd != "" {
		fmt.Fprintf(&b, "Test command: %s\n", testCmd)
	}
	if lintCmd != "" {
		fmt.Fprintf(&b, "Lint command: %s\n", lintCmd)
	}
	if testCmd == "" {
		b.WriteString("Test command: (not detected — set test_command in GOGEN.md or use execute_command)\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

func topLevelLayout(workingDir string) string {
	entries, err := os.ReadDir(workingDir)
	if err != nil {
		return ""
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if shouldSkipSearchEntry(name, true) {
			continue
		}
		dirs = append(dirs, name+"/")
	}
	if len(dirs) == 0 {
		return ""
	}
	sort.Strings(dirs)
	if len(dirs) > 12 {
		dirs = dirs[:12]
		dirs = append(dirs, "…")
	}
	return "Top-level directories: " + strings.Join(dirs, ", ") + "\n"
}

// DetectTestCommand returns the test command from override or ecosystem markers.
func DetectTestCommand(workingDir, override string) string {
	if cmd := strings.TrimSpace(override); cmd != "" {
		return cmd
	}
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		abs = workingDir
	}
	for _, m := range ecosystemMarkers {
		if m.testCmd == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(abs, m.file)); err == nil {
			return m.testCmd
		}
	}
	return ""
}

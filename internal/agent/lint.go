package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunLint runs the project lint command with optional extra arguments.
func (e *Executor) RunLint(ctx context.Context, extraArgs, lintCommandOverride string) (string, error) {
	cmd := DetectLintCommand(e.GetWorkingDir(), lintCommandOverride)
	if cmd == "" {
		return "", fmt.Errorf("no lint command detected; set lint_command in GOGEN.md or use execute_command")
	}

	full := buildLintCommand(cmd, extraArgs)
	out, err := e.ExecuteCommand(ctx, full)
	summary := summarizeLintOutput(out, err)
	if out == "" {
		return summary, err
	}
	return summary + "\n\n" + out, err
}

// DetectLintCommand returns the lint command from override or ecosystem markers.
func DetectLintCommand(workingDir, override string) string {
	if cmd := strings.TrimSpace(override); cmd != "" {
		return cmd
	}
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		abs = workingDir
	}
	for _, m := range ecosystemMarkers {
		if m.lintCmd == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(abs, m.file)); err == nil {
			return m.lintCmd
		}
	}
	return ""
}

func buildLintCommand(base, extra string) string {
	base = strings.TrimSpace(base)
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return base
	}
	return base + " " + extra
}

func summarizeLintOutput(out string, err error) string {
	var b strings.Builder
	if err != nil {
		b.WriteString("Result: issues found (non-zero exit)\n")
	} else {
		b.WriteString("Result: no issues\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

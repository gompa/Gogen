package agent

import (
	"context"
	"fmt"
	"strings"
)

// RunTests runs the project test command with optional target and extra arguments.
func (e *Executor) RunTests(ctx context.Context, target, extraArgs, testCommandOverride string) (string, error) {
	cmd := DetectTestCommand(e.GetWorkingDir(), testCommandOverride)
	if cmd == "" {
		return "", fmt.Errorf("no test command detected; set test_command in GOGEN.md or use execute_command")
	}

	full := buildTestCommand(cmd, target, extraArgs)
	out, err := e.ExecuteCommand(ctx, full)
	summary := summarizeTestOutput(out, err)
	if out == "" {
		return summary, err
	}
	return summary + "\n\n" + out, err
}

func buildTestCommand(base, target, extra string) string {
	base = strings.TrimSpace(base)
	target = strings.TrimSpace(target)
	extra = strings.TrimSpace(extra)

	if target == "" && extra == "" {
		return base
	}

	// Replace ./... with a specific package/path when the default is go test ./...
	if target != "" && strings.Contains(base, "./...") {
		base = strings.Replace(base, "./...", target, 1)
		target = ""
	}

	parts := []string{base}
	if target != "" {
		parts = append(parts, target)
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, " ")
}

func summarizeTestOutput(out string, err error) string {
	var b strings.Builder
	if err != nil {
		b.WriteString("Result: failed (non-zero exit)\n")
	} else {
		b.WriteString("Result: passed\n")
	}

	pass := strings.Count(out, "--- PASS:")
	fail := strings.Count(out, "--- FAIL:")
	if pass+fail > 0 {
		fmt.Fprintf(&b, "Tests: %d passed, %d failed\n", pass, fail)
	}
	if strings.Contains(out, "FAIL") && fail == 0 && pass == 0 {
		b.WriteString("Output contains failures\n")
	}
	if idx := strings.Index(out, "Command:"); idx >= 0 {
		end := strings.IndexByte(out[idx:], '\n')
		if end > 0 {
			b.WriteString(out[idx : idx+end])
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

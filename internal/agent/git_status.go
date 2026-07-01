package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// GitStatus returns a summary of working tree status.
func (e *Executor) GitStatus(ctx context.Context, path string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}

	args := []string{"status", "--short"}
	if strings.TrimSpace(path) != "" {
		if _, err := e.securePath(path); err != nil {
			return "", err
		}
		args = append(args, "--", path)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = e.WorkingDir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 && text != "" {
			return "", fmt.Errorf("git status failed: %s", text)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if text == "" {
			return "", fmt.Errorf("git status failed: %w", err)
		}
		return "", fmt.Errorf("git status failed: %s", text)
	}
	if text == "" {
		return "Working tree clean (no changes)", nil
	}
	return text, nil
}

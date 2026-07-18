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
	args := []string{"status", "--short"}
	if strings.TrimSpace(path) != "" {
		if _, err := e.securePath(path); err != nil {
			return "", err
		}
		args = append(args, "--", path)
	}

	cmd, cmdErr := e.newGitCmd(ctx, args...)
	if cmdErr != nil {
		return "", cmdErr
	}
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

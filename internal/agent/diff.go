package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ShowDiff returns a unified diff for the working tree using git when available.
func (e *Executor) ShowDiff(ctx context.Context, path string, staged bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH; show_diff requires a git repository")
	}

	args := []string{"diff", "--no-color", "--no-ext-diff"}
	if staged {
		args = append(args, "--cached")
	}
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
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			if text == "" {
				return "No differences found", nil
			}
			return text, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git diff failed: %s", msg)
	}
	if text == "" {
		return "No differences found", nil
	}
	return text, nil
}

func runDiffQuick(workingDir, path string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-color", "--no-ext-diff", "--", path)
	cmd.Dir = workingDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", nil
	}
	return text, nil
}

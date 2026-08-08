package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ShowDiff returns a unified diff for the working tree using git when available.
func (e *Executor) ShowDiff(ctx context.Context, path string, staged bool) (string, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff"}
	if staged {
		args = append(args, "--cached")
	}
	if strings.TrimSpace(path) != "" {
		if _, err := e.SecurePath(path); err != nil {
			return "", err
		}
		args = append(args, "--", path)
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		// git diff exits 1 when there are no differences (or --check
		// whitespace findings) — that is not a failure.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			if text == "" {
				return "No differences found", nil
			}
			return text, nil
		}
		// Preserve git's stderr (e.g. "fatal: not a git repository") when
		// the command produced output; otherwise the wrapped error already
		// names the failing subcommand.
		if text != "" {
			return "", fmt.Errorf("git diff failed: %s", text)
		}
		return "", err
	}
	if text == "" {
		return "No differences found", nil
	}
	return text, nil
}

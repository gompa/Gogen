package agent

import (
	"context"
	"strings"
)

// GitStatus returns a summary of working tree status.
func (e *Executor) GitStatus(ctx context.Context, path string) (string, error) {
	args := []string{"status", "--short"}
	if strings.TrimSpace(path) != "" {
		if _, err := e.SecurePath(path); err != nil {
			return "", err
		}
		args = append(args, "--", path)
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("status", text, err)
	}
	if text == "" {
		return "Working tree clean (no changes)", nil
	}
	return text, nil
}

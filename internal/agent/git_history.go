package agent

import (
	"context"
	"strconv"
	"strings"
)

const (
	gitLogDefaultLimit = 20
	gitLogMaxLimit     = 100
)

// GitLog returns recent commit history, optionally scoped to a path.
func (e *Executor) GitLog(ctx context.Context, path string, limit int) (string, error) {
	if limit <= 0 {
		limit = gitLogDefaultLimit
	}
	if limit > gitLogMaxLimit {
		limit = gitLogMaxLimit
	}

	args := []string{"log", "--oneline", "--no-decorate", "-n", strconv.Itoa(limit)}
	if strings.TrimSpace(path) != "" {
		if _, err := e.SecurePath(path); err != nil {
			return "", err
		}
		args = append(args, "--", path)
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("log", text, err)
	}
	if text == "" {
		return "No commits found", nil
	}
	return text, nil
}

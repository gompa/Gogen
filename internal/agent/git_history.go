package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const (
	gitLogDefaultLimit  = 20
	gitLogMaxLimit      = 100
	gitBlameDefaultSpan = 50
	gitBlameMaxSpan     = 200
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

// GitBlame returns line attribution for a file within an optional line range.
func (e *Executor) GitBlame(ctx context.Context, path string, startLine, limit int) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	if _, err := e.SecurePath(path); err != nil {
		return "", err
	}
	if startLine <= 0 {
		startLine = 1
	}
	if limit <= 0 {
		limit = gitBlameDefaultSpan
	}
	if limit > gitBlameMaxSpan {
		limit = gitBlameMaxSpan
	}
	endLine := startLine + limit - 1

	args := []string{
		"blame",
		"--date=short",
		"-L", fmt.Sprintf("%d,%d", startLine, endLine),
		"--", path,
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("blame", text, err)
	}
	if text == "" {
		return "No blame data found", nil
	}
	return text, nil
}

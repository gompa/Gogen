package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}
	if limit <= 0 {
		limit = gitLogDefaultLimit
	}
	if limit > gitLogMaxLimit {
		limit = gitLogMaxLimit
	}

	args := []string{"log", "--oneline", "--no-decorate", "-n", strconv.Itoa(limit)}
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
			return "", fmt.Errorf("git log failed: %s", text)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if text == "" {
			return "", fmt.Errorf("git log failed: %w", err)
		}
		return "", fmt.Errorf("git log failed: %s", text)
	}
	if text == "" {
		return "No commits found", nil
	}
	return text, nil
}

// GitBlame returns line attribution for a file within an optional line range.
func (e *Executor) GitBlame(ctx context.Context, path string, startLine, limit int) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}
	if _, err := e.securePath(path); err != nil {
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

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = e.WorkingDir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if text == "" {
			return "", fmt.Errorf("git blame failed: %w", err)
		}
		return "", fmt.Errorf("git blame failed: %s", text)
	}
	if text == "" {
		return "No blame data found", nil
	}
	return text, nil
}

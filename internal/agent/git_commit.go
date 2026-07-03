package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// GitStage stages files for commit.
// When paths is empty, it uses `git add -A` to stage everything.
func (e *Executor) GitStage(ctx context.Context, paths []string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}

	args := []string{"add"}
	if len(paths) == 0 {
		args = append(args, "-A")
	} else {
		for _, p := range paths {
			if _, err := e.securePath(p); err != nil {
				return "", err
			}
		}
		args = append(args, "--")
		args = append(args, paths...)
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
			return "", fmt.Errorf("git add failed: %w", err)
		}
		return "", fmt.Errorf("git add failed: %s", text)
	}
	return "Staged successfully", nil
}

// GitCommit creates a commit with the given message.
func (e *Executor) GitCommit(ctx context.Context, message string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	cmd := exec.CommandContext(ctx, "git", "commit", "-m", message)
	cmd.Dir = e.WorkingDir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Exit code 1 with "nothing to commit" is not an error.
		if strings.Contains(text, "nothing to commit") {
			return text, nil
		}
		if text == "" {
			return "", fmt.Errorf("git commit failed: %w", err)
		}
		return text, fmt.Errorf("git commit failed: %s", text)
	}
	return text, nil
}

// GitBranch lists branches (showing current), or creates/switches branches when specified.
// When create is true, it runs `git checkout -b name` (or `git switch -c`).
// When name is non-empty and create is false, it checks out that branch.
// When both are empty, it lists all branches with current marked.
func (e *Executor) GitBranch(ctx context.Context, name string, create bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}
	name = strings.TrimSpace(name)

	if name != "" && create {
		cmd := exec.CommandContext(ctx, "git", "switch", "-c", name)
		cmd.Dir = e.WorkingDir
		out, err := cmd.CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if text == "" {
				return "", fmt.Errorf("git switch -c failed: %w", err)
			}
			return "", fmt.Errorf("git switch -c failed: %s", text)
		}
		return fmt.Sprintf("Created and switched to branch %q", name), nil
	}

	if name != "" {
		cmd := exec.CommandContext(ctx, "git", "switch", name)
		cmd.Dir = e.WorkingDir
		out, err := cmd.CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if text == "" {
				return "", fmt.Errorf("git switch failed: %w", err)
			}
			return "", fmt.Errorf("git switch failed: %s", text)
		}
		return fmt.Sprintf("Switched to branch %q", name), nil
	}

	// List branches: local + remote, with current marked.
	args := []string{"branch", "-v", "-a", "--sort=-committerdate"}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = e.WorkingDir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if text == "" {
			return "", fmt.Errorf("git branch failed: %w", err)
		}
		return "", fmt.Errorf("git branch failed: %s", text)
	}
	if text == "" {
		return "No branches found", nil
	}
	return text, nil
}

// GitStash pushes changes to the stash, optionally with a message.
// When pop is true, it pops the most recent stash entry instead.
func (e *Executor) GitStash(ctx context.Context, message string, pop bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}

	if pop {
		cmd := exec.CommandContext(ctx, "git", "stash", "pop")
		cmd.Dir = e.WorkingDir
		out, err := cmd.CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if text == "" {
				return "", fmt.Errorf("git stash pop failed: %w", err)
			}
			// "No stash to pop" is a known state, not an error.
			if strings.Contains(text, "No stash") {
				return text, nil
			}
			return text, fmt.Errorf("git stash pop failed: %s", text)
		}
		return text, nil
	}

	args := []string{"stash", "push"}
	message = strings.TrimSpace(message)
	if message != "" {
		args = append(args, "-m", message)
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
			return "", fmt.Errorf("git stash failed: %w", err)
		}
		// "nothing to stash" is fine.
		if strings.Contains(text, "nothing to stash") {
			return text, nil
		}
		return "", fmt.Errorf("git stash failed: %s", text)
	}
	return text, nil
}

// GitStashList lists all stash entries.
func (e *Executor) GitStashList(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}

	cmd := exec.CommandContext(ctx, "git", "stash", "list")
	cmd.Dir = e.WorkingDir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if text == "" {
			return "", fmt.Errorf("git stash list failed: %w", err)
		}
		return "", fmt.Errorf("git stash list failed: %s", text)
	}
	if text == "" {
		return "No stash entries", nil
	}
	return text, nil
}

// GitDiffShow returns a unified diff for a specific commit or range.
// When range is empty, shows the diff for HEAD.
func (e *Executor) GitDiffShow(ctx context.Context, ref string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not available on PATH")
	}
	ref = strings.TrimSpace(ref)

	args := []string{"show", "--no-color", "--no-ext-diff"}
	if ref != "" {
		args = append(args, ref)
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
			return "", fmt.Errorf("git show failed: %w", err)
		}
		return "", fmt.Errorf("git show failed: %s", text)
	}
	if text == "" {
		return "No diff found", nil
	}
	return text, nil
}

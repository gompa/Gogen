package agent

import (
	"context"
	"fmt"
	"strings"
)

// GitStage stages files for commit.
// When paths is empty, it uses `git add -A` to stage everything.
func (e *Executor) GitStage(ctx context.Context, paths []string) (string, error) {
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

	cmd, cmdErr := e.newGitCmd(ctx, args...)
	if cmdErr != nil {
		return "", cmdErr
	}
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
	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	cmd, cmdErr := e.newGitCmd(ctx, "commit", "-m", message)
	if cmdErr != nil {
		return "", cmdErr
	}
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
	name = strings.TrimSpace(name)
	if err := validateGitBranchName(name); err != nil {
		return "", err
	}

	if name != "" && create {
		cmd, cmdErr := e.newGitCmd(ctx, "switch", "-c", name)
		if cmdErr != nil {
			return "", cmdErr
		}
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
		cmd, cmdErr := e.newGitCmd(ctx, "switch", "--", name)
		if cmdErr != nil {
			return "", cmdErr
		}
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
	cmd, cmdErr := e.newGitCmd(ctx, args...)
	if cmdErr != nil {
		return "", cmdErr
	}
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
	if pop {
		cmd, cmdErr := e.newGitCmd(ctx, "stash", "pop")
		if cmdErr != nil {
			return "", cmdErr
		}
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

	cmd, cmdErr := e.newGitCmd(ctx, args...)
	if cmdErr != nil {
		return "", cmdErr
	}
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
	cmd, cmdErr := e.newGitCmd(ctx, "stash", "list")
	if cmdErr != nil {
		return "", cmdErr
	}
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
	ref = strings.TrimSpace(ref)
	if err := validateGitRef(ref); err != nil {
		return "", err
	}

	// Ref must come before any "--" pathspec separator; after "--" git
	// treats the argument as a path, not a revision/range.
	args := []string{"show", "--no-color", "--no-ext-diff"}
	if ref != "" {
		args = append(args, ref)
	}

	cmd, cmdErr := e.newGitCmd(ctx, args...)
	if cmdErr != nil {
		return "", cmdErr
	}
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

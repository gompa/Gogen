package agent

import (
	"context"
	"fmt"
	"strings"
)

// gitCmdResult holds the output from a git command execution.
type gitCmdResult struct {
	text string
	err  error
}

// runGitCommand executes a git command with common error handling.
// It returns the trimmed text output. On error, it wraps the error with
// the git subcommand name. If ctx is cancelled, it returns the context error.
// Non-empty text is returned alongside the error when the command produces output
// but still fails (e.g. git prints a message then exits non-zero).
func (e *Executor) runGitCommand(ctx context.Context, args []string) (string, error) {
	cmd, err := e.newGitCmd(ctx, args...)
	if err != nil {
		return "", err
	}
	out, cmdErr := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if cmdErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return text, fmt.Errorf("git %s failed: %w", args[0], cmdErr)
	}
	return text, nil
}

// gitError wraps the error message with the git subcommand name when there is output.
func gitError(subcmd, text string, cause error) error {
	if text == "" {
		return fmt.Errorf("git %s failed: %w", subcmd, cause)
	}
	return fmt.Errorf("git %s failed: %s", subcmd, text)
}

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

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("add", text, err)
	}
	return "Staged successfully", nil
}

// GitCommit creates a commit with the given message.
func (e *Executor) GitCommit(ctx context.Context, message string) (string, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	text, err := e.runGitCommand(ctx, []string{"commit", "-m", message})
	if err != nil {
		// Exit code 1 with "nothing to commit" is not an error.
		if strings.Contains(text, "nothing to commit") {
			return text, nil
		}
		return text, gitError("commit", text, err)
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
		if _, err := e.runGitCommand(ctx, []string{"switch", "-c", name}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Created and switched to branch %q", name), nil
	}

	if name != "" {
		if _, err := e.runGitCommand(ctx, []string{"switch", "--", name}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Switched to branch %q", name), nil
	}

	// List branches: local + remote, with current marked.
	args := []string{"branch", "-v", "-a", "--sort=-committerdate"}
	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("branch", text, err)
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
		text, err := e.runGitCommand(ctx, []string{"stash", "pop"})
		if err != nil {
			// "No stash to pop" is a known state, not an error.
			if text != "" && strings.Contains(text, "No stash") {
				return text, nil
			}
			return text, gitError("stash pop", text, err)
		}
		return text, nil
	}

	args := []string{"stash", "push"}
	message = strings.TrimSpace(message)
	if message != "" {
		args = append(args, "-m", message)
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		if text != "" && strings.Contains(text, "nothing to stash") {
			return text, nil
		}
		return text, gitError("stash", text, err)
	}
	return text, nil
}

// GitStashList lists all stash entries.
func (e *Executor) GitStashList(ctx context.Context) (string, error) {
	text, err := e.runGitCommand(ctx, []string{"stash", "list"})
	if err != nil {
		return "", gitError("stash list", text, err)
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

	args := []string{"show", "--no-color", "--no-ext-diff"}
	if ref != "" {
		args = append(args, ref)
	}

	text, err := e.runGitCommand(ctx, args)
	if err != nil {
		return "", gitError("show", text, err)
	}
	if text == "" {
		return "No diff found", nil
	}
	return text, nil
}

package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// rejectLeadingDashArg rejects values that look like CLI flags so they cannot
// be injected into argv of tools like rg or git.
func rejectLeadingDashArg(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s must not start with '-' (got %q)", name, value)
	}
	return nil
}

// validGitRef reports whether ref is a safe commit-ish / range for `git show`.
// Rejects option-looking strings and shell metacharacters.
var validGitRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@~^:+*{}-]*$`)

func validateGitRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	if err := rejectLeadingDashArg("ref", ref); err != nil {
		return err
	}
	if !validGitRef.MatchString(ref) {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	return nil
}

// validGitBranchName matches common branch names; rejects option injection.
var validGitBranchName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func validateGitBranchName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if err := rejectLeadingDashArg("branch", name); err != nil {
		return err
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("invalid branch name %q", name)
	}
	if !validGitBranchName.MatchString(name) {
		return fmt.Errorf("invalid branch name %q", name)
	}
	return nil
}

package agent

import "testing"

func TestRejectLeadingDashArg(t *testing.T) {
	if err := rejectLeadingDashArg("pattern", "--pre=/tmp/x"); err == nil {
		t.Fatal("expected reject")
	}
	if err := rejectLeadingDashArg("pattern", "fmt.Printf"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateGitRef(t *testing.T) {
	if err := validateGitRef("--output=/tmp/x"); err == nil {
		t.Fatal("expected reject")
	}
	if err := validateGitRef("HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := validateGitRef("abc123~1"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateGitBranchName(t *testing.T) {
	if err := validateGitBranchName("--detach"); err == nil {
		t.Fatal("expected reject")
	}
	if err := validateGitBranchName("feature/foo"); err != nil {
		t.Fatal(err)
	}
}

func TestCommandGuardAllowlistBlocksMetacharacters(t *testing.T) {
	g := NewCommandGuard("allowlist", []string{"go", "git"})
	if err := g.Check("go test ./...; rm -rf ~"); err == nil {
		t.Fatal("expected block for shell chaining")
	}
	if err := g.Check("go test ./..."); err != nil {
		t.Fatal(err)
	}
}

func TestCommandGuardBlocksHomeRm(t *testing.T) {
	g := NewCommandGuard("blocklist", nil)
	for _, cmd := range []string{"rm -rf ~", "rm -rf $HOME", "find . -delete", "python3 -c 'print(1)'"} {
		if err := g.Check(cmd); err == nil {
			t.Fatalf("expected block for %q", cmd)
		}
	}
}

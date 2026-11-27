//go:build unix

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stubBin creates an executable stub for each name in a temp dir and returns
// the dir as a PATH entry. The stubs are only resolved (LookPath), never run.
func stubBin(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	return dir
}

// sandboxArg returns the element immediately before the "-c" flag — the shell
// bwrap is told to exec inside the sandbox.
func sandboxArg(args []string) string {
	for i, a := range args {
		if a == "-c" && i > 0 {
			return args[i-1]
		}
	}
	return ""
}

// TestShellNamePrefersBash pins the resolution rule: bash when PATH has it,
// POSIX sh otherwise — including a PATH with neither, which keeps the old
// behavior (the eventual Start reports the same missing-shell error).
func TestShellNamePrefersBash(t *testing.T) {
	tests := []struct {
		name string
		bins []string
		want string
	}{
		{name: "bash and sh on PATH", bins: []string{"bash", "sh"}, want: "bash"},
		{name: "only sh on PATH", bins: []string{"sh"}, want: "sh"},
		{name: "neither on PATH", bins: nil, want: "sh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", stubBin(t, tc.bins...))
			if got := shellName(); got != tc.want {
				t.Fatalf("shellName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSandboxShellFor pins which resolved bash paths survive the bwrap
// namespace: only the trees bwrap bind-mounts.
func TestSandboxShellFor(t *testing.T) {
	tests := []struct {
		name     string
		resolved string
		want     string
	}{
		{name: "usr bin", resolved: "/usr/bin/bash", want: "bash"},
		{name: "bin", resolved: "/bin/bash", want: "bash"},
		{name: "usr local", resolved: "/usr/local/bin/bash", want: "bash"},
		{name: "nix store is not bound", resolved: "/nix/store/abcd/bin/bash", want: "sh"},
		{name: "homebrew is not bound", resolved: "/opt/homebrew/bin/bash", want: "sh"},
		{name: "empty", resolved: "", want: "sh"},
		{name: "relative name is not usable inside", resolved: "bash", want: "sh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandboxShellFor(tc.resolved); got != tc.want {
				t.Fatalf("sandboxShellFor(%q) = %q, want %q", tc.resolved, got, tc.want)
			}
		})
	}
}

// TestBuildShellCommandUsesBash pins the wired path: execute_command resolves
// bash through PATH, keeps argv[0] as the bare name (a command reading $0 sees
// "bash", as it saw "sh" before), and passes the command string through -c.
func TestBuildShellCommandUsesBash(t *testing.T) {
	t.Run("bash preferred", func(t *testing.T) {
		dir := stubBin(t, "bash", "sh")
		t.Setenv("PATH", dir)
		e := NewExecutor(t.TempDir())

		cmd, err := e.buildShellCommand(context.Background(), "echo hi")
		if err != nil {
			t.Fatalf("buildShellCommand: %v", err)
		}
		if cmd.Args[0] != "bash" {
			t.Fatalf("argv[0] = %q, want \"bash\"", cmd.Args[0])
		}
		if want := filepath.Join(dir, "bash"); cmd.Path != want {
			t.Fatalf("cmd.Path = %q, want %q (the PATH resolution)", cmd.Path, want)
		}
		if !slices.Equal(cmd.Args[1:], []string{"-c", "echo hi"}) {
			t.Fatalf("args = %q, want [-c \"echo hi\"]", cmd.Args[1:])
		}
	})

	t.Run("sh fallback", func(t *testing.T) {
		dir := stubBin(t, "sh")
		t.Setenv("PATH", dir)
		e := NewExecutor(t.TempDir())

		cmd, err := e.buildShellCommand(context.Background(), "echo hi")
		if err != nil {
			t.Fatalf("buildShellCommand: %v", err)
		}
		if cmd.Args[0] != "sh" {
			t.Fatalf("argv[0] = %q, want \"sh\"", cmd.Args[0])
		}
		if want := filepath.Join(dir, "sh"); cmd.Path != want {
			t.Fatalf("cmd.Path = %q, want %q (the PATH resolution)", cmd.Path, want)
		}
	})
}

// TestBuildCommandUsesResolvedShell proves the full BuildCommand path (the one
// foreground, background, and persistent-command execution go through) carries
// the resolved shell.
func TestBuildCommandUsesResolvedShell(t *testing.T) {
	dir := stubBin(t, "bash", "sh")
	t.Setenv("PATH", dir)
	work := t.TempDir()
	e := NewExecutor(work)

	cmd, err := e.BuildCommand(context.Background(), "true")
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	if cmd.Args[0] != "bash" {
		t.Fatalf("argv[0] = %q, want \"bash\"", cmd.Args[0])
	}
	if cmd.Dir != work {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, work)
	}
}

// TestExecuteCommandRunsBash is the end-to-end regression for the resolution
// rule: the shell this process actually executes reports itself as bash when
// bash is installed, and a bash-only command line runs. It skips on a machine
// without bash, where sh is the documented behavior.
//
// $0 is the discriminating assertion: it is argv[0], so this fails on ANY host
// before the fix (including one whose /bin/sh is a bash symlink, where a
// bash-syntax command alone would still pass).
func TestExecuteCommandRunsBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed; sh is the documented fallback")
	}

	t.Run("reports the chosen shell", func(t *testing.T) {
		e := NewExecutor(t.TempDir())
		var out strings.Builder
		if err := e.runCommand(context.Background(), `printf '%s' "$0"`, nil, &out, nil); err != nil {
			t.Fatalf("runCommand: %v", err)
		}
		if got := out.String(); got != "bash" {
			t.Fatalf("$0 = %q, want \"bash\" (the command ran under sh)", got)
		}
	})

	t.Run("accepts bash syntax", func(t *testing.T) {
		e := NewExecutor(t.TempDir())
		var out strings.Builder
		const command = `for ((i = 0; i < 2; i++)); do [[ -n abc ]] && printf 'bash-ok'; done`
		if err := e.runCommand(context.Background(), command, nil, &out, nil); err != nil {
			t.Fatalf("runCommand(%q): %v", command, err)
		}
		if got := out.String(); got != "bash-okbash-ok" {
			t.Fatalf("output = %q, want %q", got, "bash-okbash-ok")
		}
	})
}

// TestBuildShellCommandSandboxShell pins the sandbox rule end to end: a bash
// outside the bind mounts must not be passed into the namespace (the exec would
// fail where the old "sh" worked), while a bash under /usr or /bin is used.
func TestBuildShellCommandSandboxShell(t *testing.T) {
	t.Run("bash outside the bind mounts falls back to sh", func(t *testing.T) {
		dir := stubBin(t, "bwrap", "bash", "sh")
		t.Setenv("PATH", dir)
		e := NewExecutor(t.TempDir())
		e.SetSandbox("bwrap")

		cmd, err := e.buildShellCommand(context.Background(), "echo hi")
		if err != nil {
			t.Fatalf("buildShellCommand: %v", err)
		}
		if got := sandboxArg(cmd.Args); got != "sh" {
			t.Fatalf("sandbox shell = %q, want \"sh\" (the stubbed bash lives outside /usr and /bin)", got)
		}
	})

	t.Run("bash under a bind mount is used", func(t *testing.T) {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap is not installed")
		}
		path, err := exec.LookPath("bash")
		if err != nil || sandboxShellFor(path) != "bash" {
			t.Skipf("no bash under /usr or /bin on this machine (resolved %q, %v)", path, err)
		}
		e := NewExecutor(t.TempDir())
		e.SetSandbox("bwrap")

		cmd, err := e.buildShellCommand(context.Background(), "echo hi")
		if err != nil {
			t.Fatalf("buildShellCommand: %v", err)
		}
		if got := sandboxArg(cmd.Args); got != "bash" {
			t.Fatalf("sandbox shell = %q, want \"bash\" (resolved %q)", got, path)
		}
	})
}

//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Windows ships no POSIX shell, so requiring one (git-bash, msys, cygwin)
// on PATH would be a hard install requirement. Instead, command strings are
// executed by the embedded pure-Go shell interpreter (mvdan.cc/sh, BSD-3):
// it implements the shell semantics the agent's commands rely on — pipes,
// redirects, && / || / ;, loops, and the echo/printf/cd/test/read/exit
// builtins — in-process, and external binaries (go, git, node, cmd.exe,
// ...) are resolved via PATH and exec'd natively. Only this file and the
// mvdan.cc/sh dependency are compiled into the Windows binary; Unix keeps
// its system sh.

// runCommand executes a command string through the embedded interpreter and
// returns the final error, mirroring the Unix path's shapes: guard/sandbox
// failures raw, execution failures wrapped with "execution error:".
func (e *Executor) runCommand(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := e.checkCommandConfig(command); err != nil {
		return err
	}
	runner, prog, err := e.newShellRunner(ctx, command, stdin, stdout, stderr)
	if err != nil {
		return err
	}
	if err := runner.Run(ctx, prog); err != nil {
		if ctx.Err() != nil {
			// Cancelled or timed out: ExecuteCommand maps this via ctx.Err()
			// ("command cancelled"/"command timed out"); the error itself is
			// kept for background jobs' exitErr.
			return err
		}
		return fmt.Errorf("execution error: %w", err)
	}
	return nil
}

// launchBackground starts a command detached from the turn and returns the
// writer for feeding its stdin (an in-memory pipe the interpreter reads)
// plus a wait function returning the interpreter's exit error. The command
// guard and sandbox apply exactly as they do to foreground commands.
func (e *Executor) launchBackground(ctx context.Context, command string, stdout, stderr io.Writer) (io.WriteCloser, func() error, error) {
	if err := e.checkCommandConfig(command); err != nil {
		return nil, nil, err
	}
	pr, pw := io.Pipe()
	runner, prog, err := e.newShellRunner(ctx, command, pr, stdout, stderr)
	if err != nil {
		_ = pw.Close()
		return nil, nil, err
	}
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = runner.Run(ctx, prog)
		close(done)
	}()
	wait := func() error {
		<-done
		return runErr
	}
	return pw, wait, nil
}

// checkCommandConfig applies the command guard and sandbox validation that
// BuildCommand performs on Unix (the Windows runner has no *exec.Cmd to
// configure).
func (e *Executor) checkCommandConfig(command string) error {
	if g := e.commandGuard(); g != nil {
		if err := g.Check(command); err != nil {
			return err
		}
	}
	switch strings.ToLower(strings.TrimSpace(e.sandbox())) {
	case "", "off":
		return nil
	case "bwrap":
		return fmt.Errorf("command_sandbox=bwrap is not supported on Windows")
	default:
		return fmt.Errorf("unknown command_sandbox %q (use \"off\" or \"bwrap\")", e.sandbox())
	}
}

// newShellRunner parses the command and builds a configured interpreter
// runner. stdin may be nil (foreground commands: nothing to read).
func (e *Executor) newShellRunner(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) (*interp.Runner, *syntax.File, error) {
	prog, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, nil, fmt.Errorf("command parse error: %w", err)
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	wd := e.GetWorkingDir()
	if wd == "" {
		wd = "."
	}
	runner, err := interp.New(
		interp.Dir(wd),
		interp.StdIO(stdin, stdout, stderr),
		interp.Env(expand.ListEnviron(os.Environ()...)),
		interp.ExecHandler(execHandler),
	)
	if err != nil {
		return nil, nil, err
	}
	return runner, prog, nil
}

// execHandler runs each external command spawned by the interpreter.
// Cancellation kills the whole process tree via taskkill /T /F: os/exec on
// Windows can only terminate the direct child, so pipelines and
// grandchildren would otherwise keep running after a cancel or timeout.
func execHandler(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.NewExitStatus(127)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Path = path
	cmd.Env = execEnvFromShell(hc.Env)
	cmd.Dir = hc.Dir
	cmd.Stdin = hc.Stdin
	cmd.Stdout = hc.Stdout
	cmd.Stderr = hc.Stderr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// taskkill ships with every Windows install; /T kills the tree,
		// /F forces termination.
		return exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
	}
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return interp.NewExitStatus(uint8(ee.ExitCode()))
		}
		return err
	}
	return nil
}

// execEnvFromShell converts the interpreter's environment to exec format,
// mirroring mvdan's internal conversion: exported string variables only.
func execEnvFromShell(env expand.Environ) []string {
	list := make([]string, 0, 64)
	for name, vr := range env.Each {
		if vr.Exported && vr.Kind == expand.String {
			list = append(list, name+"="+vr.String())
		}
	}
	return list
}

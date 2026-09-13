//go:build unix

package agent

import (
	"fmt"
	"os/exec"
	"syscall"
)

// startShellProcess starts the persistent shell over OS pipes with its own
// process group. bash is preferred (richer interactive builtins); POSIX sh is
// the fallback. The shell reads commands from its stdin pipe; stdin is kept
// open for the shell's lifetime.
func startShellProcess(dir string, env []string) (*shellProcess, error) {
	shellPath, args, err := persistentShellExecutable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(shellPath, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("persistent shell stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("persistent shell stdout pipe: %w", err)
	}
	// Merge stderr into stdout so the model sees build/test diagnostics in
	// order. StdoutPipe sets cmd.Stdout to the pipe's write end, so sharing it
	// is safe.
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start persistent shell: %w", err)
	}
	pid := cmd.Process.Pid
	return &shellProcess{
		stdin:  stdin,
		stdout: stdout,
		wait:   cmd.Wait,
		kill: func() {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
				_ = cmd.Process.Kill()
			}
		},
	}, nil
}

// persistentShellExecutable picks bash when available, else sh.
func persistentShellExecutable() (string, []string, error) {
	if path, err := exec.LookPath("bash"); err == nil {
		return path, nil, nil
	}
	if path, err := exec.LookPath("sh"); err == nil {
		return path, nil, nil
	}
	return "", nil, fmt.Errorf("no bash or sh found on PATH")
}

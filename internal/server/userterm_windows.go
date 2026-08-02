//go:build windows

package server

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// openUserPTY spawns the user's shell over pipes (Windows has no usable PTY
// here; interactive full-screen programs will not behave like a terminal).
func openUserPTY(dir string) (ptyHandle, *exec.Cmd, string, error) {
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd.exe"
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Env = withEnv(os.Environ(), "TERM", "xterm-256color")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, "", err
	}
	name := strings.TrimSuffix(filepath.Base(shell), ".exe")
	return &winPTY{in: stdin, out: io.MultiReader(stdout, stderr)}, cmd, name, nil
}

// winPTY adapts the shell's stdio pipes to ptyHandle. Resize is a no-op.
type winPTY struct {
	in  io.WriteCloser
	out io.Reader
}

func (w *winPTY) Read(p []byte) (int, error)     { return w.out.Read(p) }
func (w *winPTY) Write(p []byte) (int, error)    { return w.in.Write(p) }
func (w *winPTY) Close() error                   { return w.in.Close() }
func (w *winPTY) Resize(cols, rows uint16) error { return nil }

// killProcessGroup kills the shell process (no process groups on Windows).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

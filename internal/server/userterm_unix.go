//go:build unix

package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/creack/pty"
)

// openUserPTY spawns the user's shell attached to a real PTY so interactive
// programs (vim, top, prompts) behave like a local terminal.
func openUserPTY(dir string) (ptyHandle, *exec.Cmd, string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Env = withEnv(os.Environ(), "TERM", "xterm-256color")
	// Prefer a full session with controlling terminal so job control works
	// (Ctrl+Z, foreground process groups). Some sandboxes block setsid/
	// setctty — fall back to a plain process group, which still gives us a
	// killable group and a working pty.
	size := &pty.Winsize{Rows: 24, Cols: 80}
	f, err := pty.StartWithAttrs(cmd, size, &syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		cmd = exec.Command(shell)
		cmd.Dir = dir
		cmd.Env = withEnv(os.Environ(), "TERM", "xterm-256color")
		f, err = pty.StartWithAttrs(cmd, size, &syscall.SysProcAttr{Setpgid: true})
		if err != nil {
			return nil, nil, "", err
		}
	}
	return &unixPTY{f: f}, cmd, filepath.Base(shell), nil
}

// unixPTY adapts the pty master to ptyHandle.
type unixPTY struct{ f *os.File }

func (u *unixPTY) Read(p []byte) (int, error)  { return u.f.Read(p) }
func (u *unixPTY) Write(p []byte) (int, error) { return u.f.Write(p) }
func (u *unixPTY) Close() error                { return u.f.Close() }
func (u *unixPTY) Resize(cols, rows uint16) error {
	return pty.Setsize(u.f, &pty.Winsize{Rows: rows, Cols: cols})
}

// killProcessGroup kills the shell and everything it spawned.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

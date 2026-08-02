package server

import (
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ptyHandle is the platform-specific interactive channel for a user shell.
type ptyHandle interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
}

// UserTerminal is an interactive user shell attached to a WebSocket
// connection. It is safe for concurrent use; Write/Resize become no-ops once
// the shell exits or Close is called.
type UserTerminal struct {
	mu     sync.Mutex
	pty    ptyHandle
	cmd    *exec.Cmd
	output func(string) // called from the read goroutine with output chunks
	done   chan struct{}
	code   int
	title  string
	closed bool
}

// Title returns a short display name for the terminal tab (e.g. "bash").
func (u *UserTerminal) Title() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.title
}

// Done is closed when the shell process exits.
func (u *UserTerminal) Done() <-chan struct{} { return u.done }

// ExitCode returns the shell's exit code once Done is closed.
func (u *UserTerminal) ExitCode() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.code
}

// Write sends input (keystrokes) to the shell's stdin.
func (u *UserTerminal) Write(p []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.pty == nil {
		return io.ErrClosedPipe
	}
	_, err := u.pty.Write(p)
	return err
}

// Resize updates the terminal window size reported to the shell.
func (u *UserTerminal) Resize(cols, rows uint16) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.pty == nil {
		return io.ErrClosedPipe
	}
	return u.pty.Resize(cols, rows)
}

// Close kills the shell (process group on unix) and releases the pty.
func (u *UserTerminal) Close() error {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return nil
	}
	u.closed = true
	p := u.pty
	u.pty = nil
	cmd := u.cmd
	u.mu.Unlock()
	if cmd != nil {
		killProcessGroup(cmd)
	}
	if p != nil {
		return p.Close()
	}
	return nil
}

// startUserTerminal spawns an interactive shell in dir, forwarding its output
// to output. The output callback is invoked from a background goroutine and
// must not call back into the UserTerminal.
func startUserTerminal(dir string, output func(string)) (*UserTerminal, error) {
	p, cmd, title, err := openUserPTY(dir)
	if err != nil {
		return nil, err
	}
	u := &UserTerminal{
		pty:    p,
		cmd:    cmd,
		output: output,
		done:   make(chan struct{}),
		title:  title,
	}
	go u.readLoop(p)
	go u.waitLoop(cmd)
	return u, nil
}

func (u *UserTerminal) readLoop(p ptyHandle) {
	buf := make([]byte, 4096)
	for {
		n, err := p.Read(buf)
		if n > 0 && u.output != nil {
			u.output(string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func (u *UserTerminal) waitLoop(cmd *exec.Cmd) {
	code := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	// Give the read goroutine a moment to drain final output (e.g. the shell's
	// own "exit" line) before we sever the master.
	time.Sleep(50 * time.Millisecond)
	u.mu.Lock()
	u.code = code
	u.closed = true
	if u.pty != nil {
		_ = u.pty.Close()
		u.pty = nil
	}
	u.mu.Unlock()
	close(u.done)
}

// withEnv returns env with any existing key replaced by key=val (appended if
// absent), so interactive shells get a usable TERM even when the server was
// started from a service rather than a terminal.
func withEnv(env []string, key, val string) []string {
	prefix := key + "="
	res := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			if !replaced {
				res = append(res, prefix+val)
				replaced = true
			}
			continue
		}
		res = append(res, e)
	}
	if !replaced {
		res = append(res, prefix+val)
	}
	return res
}

// userTermHolder tracks the live user terminal for one WebSocket connection.
// A single terminal per connection matches the "one UI, one shell" model; the
// shell is respawned on request after it exits.
type userTermHolder struct {
	mu sync.Mutex
	ut *UserTerminal
}

func (h *userTermHolder) get() *UserTerminal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ut
}

func (h *userTermHolder) set(ut *UserTerminal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ut = ut
}

// clear removes ut from the holder if it is still the current terminal,
// reporting whether it was removed (false when a newer shell replaced it).
func (h *userTermHolder) clear(ut *UserTerminal) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ut == ut {
		h.ut = nil
		return true
	}
	return false
}

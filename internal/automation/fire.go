package automation

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SessionIDFileEnv is the env var a fired headless run honors to report its
// session id: when set, the `gogen -p` process writes its session id to
// that file right after the id is created. The automation runner passes the
// path to each fired child and links the id into the run history, so
// `gogen automation runs` points at the session the schedule produced.
const SessionIDFileEnv = "GOGEN_SESSION_ID_FILE"

// FireHandle is one started headless run: where its session id will appear
// (empty when the runner cannot know), a child-exited signal, and a channel
// delivering exactly one terminal result.
type FireHandle struct {
	// SessionIDFile is the file the child writes its session id to (the
	// SessionIDFileEnv contract). The scheduler polls it so the run history
	// links the session as early as possible.
	SessionIDFile string
	// Done is closed once the child has exited and the single Result value
	// has been delivered. The session-id poller waits on Done — never on
	// Result, whose one value belongs to the scheduler's fire goroutine
	// alone (the runtime wakes blocked receivers in enqueue order, so a
	// second receiver could only steal it in a freak preemption window;
	// single-ownership makes that impossible and gives the poller a
	// deterministic exit at child exit).
	Done <-chan struct{}
	// Result delivers the terminal outcome exactly once, after the child
	// process exits (or is killed at the timeout).
	Result <-chan FireResult
}

// FireResult is the terminal outcome of one headless run.
type FireResult struct {
	// SessionID is the id the child reported via the session-id file ("" if
	// it never appeared).
	SessionID string
	// Err is nil when the process exited 0; otherwise the failure (non-zero
	// exit, timeout kill, start failure).
	Err error
	// Detail is a short human-readable failure context (stderr tail) for
	// the run history.
	Detail string
}

// FireFunc starts one headless run for the automation. It must return
// quickly (spawn, not run-to-completion) and never block the sweep.
type FireFunc func(a *Automation) (*FireHandle, error)

// DefaultFireTimeout caps one headless run: a hung `-p` process is killed
// after this long and the run is recorded as failed, so a stuck automation
// cannot leak processes forever.
const DefaultFireTimeout = 60 * time.Minute

// stderrTailCap bounds the stderr tail kept as failure detail.
const stderrTailCap = 2048

// ProcessFire fires automations as `gogen -p <prompt>` subprocesses in the
// configured working directory — the literal `-p` path from main.go, in a
// separate process. A runaway or crashing run therefore cannot take the
// host (TUI / web server) down with it, and the child picks up the working
// dir's own project config (.gogen/gogen.conf) exactly like a manual run.
type ProcessFire struct {
	// Binary is the gogen executable to spawn (os.Executable() by default).
	Binary string
	// Timeout caps a single run (0 = DefaultFireTimeout).
	Timeout time.Duration
	// BuildArgs builds the child's argument list (defaults to
	// --dir <working_dir> -p <prompt>). Overridable for tests.
	BuildArgs func(a *Automation) []string
	// ExtraEnv is appended to the child environment (tests).
	ExtraEnv []string

	// idDir is where per-run session-id files live.
	idDir string
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) {
	b.mu.Lock()
	b.v = v
	b.mu.Unlock()
}

func (b *atomicBool) get() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.v
}

// NewProcessFire builds a ProcessFire spawning the current executable.
// idDir is where per-run session-id link files are created ("" = the OS
// temp dir).
func NewProcessFire(idDir string) (*ProcessFire, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve gogen executable: %w", err)
	}
	return &ProcessFire{
		Binary: bin,
		idDir:  idDir,
	}, nil
}

// defaultFireArgs is the standard `-p` invocation: the headless single
// prompt run, working in the automation's directory.
func defaultFireArgs(a *Automation) []string {
	return []string{"--dir", a.WorkingDir, "-p", a.Prompt}
}

// Fire spawns the headless run. The returned handle delivers its result
// asynchronously; Fire itself returns only start errors.
func (f *ProcessFire) Fire(a *Automation) (*FireHandle, error) {
	// Local (never mutate the receiver): Fire is called concurrently by
	// the fire goroutines.
	idDir := f.idDir
	if idDir == "" {
		idDir = os.TempDir()
	}
	if err := os.MkdirAll(idDir, 0o700); err != nil {
		return nil, fmt.Errorf("create automation id dir: %w", err)
	}
	idFile, err := os.CreateTemp(idDir, "sid-*.txt")
	if err != nil {
		return nil, fmt.Errorf("create session-id link file: %w", err)
	}
	idName := idFile.Name()
	idFile.Close()

	build := f.BuildArgs
	if build == nil {
		build = defaultFireArgs
	}
	cmd := exec.Command(f.Binary, build(a)...)
	cmd.Dir = a.WorkingDir
	env := append(os.Environ(), SessionIDFileEnv+"="+idName)
	env = append(env, f.ExtraEnv...)
	cmd.Env = env

	// Cap the stderr tail kept as failure detail (the child's stdout — the
	// model's reply — is discarded: the session transcript is the record).
	var stderr cappedTail
	stderr.cap = stderrTailCap
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		os.Remove(idName)
		return nil, fmt.Errorf("start headless run: %w", err)
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultFireTimeout
	}
	killFlag := &atomicBool{}
	timer := time.AfterFunc(timeout, func() {
		killFlag.set(true)
		_ = cmd.Process.Kill()
	})

	ch := make(chan FireResult, 1)
	done := make(chan struct{})
	go func() {
		// close(done) LAST (defers run LIFO): the single Result value is in
		// the buffer before the child-exited signal fires, so the session-id
		// poller never races fireOne for it.
		defer close(done)
		defer timer.Stop()
		defer os.Remove(idName) // best-effort cleanup of the link file
		werr := cmd.Wait()
		res := FireResult{SessionID: readSessionIDFile(idName)}
		switch {
		case werr == nil:
			// success; detail stays empty
		case killFlag.get():
			res.Err = fmt.Errorf("run exceeded the %s timeout and was killed", timeout)
			res.Detail = res.Err.Error()
		default:
			res.Err = werr
			if tail := stderr.String(); tail != "" {
				res.Detail = tail
			} else {
				res.Detail = werr.Error()
			}
		}
		ch <- res
	}()
	return &FireHandle{SessionIDFile: idName, Done: done, Result: ch}, nil
}

// readSessionIDFile reads the session id the child reported (best-effort).
func readSessionIDFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// cappedTail is an io.Writer keeping the LAST cap bytes of what was written
// (the most recent stderr output is the useful failure detail): once the
// window is full, older bytes are evicted from the FRONT and counted in
// dropped, which String reports ahead of the retained tail.
type cappedTail struct {
	mu      sync.Mutex
	tail    []byte
	cap     int
	dropped int64
}

func (t *cappedTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tail = append(t.tail, p...)
	if len(t.tail) > t.cap {
		// Evict from the front, keeping only the newest cap bytes. copy (not
		// re-slicing) leaves the retained window at the head of the backing
		// array, so repeated overflow doesn't grow it without bound.
		n := len(t.tail) - t.cap
		t.dropped += int64(n)
		copy(t.tail, t.tail[n:])
		t.tail = t.tail[:t.cap]
	}
	return len(p), nil
}

// String returns the retained tail, plus an ellipsis marker announcing how
// many older bytes were dropped.
func (t *cappedTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dropped > 0 {
		return fmt.Sprintf("… (%d more bytes) ", t.dropped) + string(t.tail)
	}
	return string(t.tail)
}

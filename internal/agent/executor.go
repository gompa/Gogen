package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/ioutil"
)

const (
	readFileWarnBytes = 100 * 1024
	readFileMaxLines  = 10000
)

// defaultCommandIdleTimeout is the default maximum time a foreground command
// may run without producing any output before it is killed.
const defaultCommandIdleTimeout = time.Duration(config.DefaultCommandIdleTimeoutSecs) * time.Second

type Executor struct {
	wdMu         sync.RWMutex
	WorkingDir   string // read via GetWorkingDir; write via SetWorkingDir
	PathBoundary string // if non-empty, overrides WorkingDir for SecurePath checks

	// The security/execution settings below are UNEXPORTED and readable or
	// writable ONLY through the accessor/setter pairs below. liveMu guards
	// them for runtime reads and writes (the web settings modal's live
	// toggles); construction-time configuration (setup.go, main.go, tests)
	// goes through the same setters, so no code path can bypass the mutex.
	// Keeping the fields unexported makes the discipline structural: a
	// direct field write that would race a concurrent read cannot compile.
	// CommandGuard is immutable once built, so accessors may return the
	// pointer after unlocking.
	guard          *CommandGuard
	deleteApproval bool
	sandboxMode    string        // off, bwrap
	maxToolOutput  int           // max bytes retained in-memory per command (0 = unbounded)
	cmdIdleTimeout time.Duration // max no-output duration for foreground commands (0 = default)
	liveMu         sync.RWMutex
}

func NewExecutor(wd string) *Executor {
	return NewExecutorWithGuard(wd, nil)
}

func NewExecutorWithGuard(wd string, guard *CommandGuard) *Executor {
	if guard == nil {
		guard = NewCommandGuard("blocklist", nil)
	}
	return &Executor{
		WorkingDir:     wd,
		guard:          guard,
		deleteApproval: true,
		sandboxMode:    "off",
		PathBoundary:   "",
	}
}

// SetCommandGuard replaces the command guard at runtime (web settings modal
// command_safety / command_allowlist). The new guard is built atomically and
// swapped; in-flight checks use the old guard.
func (e *Executor) SetCommandGuard(mode string, allowlist []string) {
	e.liveMu.Lock()
	e.guard = NewCommandGuard(mode, allowlist)
	e.liveMu.Unlock()
}

// commandGuard returns the current command guard (nil-safe).
func (e *Executor) commandGuard() *CommandGuard {
	e.liveMu.RLock()
	defer e.liveMu.RUnlock()
	return e.guard
}

// SetDeleteApproval toggles whether destructive file operations require
// approval (web settings modal delete_approval).
func (e *Executor) SetDeleteApproval(required bool) {
	e.liveMu.Lock()
	e.deleteApproval = required
	e.liveMu.Unlock()
}

// DeleteApprovalRequired reports whether delete approval is currently
// required (runtime-safe read).
func (e *Executor) DeleteApprovalRequired() bool {
	e.liveMu.RLock()
	defer e.liveMu.RUnlock()
	return e.deleteApproval
}

// SetSandbox sets the command sandbox mode (web settings modal
// command_sandbox: off / bwrap).
func (e *Executor) SetSandbox(mode string) {
	e.liveMu.Lock()
	e.sandboxMode = mode
	e.liveMu.Unlock()
}

// sandbox returns the current sandbox mode.
func (e *Executor) sandbox() string {
	e.liveMu.RLock()
	defer e.liveMu.RUnlock()
	return e.sandboxMode
}

// SetMaxToolOutputBytes caps how much combined stdout+stderr a foreground
// execute_command retains in memory (web settings modal
// max_tool_result_bytes; 0 = no cap, per the documented config semantics).
// The live-output sink is unaffected: it keeps streaming every chunk.
func (e *Executor) SetMaxToolOutputBytes(max int) {
	e.liveMu.Lock()
	e.maxToolOutput = max
	e.liveMu.Unlock()
}

// maxToolOutputBytes returns the current per-command output cap (0 = none).
func (e *Executor) maxToolOutputBytes() int {
	e.liveMu.RLock()
	defer e.liveMu.RUnlock()
	return e.maxToolOutput
}

// SetIdleTimeout sets the maximum time a foreground execute_command may run
// without producing any output before it is killed (config
// command_idle_timeout_secs; <= 0 = the default). Any output resets the
// window, so a slow command that keeps printing runs to completion; there
// is no wall-clock cap. Background jobs are unaffected.
func (e *Executor) SetIdleTimeout(timeout time.Duration) {
	e.liveMu.Lock()
	e.cmdIdleTimeout = timeout
	e.liveMu.Unlock()
}

// idleTimeout returns the current idle timeout (0 = default).
func (e *Executor) idleTimeout() time.Duration {
	e.liveMu.RLock()
	defer e.liveMu.RUnlock()
	return e.cmdIdleTimeout
}

// IdleTimeoutDuration returns the current foreground-command idle timeout
// (0 = the built-in default). Runtime-safe read, used by the web settings
// push and tests.
func (e *Executor) IdleTimeoutDuration() time.Duration {
	return e.idleTimeout()
}

// CommandGuardMode returns the current command guard's mode ("blocklist",
// "allowlist", "off"; "" when no guard is set). Runtime-safe read — the web
// settings push and tests use it.
func (e *Executor) CommandGuardMode() string {
	if g := e.commandGuard(); g != nil {
		return g.Mode
	}
	return ""
}

// SandboxMode returns the current sandbox mode ("off", "bwrap").
// Runtime-safe read.
func (e *Executor) SandboxMode() string {
	return e.sandbox()
}

// GetWorkingDir returns the current working directory.
// Safe for concurrent use with SetWorkingDir (e.g. FS browser vs config change).
func (e *Executor) GetWorkingDir() string {
	e.wdMu.RLock()
	defer e.wdMu.RUnlock()
	return e.WorkingDir
}

// SetWorkingDir updates the working directory.
func (e *Executor) SetWorkingDir(dir string) {
	e.wdMu.Lock()
	e.WorkingDir = dir
	e.wdMu.Unlock()
}

// readFileRaw reads the full raw bytes of a file without the headers or
// truncation that ReadFileRange applies. It is intended for consumers that
// need the exact file content (e.g. tree-sitter parsing), where prepended
// "Lines X-Y of Z" headers would corrupt the parse.
func (e *Executor) ReadFileRawBytes(path string) ([]byte, error) {
	secure, err := e.SecurePath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(secure)
}

// ReadFileRange reads a file with optional 1-based line offset and line limit.
// When search is non-empty, semantics change: the function jumps to the first
// regex match, offset sets the context lines before the match (default 10, not
// a starting line number), and limit caps the total lines returned (before +
// match + after); the after-context gets whatever of the limit budget remains.
func (e *Executor) ReadFileRange(path string, offset, limit int, search string, lineNumbers bool) (string, error) {
	secure, header, err := e.validateAndCheckFile(path)
	if err != nil {
		return "", err
	}

	if search != "" {
		return e.readWithRegexSearch(secure, offset, limit, search, lineNumbers, header)
	}
	return e.readWithLineRange(secure, offset, limit, lineNumbers, header)
}

func (e *Executor) validateAndCheckFile(path string) (string, string, error) {
	secure, err := e.SecurePath(path)
	if err != nil {
		return "", "", err
	}

	info, err := os.Stat(secure)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(secure)
		if err != nil {
			return "", "", fmt.Errorf("path is a directory, use list_files to explore contents")
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return "", "", fmt.Errorf("path is a directory containing: %s. Use list_files or read_file with a specific file path", strings.Join(names, ", "))
	}
	if info.Mode().IsRegular() && info.Size() > 0 {
		if f, err := os.Open(secure); err == nil {
			head := make([]byte, 512)
			n, readErr := f.Read(head)
			f.Close()
			if readErr == nil || n > 0 {
				if n > 0 && bytes.IndexByte(head[:n], 0) >= 0 {
					return "", "", fmt.Errorf("this is a binary file (%s). Use read_file with offset/limit on text files only, or use execute_command to inspect binary content", formatByteSize(info.Size()))
				}
			}
		}
	}

	var header strings.Builder
	if info.Size() > readFileWarnBytes {
		fmt.Fprintf(&header, "Warning: file is %s (%d bytes). Use offset/limit to read in chunks.\n", formatByteSize(info.Size()), info.Size())
	}
	return secure, header.String(), nil
}

func (e *Executor) readWithRegexSearch(secure string, offset, limit int, search string, lineNumbers bool, header string) (string, error) {
	re, err := compiledRegex(search)
	if err != nil {
		return "", fmt.Errorf("invalid search pattern: %w", err)
	}
	// Search mode window: before-context + match + after-context. Deterministic
	// precedence: offset > 0 sets the context lines before the match; limit > 0
	// caps the total window (before + match + after), with the after-context
	// getting whatever of the limit budget remains. Defaults to 10 lines on
	// each side when neither is given.
	ctxBefore, ctxAfter := 10, 10
	if limit > 0 {
		ctxBefore = limit / 2
	}
	if offset > 0 {
		ctxBefore = offset
	}
	if limit > 0 {
		if ctxBefore > limit-1 {
			ctxBefore = limit - 1
		}
		if ctxBefore < 0 {
			ctxBefore = 0
		}
		ctxAfter = limit - ctxBefore - 1
		if ctxAfter < 0 {
			ctxAfter = 0
		}
	}

	f, err := os.Open(secure)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var ring []string
	if ctxBefore > 0 {
		ring = make([]string, ctxBefore)
	}
	ringPos := 0
	ringFull := false
	lineNum := 0
	matchLine := 0

	for sc.Scan() {
		lineNum++
		line := sc.Text()
		if re.MatchString(line) {
			matchLine = lineNum
			var before []string
			if ringFull {
				before = append(ring[ringPos:], ring[:ringPos]...)
			} else if ringPos > 0 {
				before = ring[:ringPos]
			}
			after := []string{line}
			for sc.Scan() {
				// Stop once the total window (before + match + after) reaches
				// the limit, or once the after-context budget is filled when
				// no limit is set.
				if limit > 0 && len(before)+len(after) >= limit {
					break
				}
				if len(after) >= ctxAfter+1 {
					break
				}
				lineNum++
				after = append(after, sc.Text())
			}
			// Count the remaining lines for the "of Z lines" header. For
			// large files the drain would read the entire rest of the file
			// just to report a total — defeating the offset/limit design —
			// so it is skipped and the header reports a lower bound.
			totalLines := -1
			if info, err := f.Stat(); err == nil && info.Size() <= searchMaxFileBytes {
				for sc.Scan() {
					lineNum++
				}
				totalLines = lineNum
			}
			if err := sc.Err(); err != nil {
				return "", scannerError(err)
			}

			selected := append(before, after...)
			startLine := matchLine - len(before)
			if startLine < 1 {
				startLine = 1
			}
			body := strings.Join(selected, "\n")
			if lineNumbers {
				body = formatWithLineNumbers(selected, startLine)
			}

			out := ""
			if totalLines < 0 {
				out = fmt.Sprintf("Lines %d-%d of %d+ (matched %q at line %d; file larger than %s, total line count omitted)\n%s",
					startLine, startLine+len(selected)-1, lineNum, search, matchLine,
					formatByteSize(searchMaxFileBytes), body)
			} else {
				out = fmt.Sprintf("Lines %d-%d of %d (matched %q at line %d)\n%s",
					startLine, startLine+len(selected)-1, totalLines, search, matchLine,
					body)
			}
			if header != "" {
				out = header + out
			}
			return out, nil
		}
		if ctxBefore > 0 {
			ring[ringPos] = line
			ringPos = (ringPos + 1) % ctxBefore
			if ringPos == 0 {
				ringFull = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", scannerError(err)
	}
	if lineNum == 0 {
		return "File is empty", nil
	}
	return "", fmt.Errorf("pattern %q not found in file (%d lines)", search, lineNum)
}

// scannerError translates opaque bufio.Scanner failures into actionable
// messages. A single line longer than the scanner buffer cap (10 MB) fails
// the whole scan with bufio.ErrTooLong; report it as a size limit instead of
// the raw scanner error.
func scannerError(err error) error {
	if errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("file contains a line longer than the 10 MB scanner limit; read_file search cannot scan it")
	}
	return err
}

func (e *Executor) readWithLineRange(secure string, offset, limit int, lineNumbers bool, header string) (string, error) {
	start := 1
	if offset > 0 {
		start = offset
	}
	if start < 1 {
		return "", fmt.Errorf("offset must be >= 1")
	}

	effectiveLimit := limit
	if limit > 0 && limit > readFileMaxLines {
		effectiveLimit = readFileMaxLines
	}

	f, err := os.Open(secure)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var selected []string
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum < start {
			continue
		}
		if effectiveLimit > 0 {
			if len(selected) >= effectiveLimit {
				continue
			}
		} else if len(selected) >= readFileMaxLines {
			// No explicit limit: cap at readFileMaxLines regardless of the
			// offset, so "read from line 500 with no limit" cannot return an
			// unbounded (multi-MB) result — the "Lines X-Y of Z" header below
			// tells the caller the read was truncated. The old `offset == 0`
			// guard let offset reads run to EOF.
			continue
		}
		selected = append(selected, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	totalLines := lineNum
	// Offset past the last line (start >= 1, so this also covers empty
	// files, where totalLines == 0): report it instead of returning an
	// empty result that is indistinguishable from an empty file.
	if start > totalLines {
		msg := fmt.Sprintf("File has %d lines; offset %d is past end.", totalLines, start)
		if header != "" {
			return header + msg, nil
		}
		return msg, nil
	}

	end := start + len(selected) - 1
	var hdr strings.Builder
	if header != "" {
		hdr.WriteString(header)
	}
	if offset == 0 && limit == 0 && totalLines > readFileMaxLines {
		hdr.WriteString(fmt.Sprintf("Warning: file has %d lines; showing first %d. Use offset/limit for more.\n", totalLines, readFileMaxLines))
	}

	body := strings.Join(selected, "\n")
	if lineNumbers && len(selected) > 0 {
		body = formatWithLineNumbers(selected, start)
	}
	if len(selected) > 0 && (end < totalLines || start > 1) {
		hdr.WriteString(fmt.Sprintf("Lines %d-%d of %d\n", start, end, totalLines))
	}
	if hdr.Len() > 0 {
		return hdr.String() + body, nil
	}
	return body, nil
}

// formatWithLineNumbers formats lines with right-aligned line numbers.
// startLine is the 1-based line number of the first line in the slice.
func formatWithLineNumbers(lines []string, startLine int) string {
	if len(lines) == 0 {
		return ""
	}
	width := len(fmt.Sprintf("%d", startLine+len(lines)-1))
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%*d: %s\n", width, startLine+i, line)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *Executor) WriteFile(path string, content string) error {
	secure, err := e.SecurePath(path)
	if err != nil {
		return err
	}
	_, err = os.Stat(secure)
	if err == nil {
		return fmt.Errorf("file already exists: %s. Use patch_file or replace_in_file to edit existing files instead of write_file", path)
	}
	if !os.IsNotExist(err) {
		return err
	}
	return ioutil.WriteFileAtomic(secure, []byte(content), defaultFilePerm)
}

// OverwriteFile creates or overwrites a file under the working directory.
func (e *Executor) OverwriteFile(path string, content string) error {
	secure, err := e.SecurePath(path)
	if err != nil {
		return err
	}
	return ioutil.WriteFileAtomic(secure, []byte(content), defaultFilePerm)
}

// NewGitCmd creates a *exec.Cmd for running git subcommands.
// It handles nil ctx normalisation and PATH lookup automatically.
func (e *Executor) NewGitCmd(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := gitPath(); !ok {
		return nil, fmt.Errorf("git is not available on PATH")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = e.GetWorkingDir()
	return cmd, nil
}

func (e *Executor) ExecuteCommand(ctx context.Context, command string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Combined-output writer: accumulates the output returned to the model
	// (bounded by maxToolOutputBytes when configured) while streaming each
	// chunk to the optional ToolOutputSink so frontends can render live
	// terminal output.
	out := newCommandOutputWriter(command, ToolOutputFromContext(ctx), e.maxToolOutputBytes())

	// Idle timeout: a foreground command that produces no output for the
	// configured window is killed. Any output resets the window, so a
	// slow-but-alive command (a long build that keeps printing) runs to
	// completion, while a hung one cannot block the turn forever — there
	// is no wall-clock cap.
	idle := e.idleTimeout()
	if idle <= 0 {
		idle = defaultCommandIdleTimeout
	}
	cmdCtx, cancelIdle := context.WithCancel(ctx)
	defer cancelIdle()
	done := make(chan struct{})
	defer close(done)
	var idleKilled atomic.Bool
	go watchCommandIdle(out, idle, cancelIdle, done, &idleKilled)

	err := e.runCommand(cmdCtx, command, nil, out, out)
	// The output stream is over: runCommand's Wait has closed the pipes,
	// so every chunk has reached the sink. Fire the end callback (the
	// UI's term_exit) BEFORE the caller reports the tool result — the
	// foreground mirror of the background job's exit, which fires from
	// the job's wait goroutine instead.
	if end := ToolOutputEndFromContext(ctx); end != nil {
		end(err == nil)
	}
	outStr := out.String()
	if out.overflowed {
		outStr = e.applyToolOutputCap(outStr)
	}
	if err != nil {
		if idleKilled.Load() {
			return outStr, fmt.Errorf("command idle for %s with no output: %s", idle.Round(time.Second), command)
		}
		if ctx.Err() == context.DeadlineExceeded {
			return outStr, fmt.Errorf("command timed out: %s", command)
		}
		if ctx.Err() == context.Canceled {
			return outStr, fmt.Errorf("command cancelled: %s", command)
		}
		return outStr, err
	}
	return outStr, nil
}

// watchCommandIdle kills the command (via cancel) when it has produced no
// output for the full idle window. It polls the writer's last-activity
// timestamp on a ticker (idle/4, clamped to [250ms, idle]) and exits when
// done is closed (the command finished): a cancel after finish is a no-op,
// and killed is only observed on the error path, so a kill that races the
// command's own exit is harmless.
func watchCommandIdle(out *commandOutputWriter, idle time.Duration, cancel context.CancelFunc, done <-chan struct{}, killed *atomic.Bool) {
	tick := idle / 4
	if tick < 250*time.Millisecond {
		tick = 250 * time.Millisecond
	}
	if tick > idle {
		tick = idle
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if time.Since(out.lastActivity()) >= idle {
				killed.Store(true)
				cancel()
				return
			}
		}
	}
}

// applyToolOutputCap appends the truncation marker when the bounded writer
// dropped output beyond the configured cap. The writer keeps the FIRST cap
// bytes — the same prefix the context manager's tool-result cap preserves —
// so this only reserves room for the marker, mirroring
// contextmgr.truncateToolResult's budget discipline. The marker shares the
// manager's standard prefix, so the later truncation pass sees it and leaves
// the result untouched (no double-marking).
func (e *Executor) applyToolOutputCap(content string) string {
	cap := e.maxToolOutputBytes()
	if cap <= 0 {
		return content
	}
	marker := fmt.Sprintf("\n… truncated (command output exceeds %d bytes)", cap)
	if len(marker) >= cap {
		return contextmgr.TruncateRuneSafe(content, cap)
	}
	return contextmgr.TruncateRuneSafe(content, cap-len(marker)) + marker
}

// runeSafeTailStart returns the byte offset at which a tail capped at max
// bytes of data may start without splitting a UTF-8 rune: the raw cut point
// (len(data)-max) is advanced forward over continuation bytes until it lands
// on a rune boundary. A raw byte cut can split a multi-byte character at the
// start of the shown tail and inject invalid UTF-8 into the tool result —
// the tail-cut mirror of contextmgr.TruncateRuneSafe (which makes head cuts
// rune-safe). data is assumed valid UTF-8 (command output usually is; for
// invalid input the result is never worse than a raw byte cut). Returns 0
// when the data fits within max, or max <= 0 (no cap).
func runeSafeTailStart(data []byte, max int) int {
	if max <= 0 || len(data) <= max {
		return 0
	}
	start := len(data) - max
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	return start
}

// outputBuffer is a mutex-guarded byte buffer shared by the command output
// accumulators. exec copies stdout and stderr to the same writer from
// separate goroutines, so every mutation is serialized.
type outputBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// append adds p to the buffer and, when max > 0, trims it to the last max
// bytes afterwards (so long-running commands cannot grow memory without
// bound). The trim drops from the front on a rune boundary, so the retained
// tail never starts with a split multi-byte character: bytes.Buffer.Next is
// a byte cut, and every status/input result built from this buffer would
// otherwise begin with invalid UTF-8. after, when non-nil, runs while the
// lock is still held so callers can perform side effects (like forwarding a
// chunk to a live-output sink) atomically with the buffer update, preserving
// write ordering.
func (b *outputBuffer) append(p []byte, max int, after func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if max > 0 {
		b.buf.Next(runeSafeTailStart(b.buf.Bytes(), max))
	}
	if after != nil {
		after()
	}
}

// drain returns the accumulated bytes and clears the buffer. Used by the
// background-job unread buffer so action=input can return exactly the output
// produced since the previous read.
func (b *outputBuffer) drain() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	b.buf.Reset()
	return s
}

// string returns the accumulated output.
func (b *outputBuffer) string() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// commandOutputWriter accumulates a command's combined stdout+stderr and
// forwards each chunk to the optional sink as it arrives. Using a single
// writer for both streams keeps the merged order as close as possible to
// the child's write order — the same approach CombinedOutput uses.
//
// When max > 0 the model-facing buffer keeps only the FIRST max bytes (the
// prefix the context manager's tool-result cap preserves) and stops
// growing once full, so a noisy command cannot consume unbounded memory for
// its whole runtime; the sink still receives every chunk, so live terminal
// output is never capped. Write may be called concurrently by exec's
// pipe-copy goroutines, so the buffer and the sink callback are serialized
// by an internal lock to preserve chunk ordering. Sinks must therefore be
// fast and must not call back into the executor.
type commandOutputWriter struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	max        int // 0 = unbounded
	overflowed bool
	command    string
	sink       ToolOutputSink
	// lastActive is the monotonic-free unix-nano timestamp of the most
	// recent output write (the command's start when it has produced no
	// output yet). Updated atomically: Write runs on exec's pipe-copy
	// goroutines while the idle watcher reads it from its own goroutine.
	lastActive atomic.Int64
}

func newCommandOutputWriter(command string, sink ToolOutputSink, max int) *commandOutputWriter {
	w := &commandOutputWriter{command: command, sink: sink, max: max}
	w.lastActive.Store(time.Now().UnixNano())
	return w
}

func (w *commandOutputWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.lastActive.Store(time.Now().UnixNano())
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.max > 0 {
		if room := w.max - w.buf.Len(); room > 0 {
			if len(p) > room {
				w.buf.Write(p[:room])
				w.overflowed = true
			} else {
				w.buf.Write(p)
			}
		} else {
			w.overflowed = true
		}
	} else {
		w.buf.Write(p)
	}
	if w.sink != nil {
		w.sink(w.command, string(p))
	}
	return len(p), nil
}

// lastActivity returns the time of the most recent output write (the
// command's start when it has produced no output yet).
func (w *commandOutputWriter) lastActivity() time.Time {
	return time.Unix(0, w.lastActive.Load())
}

// String returns the accumulated output. Safe to call after Wait returns
// (exec waits for the pipe-copy goroutines before Wait completes).
func (w *commandOutputWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (e *Executor) ReplaceInFile(path string, search string, replace string, replaceAll bool) (string, error) {
	if search == "" {
		return "", fmt.Errorf("search string must not be empty")
	}
	secure, err := e.SecurePath(path)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(secure)
	if err != nil {
		return "", err
	}

	text := string(content)
	count := strings.Count(text, search)
	if count == 0 {
		// Diagnose the miss: report the closest file line so the model can
		// fix its search block (a typo or stale block from a previous read)
		// in one retry instead of guessing.
		return "", notFoundError(text, search)
	}
	if !replaceAll && count > 1 {
		// Unique-match by default (mirrors Claude Code / dsh): replacing the
		// first of N occurrences silently would be a wrong-edit gamble.
		return "", fmt.Errorf("search string appears %d times; set replace_all=true to replace all occurrences, or include more surrounding context so the match is unique", count)
	}

	if replaceAll {
		newContent := strings.ReplaceAll(text, search, replace)
		if err := ioutil.WriteFileAtomic(secure, []byte(newContent), defaultFilePerm); err != nil {
			return "", err
		}
		msg := fmt.Sprintf("File updated successfully (%d occurrences replaced)", count)
		return e.AppendSyntaxCheck(msg, path), nil
	}

	idx := strings.Index(text, search)
	newContent := text[:idx] + replace + text[idx+len(search):]
	if err := ioutil.WriteFileAtomic(secure, []byte(newContent), defaultFilePerm); err != nil {
		return "", err
	}
	msg := "File updated successfully (1 occurrence replaced)"
	return e.AppendSyntaxCheck(msg, path), nil
}

// notFoundError reports that search was not found in text, pointing at the
// closest file line: the first line containing the search block's first
// non-empty line, else the line with the longest common prefix.
func notFoundError(text, search string) error {
	first := ""
	for _, ln := range strings.Split(search, "\n") {
		if strings.TrimSpace(ln) != "" {
			first = ln
			break
		}
	}
	lines := strings.Split(text, "\n")
	if first == "" {
		return fmt.Errorf("search string not found in file")
	}
	for i, ln := range lines {
		if strings.Contains(ln, first) {
			return fmt.Errorf("search string not found in file (closest match at line %d: %q)", i+1, ln)
		}
	}
	bestLine, bestScore := 0, -1
	for i, ln := range lines {
		score := 0
		for score < len(first) && score < len(ln) && first[score] == ln[score] {
			score++
		}
		if score > bestScore {
			bestLine, bestScore = i+1, score
		}
	}
	if bestLine == 0 {
		return fmt.Errorf("search string not found in file")
	}
	return fmt.Errorf("search string not found in file (closest match at line %d: %q)", bestLine, lines[bestLine-1])
}

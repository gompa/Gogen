package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gogen/internal/config"
	"gogen/internal/ioutil"
)

const (
	readFileWarnBytes = 100 * 1024
	readFileMaxLines  = 10000
)

// defaultCommandTimeout is the default maximum duration for command execution.
const defaultCommandTimeout = time.Duration(config.DefaultCommandTimeoutSecs) * time.Second

type Executor struct {
	wdMu                  sync.RWMutex
	WorkingDir            string // read via GetWorkingDir; write via SetWorkingDir
	Commands              *CommandGuard
	RequireDeleteApproval bool
	CommandTimeout        time.Duration // 0 = default 2 minutes
	Sandbox               string        // off, bwrap
	PathBoundary          string        // if non-empty, overrides WorkingDir for SecurePath checks
}

func NewExecutor(wd string) *Executor {
	return NewExecutorWithGuard(wd, nil)
}

func NewExecutorWithGuard(wd string, guard *CommandGuard) *Executor {
	if guard == nil {
		guard = NewCommandGuard("blocklist", nil)
	}
	return &Executor{
		WorkingDir:            wd,
		Commands:              guard,
		RequireDeleteApproval: true,
		CommandTimeout:        defaultCommandTimeout,
		Sandbox:               "off",
		PathBoundary:          "",
	}
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
// regex match, offset is treated as context lines around that match (default 10,
// not a starting line number), and limit caps the total lines returned.
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
	re, err := regexp.Compile(search)
	if err != nil {
		return "", fmt.Errorf("invalid search pattern: %w", err)
	}
	ctx := 10
	if limit > 0 {
		ctx = limit / 2
	}
	if offset > 0 {
		ctx = offset
	}
	if ctx < 1 {
		ctx = 1
	}

	f, err := os.Open(secure)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)

	ring := make([]string, ctx)
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
				lineNum++
				after = append(after, sc.Text())
				if len(after) >= ctx+1 {
					break
				}
			}
			for sc.Scan() {
				lineNum++
			}
			if err := sc.Err(); err != nil {
				return "", err
			}
			totalLines := lineNum

			selected := append(before, after...)
			startLine := matchLine - len(before)
			if startLine < 1 {
				startLine = 1
			}
			body := strings.Join(selected, "\n")
			if lineNumbers {
				body = formatWithLineNumbers(selected, startLine)
			}

			out := fmt.Sprintf("Lines %d-%d of %d (matched %q at line %d)\n%s",
				startLine, startLine+len(selected)-1, totalLines, search, matchLine,
				body)
			if header != "" {
				out = header + out
			}
			return out, nil
		}
		ring[ringPos] = line
		ringPos = (ringPos + 1) % ctx
		if ringPos == 0 {
			ringFull = true
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if lineNum == 0 {
		return "File is empty", nil
	}
	return "", fmt.Errorf("pattern %q not found in file (%d lines)", search, lineNum)
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
	if totalLines == 0 {
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
	timeout := e.CommandTimeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", ctx.Err()
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := e.BuildCommand(ctx, command)
	if err != nil {
		return "", err
	}

	// Combined-output writer: accumulates the full output (returned to the
	// caller exactly as before) while streaming each chunk to the optional
	// ToolOutputSink so frontends can render live terminal output.
	out := newCommandOutputWriter(command, ToolOutputFromContext(ctx))
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("execution error: %w", err)
	}
	err = cmd.Wait()
	outStr := out.String()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return outStr, fmt.Errorf("command timed out after %s: %s", timeout.Round(time.Second), command)
		}
		if ctx.Err() == context.Canceled {
			return outStr, fmt.Errorf("command cancelled: %s", command)
		}
		return outStr, fmt.Errorf("execution error: %w", err)
	}
	return outStr, nil
}

// BuildCommand validates the command against the command guard and returns a
// prepared *exec.Cmd for the given context: shell wrapper, sandbox wrapper
// (bwrap), working directory, and process-group cancellation are configured
// exactly as ExecuteCommand configures them. Background job execution uses
// it so the guard and sandbox rules apply identically to foreground and
// background commands.
func (e *Executor) BuildCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	if e.Commands != nil {
		if err := e.Commands.Check(command); err != nil {
			return nil, err
		}
	}
	cmd, err := e.buildShellCommand(ctx, command)
	if err != nil {
		return nil, err
	}
	configureCancelableCmd(cmd)
	cmd.Dir = e.GetWorkingDir()
	return cmd, nil
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
// bound). after, when non-nil, runs while the lock is still held so callers
// can perform side effects (like forwarding a chunk to a live-output sink)
// atomically with the buffer update, preserving write ordering.
func (b *outputBuffer) append(p []byte, max int, after func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if max > 0 {
		if overflow := b.buf.Len() - max; overflow > 0 {
			b.buf.Next(overflow)
		}
	}
	if after != nil {
		after()
	}
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
// Write may be called concurrently by exec's pipe-copy goroutines, so the
// sink callback is invoked while holding the internal lock to preserve
// chunk ordering. Sinks must therefore be fast and must not call back into
// the executor.
type commandOutputWriter struct {
	outputBuffer
	command string
	sink    ToolOutputSink
}

func newCommandOutputWriter(command string, sink ToolOutputSink) *commandOutputWriter {
	return &commandOutputWriter{command: command, sink: sink}
}

func (w *commandOutputWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.append(p, 0, func() {
		if w.sink != nil {
			w.sink(w.command, string(p))
		}
	})
	return len(p), nil
}

// String returns the accumulated output. Safe to call after Wait returns
// (exec waits for the pipe-copy goroutines before Wait completes).
func (w *commandOutputWriter) String() string {
	return w.string()
}

func (e *Executor) buildShellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	sandbox := strings.ToLower(strings.TrimSpace(e.Sandbox))
	switch sandbox {
	case "", "off":
		return exec.CommandContext(ctx, "sh", "-c", command), nil
	case "bwrap":
		path, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, fmt.Errorf("command_sandbox=bwrap but bwrap not found on PATH: %w", err)
		}
		wd := e.GetWorkingDir()
		if wd == "" {
			wd = "."
		}
		// Resolve symlinks so the --bind/--chdir target matches what the
		// child process will see after its own symlink traversal. Without
		// this, a working directory that is itself a symlink (common on
		// macOS /home -> /Users) gets bind-mounted under one path while
		// the child ends up in the other, so writes/reads don't line up.
		if resolved, err := filepath.EvalSymlinks(wd); err == nil {
			wd = resolved
		}
		// Restrict filesystem to the working directory; keep network and
		// basic devices so builds/tests still work. Not a full container.
		return exec.CommandContext(ctx, path,
			"--die-with-parent",
			"--unshare-pid",
			"--dev", "/dev",
			"--proc", "/proc",
			"--ro-bind", "/usr", "/usr",
			"--ro-bind", "/bin", "/bin",
			"--ro-bind", "/lib", "/lib",
			"--ro-bind-try", "/lib64", "/lib64",
			"--ro-bind-try", "/etc", "/etc",
			"--bind", wd, wd,
			"--chdir", wd,
			"sh", "-c", command,
		), nil
	default:
		return nil, fmt.Errorf("unknown command_sandbox %q (use \"off\" or \"bwrap\")", e.Sandbox)
	}
}

func (e *Executor) ReplaceInFile(path string, search string, replace string, replaceAll bool) (string, error) {
	secure, err := e.SecurePath(path)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(secure)
	if err != nil {
		return "", err
	}

	text := string(content)
	if replaceAll {
		count := strings.Count(text, search)
		if count == 0 {
			return "", fmt.Errorf("search string not found in file")
		}
		newContent := strings.ReplaceAll(text, search, replace)
		if err := ioutil.WriteFileAtomic(secure, []byte(newContent), defaultFilePerm); err != nil {
			return "", err
		}
		msg := fmt.Sprintf("File updated successfully (%d occurrences replaced)", count)
		return e.AppendSyntaxCheck(msg, path), nil
	}

	idx := strings.Index(text, search)
	if idx < 0 {
		return "", fmt.Errorf("search string not found in file")
	}
	newContent := text[:idx] + replace + text[idx+len(search):]
	if err := ioutil.WriteFileAtomic(secure, []byte(newContent), defaultFilePerm); err != nil {
		return "", err
	}
	msg := "File updated successfully (1 occurrence replaced)"
	return e.AppendSyntaxCheck(msg, path), nil
}

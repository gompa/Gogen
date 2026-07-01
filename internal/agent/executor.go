package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	readFileWarnBytes = 100 * 1024
	readFileMaxLines  = 10000
)

type Executor struct {
	WorkingDir            string
	Commands              *CommandGuard
	RequireDeleteApproval bool
}

func NewExecutor(wd string) *Executor {
	return &Executor{
		WorkingDir:            wd,
		Commands:              NewCommandGuard("blocklist", nil),
		RequireDeleteApproval: true,
	}
}

func NewExecutorWithGuard(wd string, guard *CommandGuard) *Executor {
	if guard == nil {
		guard = NewCommandGuard("blocklist", nil)
	}
	return &Executor{
		WorkingDir:            wd,
		Commands:              guard,
		RequireDeleteApproval: true,
	}
}

func (e *Executor) ReadFile(path string) (string, error) {
	return e.ReadFileRange(path, 0, 0, "")
}

// readFileRaw reads the full raw bytes of a file without the headers or
// truncation that ReadFileRange applies. It is intended for consumers that
// need the exact file content (e.g. tree-sitter parsing), where prepended
// "Lines X-Y of Z" headers would corrupt the parse.
func (e *Executor) readFileRaw(path string) ([]byte, error) {
	secure, err := e.securePath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(secure)
}

// ReadFileRange reads a file with optional 1-based line offset and line limit.
// When search is non-empty, offset and limit are applied relative to the first
// matching line (regex match).
func (e *Executor) ReadFileRange(path string, offset, limit int, search string) (string, error) {
	secure, err := e.securePath(path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(secure)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(secure)
		if err != nil {
			return "", fmt.Errorf("path is a directory, use list_files to explore contents")
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return "", fmt.Errorf("path is a directory containing: %s. Use list_files or read_file with a specific file path", strings.Join(names, ", "))
	}
	// Detect symlinks and binary files early so the LLM gets a
	// helpful message instead of a cryptic error or garbage output.
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(secure)
		if err != nil {
			target = "(unknown)"
		}
		return "", fmt.Errorf("path is a symlink → %s", target)
	}
	if info.Mode().IsRegular() && info.Size() > 0 {
		head := make([]byte, 512)
		f, err := os.Open(secure)
		if err == nil {
			n, _ := f.Read(head)
			f.Close()
			if n > 0 && isBinary(head[:n]) {
				return "", fmt.Errorf("this is a binary file (%s). Use read_file with offset/limit on text files only, or use execute_command to inspect binary content", formatByteSize(info.Size()))
			}
		}
	}

	var header strings.Builder
	if info.Size() > readFileWarnBytes {
		fmt.Fprintf(&header, "Warning: file is %s (%d bytes). Use offset/limit to read in chunks.\n", formatByteSize(info.Size()), info.Size())
	}

	// When search is set, read all lines, find the first match, and
	// return a window around it.
	if search != "" {
		f, err := os.Open(secure)
		if err != nil {
			return "", err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
		var allLines []string
		for sc.Scan() {
			allLines = append(allLines, sc.Text())
			if len(allLines) >= readFileMaxLines {
				break
			}
		}
		if err := sc.Err(); err != nil {
			return "", err
		}
		if len(allLines) == 0 {
			return "File is empty", nil
		}
		matchLine := 0
		for i, line := range allLines {
			if matched, _ := regexp.MatchString(search, line); matched {
				matchLine = i + 1
				break
			}
		}
		if matchLine == 0 {
			return "", fmt.Errorf("pattern %q not found in file (%d lines)", search, len(allLines))
		}
		ctx := 10
		if limit > 0 {
			ctx = limit / 2
		}
		if offset > 0 {
			ctx = offset
		}
		start := matchLine - ctx
		if start < 1 {
			start = 1
		}
		end := matchLine + ctx
		if limit > 0 {
			end = start + limit - 1
		}
		if end > len(allLines) {
			end = len(allLines)
		}
		selected := allLines[start-1 : end]
		out := fmt.Sprintf("Lines %d-%d of %d (matched %q at line %d)\n%s",
			start, end, len(allLines), search, matchLine,
			strings.Join(selected, "\n"))
		if header.Len() > 0 {
			out = header.String() + out
		}
		return out, nil
	}

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
		} else if offset == 0 && len(selected) >= readFileMaxLines {
			continue
		}
		selected = append(selected, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	totalLines := lineNum
	if totalLines == 0 {
		if header.Len() > 0 {
			return header.String() + fmt.Sprintf("File has %d lines; offset %d is past end.", totalLines, start), nil
		}
		return fmt.Sprintf("File has %d lines; offset %d is past end.", totalLines, start), nil
	}

	end := start + len(selected) - 1
	if offset == 0 && limit == 0 && totalLines > readFileMaxLines {
		header.WriteString(fmt.Sprintf("Warning: file has %d lines; showing first %d. Use offset/limit for more.\n", totalLines, readFileMaxLines))
	}

	body := strings.Join(selected, "\n")
	if len(selected) > 0 && (end < totalLines || start > 1) {
		header.WriteString(fmt.Sprintf("Lines %d-%d of %d\n", start, end, totalLines))
	}
	if header.Len() > 0 {
		return header.String() + body, nil
	}
	return body, nil
}

func formatByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < len("KMGTPE")-1; v /= unit {
		if div > (1<<63-1)/unit {
			break
		}
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (e *Executor) WriteFile(path string, content string) error {
	secure, err := e.securePath(path)
	if err != nil {
		return err
	}
	return writeFileAtomic(secure, []byte(content), 0o644)
}

func (e *Executor) ExecuteCommand(ctx context.Context, command string) (string, error) {
	if e.Commands != nil {
		if err := e.Commands.Check(command); err != nil {
			return "", err
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := 5 * time.Minute
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

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = e.WorkingDir
	out, err := cmd.CombinedOutput()
	outStr := string(out)
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

func (e *Executor) ReplaceInFile(path string, search string, replace string, replaceAll bool) (string, error) {
	secure, err := e.securePath(path)
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
		if err := writeFileAtomic(secure, []byte(newContent), 0o644); err != nil {
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
	if err := writeFileAtomic(secure, []byte(newContent), 0o644); err != nil {
		return "", err
	}
	msg := "File updated successfully (1 occurrence replaced)"
	return e.AppendSyntaxCheck(msg, path), nil
}

func isBinary(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

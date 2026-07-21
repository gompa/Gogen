package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SearchMatch is one structured hit from SearchCodeMatches (no context lines).
type SearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

const (
	searchMaxMatches      = 200
	searchMaxOutputBytes  = 512 * 1024
	searchMaxFileBytes    = 1_000_000
	searchBinaryProbe     = 8192
	searchMaxContextLines = 20
)

var searchSkipDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"vendor":       {},
	"__pycache__":  {},
	".cursor":      {},
}

// shouldSkipSearchEntry mirrors ripgrep's default filtering: respect hidden
// dotfiles/dotdirs and skip common vendor trees. To search inside a hidden
// directory (e.g. .github), pass it as search_code's path argument.
func shouldSkipSearchEntry(name string, isDir bool) bool {
	if isDir {
		if _, skip := searchSkipDirs[name]; skip {
			return true
		}
	}
	return strings.HasPrefix(name, ".") && name != "."
}

// SearchCodeMatches returns structured matches (context_lines=0) for UI find-in-files.
// truncated is true when the result set hit search caps.
func (e *Executor) SearchCodeMatches(ctx context.Context, pattern, subpath, glob string) (matches []SearchMatch, truncated bool, err error) {
	out, err := e.SearchCode(ctx, pattern, subpath, glob, 0)
	if err != nil {
		return nil, false, err
	}
	currentFile := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "…") || strings.Contains(line, "truncated") {
			truncated = true
			continue
		}
		if strings.HasPrefix(line, "No matches") || strings.HasPrefix(line, "(") {
			continue
		}

		// Try match line first: "lineNum:content" in compact format.
		if num, content, ok := parseCompactMatchLine(line); ok {
			if currentFile != "" {
				matches = append(matches, SearchMatch{Path: currentFile, Line: num, Text: content})
			}
			continue
		}

		// Otherwise treat as a filename line setting the current file context.
		if file, ok := parseSearchFileLine(line); ok {
			currentFile = file
		}
	}
	return matches, truncated, nil
}

// parseSearchFileLine checks if line is a standalone filename (no ':digit:' pattern).
func parseSearchFileLine(line string) (string, bool) {
	// A file line has no ':' followed by digits followed by ':'.
	if _, _, ok := splitSearchLine(line); ok {
		return "", false // It's a structured line, not a bare filename.
	}
	// Bare filename lines don't contain ':digit:' patterns.
	return line, true
}

// parseCompactMatchLine parses "line:content" from compact format.
func parseCompactMatchLine(line string) (num int, content string, ok bool) {
	// Compact format: "lineNum:content" where lineNum is at the start.
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != ':' {
		return 0, "", false
	}
	n, convErr := strconv.Atoi(line[:i])
	if convErr != nil || n < 1 {
		return 0, "", false
	}
	return n, line[i+1:], true
}

// SearchCode finds pattern matches using system rg when available, otherwise a Go fallback.
func (e *Executor) SearchCode(ctx context.Context, pattern, subpath, glob string, contextLines int) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	if err := rejectLeadingDashArg("pattern", pattern); err != nil {
		return "", err
	}
	if glob != "" {
		if err := rejectLeadingDashArg("glob", glob); err != nil {
			return "", err
		}
	}
	if contextLines < 0 {
		return "", fmt.Errorf("context_lines must be non-negative")
	}
	if contextLines > searchMaxContextLines {
		contextLines = searchMaxContextLines
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	if _, err := exec.LookPath("rg"); err == nil {
		out, rgErr := e.searchWithRipgrep(ctx, searchRoot, relPrefix, pattern, glob, contextLines)
		if rgErr == nil {
			return out, nil
		}
	}

	out, goErr := e.searchWithGo(ctx, searchRoot, relPrefix, pattern, glob, contextLines)
	if goErr != nil {
		return "", goErr
	}
	return out, nil
}

func (e *Executor) searchRoot(subpath string) (absRoot, relPrefix string, err error) {
	if strings.TrimSpace(subpath) == "" {
		abs, err := filepath.Abs(e.WorkingDir)
		return abs, "", err
	}
	secure, err := e.SecurePath(subpath)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(secure)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("search path must be a directory: %s", subpath)
	}
	absWD, err := filepath.Abs(e.WorkingDir)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(absWD, secure)
	if err != nil {
		return "", "", err
	}
	return secure, rel, nil
}

func (e *Executor) searchWithRipgrep(ctx context.Context, searchRoot, relPrefix, pattern, glob string, contextLines int) (string, error) {
	args := []string{
		"-n",
		"--no-heading",
		"--color=never",
		"--max-count", fmt.Sprintf("%d", searchMaxMatches),
		"--max-columns", "500",
	}
	if contextLines > 0 {
		args = append(args, "-C", fmt.Sprintf("%d", contextLines))
	}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	// "--" prevents patterns like --pre=… from being treated as rg flags.
	args = append(args, "--", pattern, ".")

	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = searchRoot
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			if text != "" {
				return formatSearchOutput("rg", relPrefix, text), ctx.Err()
			}
			return "", ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && text == "" {
			return "No matches found", nil
		}
		if text != "" {
			return formatSearchOutput("rg", relPrefix, text), nil
		}
		return "", fmt.Errorf("rg failed: %w", err)
	}
	if text == "" {
		return "No matches found", nil
	}
	return formatSearchOutput("rg", relPrefix, text), nil
}

func prefixRelPaths(body, relPrefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			b.WriteByte('\n')
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			b.WriteString(filepath.ToSlash(filepath.Join(relPrefix, line)))
		} else {
			b.WriteString(filepath.ToSlash(filepath.Join(relPrefix, line[:idx])))
			b.WriteString(line[idx:])
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (e *Executor) searchWithGo(ctx context.Context, searchRoot, relPrefix, pattern, glob string, contextLines int) (string, error) {
	re, err := compileSearchPattern(pattern)
	if err != nil {
		return "", err
	}
	glob = strings.TrimSpace(glob)

	var matches []string
	var size int
	truncated := false

	err = filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if d.IsDir() {
			if shouldSkipSearchEntry(name, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipSearchEntry(name, false) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > searchMaxFileBytes {
			return nil
		}
		if isBinaryFile(path) {
			return nil
		}

		rel, err := filepath.Rel(searchRoot, path)
		if err != nil {
			return nil
		}
		if relPrefix != "" {
			rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
		} else {
			rel = filepath.ToSlash(rel)
		}
		if glob != "" && !matchGlobPattern(glob, rel) {
			return nil
		}

		limit := searchMaxMatches - len(matches)
		if limit <= 0 {
			return nil
		}
		fileMatches, err := scanFileSinglePass(path, rel, re, contextLines, limit)
		if err != nil {
			return nil
		}
		for _, line := range fileMatches {
			if len(matches) >= searchMaxMatches {
				truncated = true
				return nil
			}
			if size+len(line)+1 > searchMaxOutputBytes {
				truncated = true
				return nil
			}
			matches = append(matches, line)
			size += len(line) + 1
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "No matches found (go fallback; install ripgrep for faster search)", nil
	}
	out := formatSearchOutput("go", relPrefix, strings.Join(matches, "\n"))
	if truncated {
		out += fmt.Sprintf("\n… truncated (showing first %d matches)", len(matches))
	}
	return out, nil
}

func compileSearchPattern(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		re, err = regexp.Compile(regexp.QuoteMeta(pattern))
		if err != nil {
			return nil, fmt.Errorf("invalid search pattern: %w", err)
		}
	}
	return re, nil
}

// scanFileSinglePass reads the file once, finds matches, and emits results
// with context lines. Replaces the prior two-pass approach (findMatchLineNums
// + fetchMatchedLines) to halve file I/O.
func scanFileSinglePass(path, relPath string, re *regexp.Regexp, contextLines, matchLimit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), searchMaxFileBytes)
	var lines []string
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Find matching line numbers.
	matchNums := make([]int, 0, matchLimit)
	for i, line := range lines {
		if re.MatchString(line) {
			matchNums = append(matchNums, i+1)
			if len(matchNums) >= matchLimit {
				break
			}
		}
	}
	if len(matchNums) == 0 {
		return nil, nil
	}

	// Build output set: which lines to emit and with what separator.
	want := make(map[int]byte, len(matchNums)*(contextLines*2+1))
	for _, n := range matchNums {
		if contextLines <= 0 {
			want[n] = ':'
			continue
		}
		start := n - contextLines
		if start < 1 {
			start = 1
		}
		for i := start; i <= n+contextLines; i++ {
			sep := byte('-')
			if i == n {
				sep = ':'
			}
			if _, ok := want[i]; !ok || sep == ':' {
				want[i] = sep
			}
		}
	}

	var out []string
	for lineNum = 1; lineNum <= len(lines); lineNum++ {
		sep, ok := want[lineNum]
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s%c%d%c%s", relPath, sep, lineNum, sep, lines[lineNum-1]))
	}
	return out, nil
}

// binaryProbePool reuses 8KB buffers for binary-file detection to avoid
// allocating a new buffer on every file walked during search.
var binaryProbePool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, searchBinaryProbe)
		return &b
	},
}

func isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	bp := binaryProbePool.Get().(*[]byte)
	defer binaryProbePool.Put(bp)
	buf := *bp
	n, _ := f.Read(buf)
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return true
		}
	}
	return false
}

// compactSearchOutput rewrites lines of the form "filepath:line:content" or
// "filepath-line-content" so that the filepath appears on its own line only
// when it changes, and subsequent lines for the same file show just
// "line:content" (or "line-content" for context lines).  Separator lines
// like "--" are passed through unchanged.
func compactSearchOutput(body string) string {
	var b strings.Builder
	prevFile := ""
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			b.WriteByte('\n')
			continue
		}

		// Separator lines from ripgrep context output (e.g. "--").
		if line == "--" {
			b.WriteString("--\n")
			prevFile = ""
			continue
		}

		// Try to split off the leading filepath.
		// Matched lines use ':', context lines use '-'.
		// We look for "filepathSepNumSepRest" where sep is ':' or '-'.
		file, rest, ok := splitSearchLine(line)
		if !ok {
			// Not a structured line (e.g. truncation notice); pass through.
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}

		if file != prevFile {
			b.WriteString(file)
			b.WriteByte('\n')
			prevFile = file
		}
		b.WriteString(rest)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// splitSearchLine splits "filepathSepNumSepRest" into (filepath, "numSepRest", true).
// The separator sep is either ':' (matched line) or '-' (context line).
func splitSearchLine(line string) (file, rest string, ok bool) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c != ':' && c != '-' {
			continue
		}
		j := i + 1
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j > i+1 && j < len(line) && (line[j] == ':' || line[j] == '-') {
			return line[:i], line[i+1:], true
		}
	}
	return "", "", false
}

func formatSearchOutput(engine, relPrefix, body string) string {
	if engine == "rg" {
		body = prefixRelPaths(body, relPrefix)
	}
	body = compactSearchOutput(body)
	if engine == "go" {
		return body + "\n(search: go fallback; install ripgrep for faster search)"
	}
	return body
}

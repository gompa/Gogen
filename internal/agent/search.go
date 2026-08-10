package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gogen/internal/ioutil"
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

// searchTimeout bounds a single SearchCode call.
const searchTimeout = 30 * time.Second

// replaceTreeTimeout bounds a ReplaceInTree call, which may scan many files
// and is given a longer budget than a plain search.
const replaceTreeTimeout = 60 * time.Second

// gitLsFilesTimeout bounds the git ls-files call inside search scoping.
const gitLsFilesTimeout = 15 * time.Second

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
	if err := validateSearchArgs(pattern, glob); err != nil {
		return nil, false, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	searchRoot, relPrefix, glob, err := e.resolveSearchScope(subpath, glob)
	if err != nil {
		return nil, false, err
	}
	matches, truncated, _, err = e.searchStructured(ctx, searchRoot, relPrefix, pattern, glob)
	return matches, truncated, err
}

// validateSearchArgs checks pattern and glob for the common search entry
// points (SearchCode, SearchCodeMatches).
func validateSearchArgs(pattern, glob string) error {
	if pattern == "" {
		return fmt.Errorf("pattern is required")
	}
	if err := rejectLeadingDashArg("pattern", pattern); err != nil {
		return err
	}
	if glob != "" {
		if err := rejectLeadingDashArg("glob", glob); err != nil {
			return err
		}
	}
	return nil
}

// resolveSearchScope applies the single-file subpath shortcut (searching a
// file scopes the root to its directory and the glob to its basename) and
// returns the search root plus the workspace-relative prefix.
func (e *Executor) resolveSearchScope(subpath, glob string) (searchRoot, relPrefix, effectiveGlob string, err error) {
	effectiveGlob = glob
	if strings.TrimSpace(subpath) != "" {
		secure, err := e.SecurePath(subpath)
		if err == nil {
			info, err := os.Stat(secure)
			if err == nil && !info.IsDir() {
				// Use the file's directory as the search root and the
				// filename as an implicit glob (unless one is already set).
				if effectiveGlob == "" {
					effectiveGlob = filepath.Base(secure)
				}
				subpath = filepath.Dir(secure)
			}
		}
	}
	searchRoot, relPrefix, err = e.searchRoot(subpath)
	return searchRoot, relPrefix, effectiveGlob, err
}

// SearchCode finds pattern matches using system rg when available, otherwise a Go fallback.
func (e *Executor) SearchCode(ctx context.Context, pattern, subpath, glob string, contextLines int) (string, error) {
	if err := validateSearchArgs(pattern, glob); err != nil {
		return "", err
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
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	searchRoot, relPrefix, glob, err := e.resolveSearchScope(subpath, glob)
	if err != nil {
		return "", err
	}

	// Context-free searches go through the structured path shared with
	// SearchCodeMatches; context-lines mode keeps the raw-output formatter.
	if contextLines == 0 {
		matches, truncated, engine, sErr := e.searchStructured(ctx, searchRoot, relPrefix, pattern, glob)
		if sErr != nil {
			return "", sErr
		}
		if len(matches) == 0 {
			if engine == "go" {
				return "No matches found (go fallback; install ripgrep for faster search)", nil
			}
			return "No matches found", nil
		}
		out := formatSearchMatches(matches)
		if engine == "go" {
			out += "\n(search: go fallback; install ripgrep for faster search)"
		}
		if truncated {
			out += fmt.Sprintf("\n… truncated (showing first %d matches)", len(matches))
		}
		return out, nil
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

// ReplaceInTree replaces every regex match of pattern with replacement under
// subpath (same pattern semantics as SearchCode). Unlike search, it is not
// capped by searchMaxMatches — every matching text file is updated.
func (e *Executor) ReplaceInTree(ctx context.Context, pattern, replacement, subpath, glob string) (replaced, fileCount int, err error) {
	if pattern == "" {
		return 0, 0, fmt.Errorf("search pattern is required")
	}
	if err := rejectLeadingDashArg("pattern", pattern); err != nil {
		return 0, 0, err
	}
	if glob != "" {
		if err := rejectLeadingDashArg("glob", glob); err != nil {
			return 0, 0, err
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, replaceTreeTimeout)
	defer cancel()

	re, err := compileSearchPattern(pattern)
	if err != nil {
		return 0, 0, err
	}

	// Single-file scope: replace only in that file.
	if strings.TrimSpace(subpath) != "" {
		secure, secErr := e.SecurePath(subpath)
		if secErr == nil {
			info, statErr := os.Stat(secure)
			if statErr == nil && !info.IsDir() {
				absWD, absErr := filepath.Abs(e.GetWorkingDir())
				if absErr != nil {
					return 0, 0, absErr
				}
				rel, relErr := filepath.Rel(absWD, secure)
				if relErr != nil {
					rel = subpath
				}
				rel = filepath.ToSlash(rel)
				n, writeErr := e.replaceInFilePath(secure, rel, re, replacement)
				if writeErr != nil {
					return 0, 0, writeErr
				}
				if n > 0 {
					return n, 1, nil
				}
				return 0, 0, nil
			}
		}
	}

	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return 0, 0, err
	}
	glob = strings.TrimSpace(glob)

	err = walkTree(ctx, searchRoot, relPrefix, walkOpts{glob: glob, checkReadable: true}, func(path, rel string, d os.DirEntry) error {
		n, writeErr := e.replaceInFilePath(path, rel, re, replacement)
		if writeErr != nil {
			return writeErr
		}
		if n > 0 {
			replaced += n
			fileCount++
		}
		return nil
	})
	return replaced, fileCount, err
}

func (e *Executor) replaceInFilePath(absPath, relPath string, re *regexp.Regexp, replacement string) (int, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return 0, fmt.Errorf("failed to read %s: %w", relPath, err)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return 0, nil
	}
	content := string(data)
	locs := re.FindAllStringIndex(content, -1)
	if len(locs) == 0 {
		return 0, nil
	}
	newContent := re.ReplaceAllString(content, replacement)
	if newContent == content {
		return 0, nil
	}
	if err := ioutil.WriteFileAtomic(absPath, []byte(newContent), defaultFilePerm); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", relPath, err)
	}
	return len(locs), nil
}

func (e *Executor) searchRoot(subpath string) (absRoot, relPrefix string, err error) {
	if strings.TrimSpace(subpath) == "" {
		abs, err := filepath.Abs(e.GetWorkingDir())
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
	absWD, err := filepath.Abs(e.GetWorkingDir())
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(absWD, secure)
	if err != nil {
		return "", "", err
	}
	return secure, rel, nil
}

// runRipgrep runs rg with the standard search arguments and returns the
// trimmed combined output. A non-zero exit (including rg's exit code 1 for
// "no matches") is returned as an *exec.ExitError; callers distinguish the
// no-match case via errors.As.
func runRipgrep(ctx context.Context, searchRoot, pattern, glob string, contextLines int) (string, error) {
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
	return strings.TrimSpace(string(out)), err
}

func (e *Executor) searchWithRipgrep(ctx context.Context, searchRoot, relPrefix, pattern, glob string, contextLines int) (string, error) {
	text, err := runRipgrep(ctx, searchRoot, pattern, glob, contextLines)
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

// searchWithRipgrepMatches runs ripgrep and returns structured matches
// (context_lines=0 semantics). Nil matches mean "no matches found"; an error
// means the caller should fall back to the Go walker.
func (e *Executor) searchWithRipgrepMatches(ctx context.Context, searchRoot, relPrefix, pattern, glob string) ([]SearchMatch, error) {
	text, err := runRipgrep(ctx, searchRoot, pattern, glob, 0)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && text == "" {
			return nil, nil // no matches
		}
		if text != "" {
			return parseRgMatches(text, relPrefix), nil
		}
		return nil, fmt.Errorf("rg failed: %w", err)
	}
	if text == "" {
		return nil, nil
	}
	return parseRgMatches(text, relPrefix), nil
}

// parseRgMatches converts rg's "path:line:content" output into structured
// matches, applying relPrefix to each path.
func parseRgMatches(text, relPrefix string) []SearchMatch {
	var matches []SearchMatch
	for _, line := range strings.Split(text, "\n") {
		path, lineNum, content, ok := splitSearchMatchLine(line)
		if !ok {
			continue
		}
		if relPrefix != "" {
			path = filepath.ToSlash(filepath.Join(relPrefix, path))
		}
		matches = append(matches, SearchMatch{Path: path, Line: lineNum, Text: content})
	}
	return matches
}

// splitSearchLineParts splits ripgrep's "filepathSepNumSepRest" output into
// its parts. sep1 and sep2 are the separator characters: ':' for matched
// lines and '-' for context lines. ok is false when the line does not match
// the "path + separator + digits + separator" shape. Both splitSearchLine
// and splitSearchMatchLine are derived from this single parser so the line
// format is only parsed in one place.
func splitSearchLineParts(line string) (file string, sep1 byte, num string, sep2 byte, rest string, ok bool) {
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
			return line[:i], c, line[i+1 : j], line[j], line[j+1:], true
		}
	}
	return "", 0, "", 0, "", false
}

// splitSearchMatchLine splits rg output "path:line:content" into its parts.
// The separator is ':' — context-free output has no '-' separators.
func splitSearchMatchLine(line string) (path string, lineNum int, content string, ok bool) {
	file, sep1, num, sep2, rest, ok := splitSearchLineParts(line)
	if !ok || sep1 != ':' || sep2 != ':' {
		return "", 0, "", false
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return "", 0, "", false
	}
	return file, n, rest, true
}

// searchStructured runs the search through ripgrep when available, else the
// Go fallback, and returns structured matches (context_lines=0 semantics).
// engine is "rg" or "go" so callers can reproduce the per-engine output
// footer SearchCode shows.
func (e *Executor) searchStructured(ctx context.Context, searchRoot, relPrefix, pattern, glob string) (matches []SearchMatch, truncated bool, engine string, err error) {
	if _, err := exec.LookPath("rg"); err == nil {
		ms, rgErr := e.searchWithRipgrepMatches(ctx, searchRoot, relPrefix, pattern, glob)
		if rgErr == nil {
			return ms, false, "rg", nil
		}
		// Real rg failure — fall back to the Go walker (matches SearchCode).
	}
	ms, truncated, err := e.searchWithGoMatches(ctx, searchRoot, relPrefix, pattern, glob)
	return ms, truncated, "go", err
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

// errSearchWalkStop stops walkSearchableFiles early once the match/output
// caps are reached. It is swallowed by the walker, so callers only see it as
// a nil error.
var errSearchWalkStop = errors.New("search walk stopped")

// walkSearchableFiles walks searchRoot with the Go fallback semantics (glob
// filter, readable-file probe) and calls visit for each searchable file. The
// pattern is compiled once and the glob trimmed once by the walker, so the
// formatted and structured search paths cannot drift on setup. visit returns
// true to stop the walk (used once the match/output caps are reached); the
// returned error is nil in that case.
func walkSearchableFiles(ctx context.Context, searchRoot, relPrefix, pattern, glob string, visit func(path, rel string, re *regexp.Regexp) (stop bool)) error {
	re, err := compileSearchPattern(pattern)
	if err != nil {
		return err
	}
	glob = strings.TrimSpace(glob)

	err = walkTree(ctx, searchRoot, relPrefix, walkOpts{glob: glob, checkReadable: true}, func(path, rel string, d os.DirEntry) error {
		if visit(path, rel, re) {
			return errSearchWalkStop
		}
		return nil
	})
	if errors.Is(err, errSearchWalkStop) {
		return nil
	}
	return err
}

func (e *Executor) searchWithGo(ctx context.Context, searchRoot, relPrefix, pattern, glob string, contextLines int) (string, error) {
	var matches []string
	var size int
	truncated := false

	err := walkSearchableFiles(ctx, searchRoot, relPrefix, pattern, glob, func(path, rel string, re *regexp.Regexp) bool {
		limit := searchMaxMatches - len(matches)
		if limit <= 0 {
			return false
		}
		reader, closer, binary, err := openSearchableFile(path)
		if err != nil {
			return false
		}
		defer closer.Close()
		if binary {
			return false
		}
		fileMatches, err := scanFileSinglePass(reader, rel, re, contextLines, limit)
		if err != nil {
			return false
		}
		for _, line := range fileMatches {
			if len(matches) >= searchMaxMatches {
				truncated = true
				return true
			}
			if size+len(line)+1 > searchMaxOutputBytes {
				truncated = true
				return true
			}
			matches = append(matches, line)
			size += len(line) + 1
		}
		return false
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

// searchWithGoMatches runs the Go fallback walker and returns structured
// matches (context_lines=0 semantics) plus the truncation flag. Paths are
// already workspace-relative with relPrefix applied by walkTree.
func (e *Executor) searchWithGoMatches(ctx context.Context, searchRoot, relPrefix, pattern, glob string) ([]SearchMatch, bool, error) {
	var matches []SearchMatch
	var size int
	truncated := false

	err := walkSearchableFiles(ctx, searchRoot, relPrefix, pattern, glob, func(path, rel string, re *regexp.Regexp) bool {
		limit := searchMaxMatches - len(matches)
		if limit <= 0 {
			return false
		}
		reader, closer, binary, err := openSearchableFile(path)
		if err != nil {
			return false
		}
		defer closer.Close()
		if binary {
			return false
		}
		fileMatches, err := scanFileMatches(reader, rel, re, limit)
		if err != nil {
			return false
		}
		for _, m := range fileMatches {
			if len(matches) >= searchMaxMatches {
				truncated = true
				return true
			}
			// Same byte accounting as the formatted "path:line:content" line.
			lineLen := len(m.Path) + 1 + len(strconv.Itoa(m.Line)) + 1 + len(m.Text)
			if size+lineLen+1 > searchMaxOutputBytes {
				truncated = true
				return true
			}
			matches = append(matches, m)
			size += lineLen + 1
		}
		return false
	})
	if err != nil {
		return nil, false, err
	}
	return matches, truncated, nil
}

// scanFileMatches reads the file once (via the caller-provided reader, which
// already includes the binary probe) in a single streaming pass and returns
// structured matches (no context lines).
func scanFileMatches(r io.Reader, relPath string, re *regexp.Regexp, matchLimit int) ([]SearchMatch, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), searchMaxFileBytes)

	var out []SearchMatch
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if re.MatchString(scanner.Text()) {
			out = append(out, SearchMatch{Path: relPath, Line: lineNum, Text: scanner.Text()})
			if len(out) >= matchLimit {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// formatSearchMatches renders structured matches in the same compact layout
// SearchCode shows: a file header line followed by "line:content" lines,
// grouped by file in match order. Paths are already workspace-relative.
func formatSearchMatches(matches []SearchMatch) string {
	var b strings.Builder
	prevFile := ""
	for _, m := range matches {
		if m.Path != prevFile {
			b.WriteString(m.Path)
			b.WriteByte('\n')
			prevFile = m.Path
		}
		fmt.Fprintf(&b, "%d:%s\n", m.Line, m.Text)
	}
	return strings.TrimSuffix(b.String(), "\n")
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

// scanFileSinglePass reads the file once (via the caller-provided reader,
// which already includes the binary probe), finds matches, and emits results
// with context lines (contextLines > 0). The context-free case is handled by
// scanFileMatches.
func scanFileSinglePass(r io.Reader, relPath string, re *regexp.Regexp, contextLines, matchLimit int) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), searchMaxFileBytes)

	// Context-lines path: store lines to support before/after context.
	var lines []string
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}

	// Find matching line numbers in a single pass over stored lines.
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

	// Use a byte slice (1-indexed) instead of a map for the emit-mask.
	// 0 = skip, ':' = match line, '-' = context line.
	want := make([]byte, len(lines)+1)
	for _, n := range matchNums {
		start := n - contextLines
		if start < 1 {
			start = 1
		}
		end := n + contextLines
		if end > len(lines) {
			end = len(lines)
		}
		for i := start; i <= end; i++ {
			if i == n || want[i] == 0 {
				want[i] = ':'
			} else if want[i] != ':' {
				want[i] = '-'
			}
		}
	}

	var out []string
	for lineNum = 1; lineNum <= len(lines); lineNum++ {
		sep := want[lineNum]
		if sep == 0 {
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

// openSearchableFile opens path and probes the first searchBinaryProbe bytes
// for NUL (binary detection) in the SAME open the scan will use. A binary
// file returns binary=true with a nil reader (the probe bytes are discarded,
// so binaries still cost only one 8KB read). A text file returns a reader
// over the whole file — the probe bytes followed by the rest — so the caller
// reads the file exactly once. The previous design probed with a separate
// open (isBinaryFile) and then opened the file again for the scan, doubling
// file opens on slow filesystems whenever ripgrep was unavailable. The probe
// bytes are copied out of the pooled buffer because the returned reader
// outlives the pool's ownership of the buffer.
func openSearchableFile(path string) (reader io.Reader, closer io.Closer, binary bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false, err
	}
	bp := binaryProbePool.Get().(*[]byte)
	buf := *bp
	n, _ := f.Read(buf)
	binaryProbePool.Put(bp)
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			_ = f.Close()
			return nil, nil, true, nil
		}
	}
	probe := make([]byte, n)
	copy(probe, buf[:n])
	return io.MultiReader(bytes.NewReader(probe), f), f, false, nil
}

// searchableWalkFile reports whether a walked file is worth reading: the
// stat succeeded and the file is within the search size cap. The binary
// probe is deferred to the read (openSearchableFile) so each file is opened
// exactly once; walkers silently skip files that fail this check.
func searchableWalkFile(path string, info os.FileInfo, err error) bool {
	if err != nil || info == nil || info.Size() > searchMaxFileBytes {
		return false
	}
	return true
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
	file, _, num, sep, tail, ok := splitSearchLineParts(line)
	if !ok {
		return "", "", false
	}
	return file, num + string(sep) + tail, true
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

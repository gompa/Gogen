package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	exploreMaxEntries      = 500
	readFilesMaxCount      = 20
	readFilesMaxTotalBytes = 512 * 1024
)

func relDisplayPath(searchRoot, absPath string, isDir bool) (string, error) {
	rel, err := filepath.Rel(searchRoot, absPath)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}
	if isDir && rel != "" {
		rel += "/"
	}
	return rel, nil
}

// cachedGitPath caches the result of exec.LookPath("git") so repeated calls
// don't re-scan PATH.
var cachedGitPath struct {
	sync.Once
	path string
	ok   bool
}

func gitPath() (string, bool) {
	cachedGitPath.Do(func() {
		p, err := exec.LookPath("git")
		cachedGitPath.path = p
		cachedGitPath.ok = err == nil
	})
	return cachedGitPath.path, cachedGitPath.ok
}

// globRegexCache caches compiled regexes for glob patterns containing **,
// avoiding recompilation for every file during WalkDir. The cache lives for
// the lifetime of the process. When the cache exceeds globRegexCacheMax
// entries, the oldest entry (by insertion order) is evicted rather than
// dropping the entire map. This avoids cache thrashing under burst queries.
const globRegexCacheMax = 100

// globRegexCache is a package-level, mutex-guarded map of compiled regexes.
var (
	globRegexMu    sync.Mutex
	globRegexCache = make(map[string]*regexp.Regexp)
	// globRegexOrder tracks insertion order for FIFO eviction.
	globRegexOrder []string
)

// resetGlobRegexCacheLocked clears the glob regex cache and insertion-order
// slice. Caller must hold globRegexMu. Exposed for tests only.
func resetGlobRegexCacheLocked() {
	globRegexCache = make(map[string]*regexp.Regexp)
	globRegexOrder = globRegexOrder[:0]
}

// ListFiles lists directory entries. When recursive is true, walks the tree (max 500 paths).
// When trackedOnly is true, results are filtered to git-tracked files.
func (e *Executor) ListFiles(path string, recursive, trackedOnly bool) (string, error) {
	secure, err := e.SecurePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(secure)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", path)
	}
	if !recursive {
		entries, err := os.ReadDir(secure)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			sb.WriteString(name)
			sb.WriteByte('\n')
		}
		return sb.String(), nil
	}

	var lines []string
	err = filepath.WalkDir(secure, func(walkPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if walkPath == secure {
			return nil
		}
		if shouldSkipSearchEntry(d.Name(), d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := relDisplayPath(secure, walkPath, d.IsDir())
		if err != nil {
			return nil
		}
		if rel == "" {
			return nil
		}
		lines = append(lines, rel)
		if len(lines) >= exploreMaxEntries {
			return errExploreTruncated
		}
		return nil
	})
	if err != nil && err != errExploreTruncated {
		return "", err
	}
	sort.Strings(lines)
	if trackedOnly {
		lines = filterTracked(e.WorkingDir, lines)
	}
	out := strings.Join(lines, "\n")
	if err == errExploreTruncated {
		out += fmt.Sprintf("\n… truncated (showing first %d entries)", len(lines))
	}
	if out == "" {
		return "(empty)", nil
	}
	return out, nil
}

func filterTracked(workingDir string, paths []string) []string {
	git, ok := gitPath()
	if !ok || len(paths) == 0 {
		return paths
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "ls-files", "--cached", "--others", "--exclude-standard")
	cmd.Dir = workingDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return paths
	}
	tracked := make(map[string]struct{}, len(paths))
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tracked[filepath.ToSlash(line)] = struct{}{}
		}
	}
	if len(tracked) == 0 {
		return paths
	}
	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, ok := tracked[filepath.ToSlash(p)]; ok {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return paths
	}
	return filtered
}

var errExploreTruncated = fmt.Errorf("explore truncated")

// GlobFiles finds files matching a glob pattern under path (default .).
// Patterns may match basenames (*.go) or relative paths (internal/*.go, **/*.md).
func (e *Executor) GlobFiles(pattern, subpath string, trackedOnly bool) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	searchRoot, relPrefix, err := e.searchRoot(subpath)
	if err != nil {
		return "", err
	}

	var matches []string
	err = filepath.WalkDir(searchRoot, func(walkPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if walkPath != searchRoot && shouldSkipSearchEntry(d.Name(), true) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(searchRoot, walkPath)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if relPrefix != "" {
			rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
		}
		if !matchGlobPattern(pattern, rel) {
			return nil
		}
		matches = append(matches, rel)
		if len(matches) >= exploreMaxEntries {
			return errExploreTruncated
		}
		return nil
	})
	if err != nil && err != errExploreTruncated {
		return "", err
	}
	if len(matches) == 0 {
		return "No matches found", nil
	}
	sort.Strings(matches)
	if trackedOnly {
		matches = filterTracked(e.WorkingDir, matches)
	}
	out := strings.Join(matches, "\n")
	if err == errExploreTruncated {
		out += fmt.Sprintf("\n… truncated (showing first %d matches)", len(matches))
	}
	return out, nil
}

func matchGlobPattern(pattern, relPath string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	relPath = filepath.ToSlash(relPath)
	if pattern == "" {
		return false
	}
	// Handle ** (zero or more directories) by converting to a regex.
	if !strings.Contains(pattern, "/") {
		base := relPath
		if idx := strings.LastIndex(relPath, "/"); idx >= 0 {
			base = relPath[idx+1:]
		}
		ok, err := filepath.Match(pattern, base)
		if err != nil {
			// Malformed pattern (filepath.ErrBadPattern). The path-based
			// branch below also treats bad patterns as no-match, so do the
			// same here rather than silently redefining a bad glob as a
			// substring match.
			return false
		}
		return ok
	}
	if strings.Contains(pattern, "**") {
		return matchGlobRegex(pattern, relPath)
	}
	// Path-based patterns without ** use filepath.Match.
	ok, err := filepath.Match(pattern, relPath)
	if err != nil {
		return false
	}
	return ok
}

// matchGlobRegex handles glob patterns that contain ** by converting
// them to a regular expression. ** matches zero or more path segments.
func matchGlobRegex(pattern, path string) bool {
	// Fast path: read a compiled regex under the shared lock. The lookup is
	// cheap and the regex (once compiled) is safe for concurrent use.
	globRegexMu.Lock()
	re, ok := globRegexCache[pattern]
	globRegexMu.Unlock()
	if ok {
		return re.MatchString(path)
	}
	// Split pattern into segments, then convert each segment.
	segments := strings.Split(pattern, "/")
	var reParts []string
	reParts = append(reParts, "^")
	for i, seg := range segments {
		if i > 0 {
			reParts = append(reParts, "/")
		}
		if seg == "**" {
			// Leading or middle "**" matches zero or more path segments
			// (`(?:.*/)?`); a trailing "**" matches zero or more of any
			// character including "/" (`.*`) so "**/*.go" and "src/**"
			// both behave intuitively.
			if i == 0 {
				reParts = append(reParts, `(?:.*/)?`)
			} else if i == len(segments)-1 {
				reParts = append(reParts, `.*`)
			} else {
				reParts = append(reParts, `(?:.*/)?`)
			}
		} else {
			// Convert * and ? within a single path segment (not crossing /).
			escaped := regexp.QuoteMeta(seg)
			escaped = strings.ReplaceAll(escaped, `\*`, `[^/]*`)
			escaped = strings.ReplaceAll(escaped, `\?`, `[^/]`)
			reParts = append(reParts, escaped)
		}
	}
	reParts = append(reParts, "$")
	reStr := strings.Join(reParts, "")
	re = regexp.MustCompile(reStr)
	// Insert under the lock. If this insert pushes us over the cap, evict
	// the oldest entry (by insertion order) rather than clearing the entire
	// map. This avoids cache thrashing when many distinct patterns are used
	// in quick succession.
	globRegexMu.Lock()
	if _, ok := globRegexCache[pattern]; !ok {
		if len(globRegexCache) >= globRegexCacheMax && len(globRegexOrder) > 0 {
			victim := globRegexOrder[0]
			globRegexOrder = globRegexOrder[1:]
			delete(globRegexCache, victim)
		}
		globRegexCache[pattern] = re
		globRegexOrder = append(globRegexOrder, pattern)
	}
	globRegexMu.Unlock()
	return re.MatchString(path)
}

// ReadFiles reads multiple files and returns them with path headers.
func (e *Executor) ReadFiles(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("paths is required")
	}
	if len(paths) > readFilesMaxCount {
		return "", fmt.Errorf("too many paths (max %d)", readFilesMaxCount)
	}

	var b strings.Builder
	total := 0
	truncated := false

	for i, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		secure, err := e.SecurePath(path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(secure)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", path)
		}
		if info.Size() > searchMaxFileBytes {
			return "", fmt.Errorf("%s exceeds max file size (%d bytes)", path, searchMaxFileBytes)
		}
		content, err := os.ReadFile(secure)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		header := fmt.Sprintf("=== %s ===\n", filepath.ToSlash(path))
		block := header + string(content)
		if i > 0 {
			block = "\n" + block
		}
		if total+len(block) > readFilesMaxTotalBytes {
			truncated = true
			remain := readFilesMaxTotalBytes - total
			if remain <= len(header)+20 {
				break
			}
			block = block[:remain] + fmt.Sprintf("\n… truncated (%d bytes total across files)", total+remain)
			b.WriteString(block)
			total += remain
			break
		}
		b.WriteString(block)
		total += len(block)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("no readable files in paths")
	}
	out := b.String()
	if truncated && !strings.Contains(out, "truncated") {
		out += fmt.Sprintf("\n… truncated (read %d bytes)", total)
	}
	return out, nil
}

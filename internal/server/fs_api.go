package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"gogen/internal/agent"
)

const (
	fsListMaxEntries = 500
	fsReadMaxBytes   = 2 * 1024 * 1024
)

// validateTextFile checks if data is valid text content suitable for the editor.
// It rejects binary files (NUL bytes), oversized files, and invalid UTF-8.
func validateTextFile(data []byte, maxSize int) error {
	if bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("binary file not supported")
	}
	if len(data) > maxSize {
		return fmt.Errorf("file too large (%d bytes; max %d)", len(data), maxSize)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("file is not valid UTF-8")
	}
	return nil
}

type FSEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

type GitStatusEntry struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// GitStatus is the pre-bucketed result of `git status --porcelain=v2 -b`.
// Clients consume ready-to-render lists and never see the XY matrix; a
// partially staged file appears in BOTH Staged and Unstaged (matching VS
// Code). Branch/Upstream/Ahead/Behind come from the "# branch.*" headers
// (empty/zero when the repo has no branch info).
type GitStatus struct {
	Branch    string           `json:"branch,omitempty"`
	Upstream  string           `json:"upstream,omitempty"`
	Ahead     int              `json:"ahead,omitempty"`
	Behind    int              `json:"behind,omitempty"`
	Staged    []GitStatusEntry `json:"staged,omitempty"`
	Unstaged  []GitStatusEntry `json:"unstaged,omitempty"`
	Untracked []GitStatusEntry `json:"untracked,omitempty"`
	Unmerged  []GitStatusEntry `json:"unmerged,omitempty"`
}

// extLanguage maps a lowercase file extension to the language name sent to
// the web editor. Unknown extensions fall back to "plaintext".
var extLanguage = map[string]string{
	".go":       "go",
	".js":       "javascript",
	".mjs":      "javascript",
	".cjs":      "javascript",
	".jsx":      "javascript",
	".ts":       "typescript",
	".tsx":      "typescript",
	".json":     "json",
	".md":       "markdown",
	".markdown": "markdown",
	".html":     "html",
	".htm":      "html",
	".css":      "css",
	".scss":     "scss",
	".less":     "less",
	".yaml":     "yaml",
	".yml":      "yaml",
	".toml":     "ini",
	".xml":      "xml",
	".sh":       "shell",
	".bash":     "shell",
	".zsh":      "shell",
	".py":       "python",
	".rs":       "rust",
	".java":     "java",
	".c":        "c",
	".h":        "c",
	".cpp":      "cpp",
	".cc":       "cpp",
	".cxx":      "cpp",
	".hpp":      "cpp",
	".cs":       "csharp",
	".sql":      "sql",
	".rb":       "ruby",
	".php":      "php",
	".swift":    "swift",
	".kt":       "kotlin",
	".lua":      "lua",
	".r":        "r",
	".diff":     "diff",
	".patch":    "diff",
	".mod":      "go",
}

// languageFromPath maps a file path to the editor language name. Pure
// lookup; unknown extensions fall back to "plaintext".
func languageFromPath(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if lang, ok := extLanguage[ext]; ok {
		return lang
	}
	return "plaintext"
}

func (s *Server) fsList(path string) ([]FSEntry, error) {
	exec := s.ws.Exec
	if path == "" {
		path = "."
	}
	secure, err := exec.SecurePath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(secure)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", path)
	}
	entries, err := os.ReadDir(secure)
	if err != nil {
		return nil, err
	}
	out := make([]FSEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." {
			continue
		}
		rel := name
		if path != "." && path != "" {
			rel = filepath.ToSlash(filepath.Join(path, name))
		} else {
			rel = filepath.ToSlash(name)
		}
		out = append(out, FSEntry{Name: name, Path: rel, IsDir: entry.IsDir()})
		if len(out) >= fsListMaxEntries {
			break
		}
	}
	return out, nil
}

func (s *Server) fsRead(path string) (content, language string, err error) {
	data, err := s.ws.Exec.ReadFileRawBytes(path)
	if err != nil {
		return "", "", err
	}
	if err := validateTextFile(data, fsReadMaxBytes); err != nil {
		return "", "", err
	}
	return string(data), languageFromPath(path), nil
}

func (s *Server) fsSearch(ctx context.Context, pattern, path, glob string) ([]agent.SearchMatch, bool, error) {
	if s.ws == nil || s.ws.Exec == nil {
		return nil, false, fmt.Errorf("executor unavailable")
	}
	return s.ws.Exec.SearchCodeMatches(ctx, pattern, path, glob)
}

func (s *Server) fsWrite(path, content string) error {
	return s.ws.Exec.OverwriteFile(path, content)
}

// fsReplace performs a regex search-and-replace across files matching the given
// pattern (same semantics as fs_search / SearchCode). It walks the tree rather
// than relying on the capped search result set, so replace-all is complete.
// fsReplace also returns the workspace-relative paths of the files that were
// modified, so the editor can refresh open buffers for exactly those files.
func (s *Server) fsReplace(ctx context.Context, search, replacement, subpath, glob string) (replaced int, fileCount int, files []string, err error) {
	if s.ws == nil || s.ws.Exec == nil {
		return 0, 0, nil, fmt.Errorf("executor unavailable")
	}
	return s.ws.Exec.ReplaceInTree(ctx, search, replacement, subpath, glob)
}

// gitStatusEntries runs `git status --porcelain=v2 -b` (single command) and
// returns the pre-bucketed status.
func (s *Server) gitStatusEntries(ctx context.Context) (GitStatus, error) {
	exec := s.ws.Exec
	cmd, err := exec.NewGitCmd(ctx, "status", "--porcelain=v2", "-b")
	if err != nil {
		return GitStatus{}, err
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		msg := strings.TrimSpace(text)
		if msg == "" {
			msg = err.Error()
		}
		return GitStatus{}, fmt.Errorf("git status failed: %s", msg)
	}
	return parsePorcelainV2(text), nil
}

// parsePorcelainV2 parses `git status --porcelain=v2 -b` output into
// pre-bucketed lists. Pure function; no git invocation.
//
// Record rules:
//   - "1 XY ... path": X not ' '/'.' → Staged (status X); Y not ' '/'.'
//     → Unstaged (status Y). A file may appear in BOTH lists (partially
//     staged). '.' means "no change" (worktree column for new index entries).
//   - "2 XY ... path": Unmerged (status U), also into Staged/Unstaged per
//     column so conflicts stay actionable there.
//   - "? path": Untracked (status U).
//   - "N ... old -> new": rename/copy companion of the previous record —
//     the entry's path is replaced with the new path.
//   - "# branch.head/upstream/ab" headers fill the branch fields; other
//     "#" lines (and T/u records) are ignored.
func parsePorcelainV2(text string) GitStatus {
	var st GitStatus
	// Rename/copy companion lines ("N") arrive AFTER their "1"/"2" record;
	// remember the last record's path and the slice positions of the entries
	// appended for it so the N line can swap in the new path.
	lastFrom := ""
	lastStaged, lastUnstaged := -1, -1
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case '#':
			parseBranchHeader(line, &st)
		case '1', '2':
			if len(line) < 5 {
				continue
			}
			// "1 <XY> <sub> <hM> <m1> <m2> <m3> <o> <X> <Y> <path>": the
			// path is the LAST field (quoted paths contain no raw spaces,
			// so Fields is safe).
			xy := line[2:4]
			fields := strings.Fields(line[5:])
			if len(fields) == 0 {
				continue
			}
			path := unquoteGitPath(fields[len(fields)-1])
			lastFrom, lastStaged, lastUnstaged = path, -1, -1
			if line[0] == '2' {
				st.Unmerged = append(st.Unmerged, GitStatusEntry{Path: path, Status: "U"})
			}
			// ' ' or '.' means no change in that column ('.' is used for
			// the worktree column when the index entry is new and matches).
			if x := xy[0]; x != ' ' && x != '.' {
				lastStaged = len(st.Staged)
				st.Staged = append(st.Staged, GitStatusEntry{Path: path, Status: string(x)})
			}
			if y := xy[1]; y != ' ' && y != '.' {
				lastUnstaged = len(st.Unstaged)
				st.Unstaged = append(st.Unstaged, GitStatusEntry{Path: path, Status: string(y)})
			}
		case 'N':
			if len(line) < 3 || lastFrom == "" {
				continue
			}
			arrow := strings.Index(line, " -> ")
			if arrow < 0 {
				continue
			}
			fromFields := strings.Fields(line[:arrow])
			if len(fromFields) == 0 || unquoteGitPath(fromFields[len(fromFields)-1]) != lastFrom {
				continue
			}
			to := unquoteGitPath(strings.TrimSpace(line[arrow+4:]))
			if lastStaged >= 0 {
				st.Staged[lastStaged].Path = to
			}
			if lastUnstaged >= 0 {
				st.Unstaged[lastUnstaged].Path = to
			}
		case '?':
			if len(line) < 3 {
				continue
			}
			path := unquoteGitPath(strings.TrimSpace(line[2:]))
			if path == "" {
				continue
			}
			st.Untracked = append(st.Untracked, GitStatusEntry{Path: path, Status: "U"})
		}
	}
	return st
}

// parseBranchHeader fills the branch fields from "# branch.head <name>",
// "# branch.upstream <name>" and "# branch.ab +N -M" lines; other "#"
// headers are ignored.
func parseBranchHeader(line string, st *GitStatus) {
	rest := strings.TrimSpace(line[1:])
	i := strings.IndexByte(rest, ' ')
	if i < 0 {
		return
	}
	key, val := rest[:i], strings.TrimSpace(rest[i+1:])
	switch key {
	case "branch.head":
		st.Branch = val
	case "branch.upstream":
		st.Upstream = val
	case "branch.ab":
		// Format is "+N -M": the behind count arrives negative.
		fields := strings.Fields(val)
		if len(fields) != 2 {
			return
		}
		if a, err := strconv.Atoi(fields[0]); err == nil {
			st.Ahead = a
		}
		if b, err := strconv.Atoi(fields[1]); err == nil && b < 0 {
			st.Behind = -b
		}
	}
}

// unquoteGitPath decodes a C-style-quoted path from porcelain output (git
// quotes paths containing special bytes; escapes are C-style, with octal
// sequences carrying raw byte values). Non-quoted paths pass through.
func unquoteGitPath(p string) string {
	if len(p) < 2 || p[0] != '"' {
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	for i := 1; i < len(p); i++ {
		c := p[i]
		if c == '"' {
			break
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(p) {
			b.WriteByte('\\')
			continue
		}
		i++
		switch e := p[i]; e {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := int(e - '0')
			for n := 1; n < 3 && i+1 < len(p) && p[i+1] >= '0' && p[i+1] <= '7'; n++ {
				i++
				v = v*8 + int(p[i]-'0')
			}
			b.WriteByte(byte(v))
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
		}
	}
	return b.String()
}

func (s *Server) gitFileDiff(ctx context.Context, path string) (original, modified, language string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", "", fmt.Errorf("path is required")
	}
	language = languageFromPath(path)
	exec := s.ws.Exec
	if _, err := exec.SecurePath(path); err != nil {
		return "", "", "", err
	}

	modified, err = readWorkingTreeText(exec, path)
	if err != nil {
		return "", "", "", err
	}

	original, err = gitShowHEAD(ctx, exec, path)
	if err != nil {
		if isGitMissingPath(err) {
			return "", modified, language, nil
		}
		return "", "", "", err
	}
	return original, modified, language, nil
}

func readWorkingTreeText(exec *agent.Executor, path string) (string, error) {
	data, err := exec.ReadFileRawBytes(path)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(strings.ToLower(err.Error()), "no such file") {
			return "", nil
		}
		return "", err
	}
	if err := validateTextFile(data, fsReadMaxBytes); err != nil {
		return "", err
	}
	return string(data), nil
}

func gitShowHEAD(ctx context.Context, exec *agent.Executor, path string) (string, error) {
	cmd, err := exec.NewGitCmd(ctx, "show", "HEAD:"+filepath.ToSlash(path))
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	if err := validateTextFile(out, fsReadMaxBytes); err != nil {
		return "", err
	}
	return string(out), nil
}

func isGitMissingPath(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "exists on disk, but not in") ||
		strings.Contains(msg, "bad revision") ||
		strings.Contains(msg, "path does not exist") ||
		strings.Contains(msg, "is outside repository") ||
		strings.Contains(msg, "invalid object name")
}

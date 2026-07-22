package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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

func languageFromPath(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".js", ".mjs", ".cjs", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".json":
		return "json"
	case ".md", ".markdown":
		return "markdown"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".scss":
		return "scss"
	case ".less":
		return "less"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "ini"
	case ".xml":
		return "xml"
	case ".sh", ".bash", ".zsh":
		return "shell"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".cxx", ".hpp":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".sql":
		return "sql"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".swift":
		return "swift"
	case ".kt":
		return "kotlin"
	case ".lua":
		return "lua"
	case ".r":
		return "r"
	case ".diff", ".patch":
		return "diff"
	case ".mod":
		return "go"
	default:
		return "plaintext"
	}
}

func (s *Server) fsList(path string) ([]FSEntry, error) {
	exec := s.agent.Executor
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
	data, err := s.agent.Executor.ReadFileRawBytes(path)
	if err != nil {
		return "", "", err
	}
	if err := validateTextFile(data, fsReadMaxBytes); err != nil {
		return "", "", err
	}
	return string(data), languageFromPath(path), nil
}

func (s *Server) fsSearch(ctx context.Context, pattern, path, glob string) ([]agent.SearchMatch, bool, error) {
	if s.agent == nil || s.agent.Executor == nil {
		return nil, false, fmt.Errorf("executor unavailable")
	}
	return s.agent.Executor.SearchCodeMatches(ctx, pattern, path, glob)
}

func (s *Server) fsWrite(path, content string) error {
	return s.agent.Executor.OverwriteFile(path, content)
}

// fsReplace performs a regex search-and-replace across files matching the given
// pattern (same semantics as fs_search / SearchCode). It walks the tree rather
// than relying on the capped search result set, so replace-all is complete.
func (s *Server) fsReplace(ctx context.Context, search, replacement, subpath, glob string) (replaced int, fileCount int, err error) {
	if s.agent == nil || s.agent.Executor == nil {
		return 0, 0, fmt.Errorf("executor unavailable")
	}
	return s.agent.Executor.ReplaceInTree(ctx, search, replacement, subpath, glob)
}

func (s *Server) gitStatusEntries(ctx context.Context) ([]GitStatusEntry, error) {
	exec := s.agent.Executor
	cmd, err := exec.NewGitCmd(ctx, "status", "--porcelain", "-uall")
	if err != nil {
		return nil, err
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		msg := strings.TrimSpace(text)
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git status failed: %s", msg)
	}
	return parsePorcelainUnstaged(text), nil
}

// parsePorcelainUnstaged returns working-tree (unstaged / untracked) changes.
func parsePorcelainUnstaged(text string) []GitStatusEntry {
	var entries []GitStatusEntry
	for _, line := range strings.Split(text, "\n") {
		if len(line) < 4 {
			continue
		}
		xy := line[:2]
		rest := strings.TrimSpace(line[2:])
		if rest == "" {
			continue
		}
		path := rest
		if i := strings.Index(rest, " -> "); i >= 0 {
			path = rest[i+4:]
		}
		path = filepath.ToSlash(path)

		if xy == "??" {
			entries = append(entries, GitStatusEntry{Path: path, Status: "untracked"})
			continue
		}
		wt := xy[1]
		if wt == ' ' {
			continue
		}
		status := "modified"
		switch wt {
		case 'M':
			status = "modified"
		case 'D':
			status = "deleted"
		case 'A':
			status = "added"
		case 'R':
			status = "renamed"
		case 'C':
			status = "copied"
		case 'U':
			status = "unmerged"
		case '?':
			status = "untracked"
		default:
			status = string(wt)
		}
		entries = append(entries, GitStatusEntry{Path: path, Status: status})
	}
	return entries
}

func (s *Server) gitFileDiff(ctx context.Context, path string) (original, modified, language string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", "", fmt.Errorf("path is required")
	}
	language = languageFromPath(path)
	exec := s.agent.Executor
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

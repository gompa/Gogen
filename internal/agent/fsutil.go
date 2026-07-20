package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gogen/internal/debuglog"
)

// evalPath resolves symlinks for an existing path, or for the nearest existing
// parent when creating a new file.
func evalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, lerr := os.Lstat(abs); lerr == nil {
		return filepath.EvalSymlinks(abs)
	} else if !os.IsNotExist(lerr) {
		return "", lerr
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs, nil
	}
	resolvedParent, err := evalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}

func writeFileAtomic(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Preserve the existing file mode when overwriting, so execute bits
	// on scripts are not destroyed.
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode()
	}
	tmp, err := os.CreateTemp(dir, ".gogen-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	// Chmod may be unsupported on some filesystems (Windows, FUSE, 9p, some
	// network mounts). When that happens, don't fail the whole write — log
	// a debug entry and continue. The temp file's default mode (typically
	// 0600) will be inherited by the renamed final file, but the content
	// write itself still succeeds.
	if err := tmp.Chmod(perm); err != nil {
		if !isChmodUnsupported(err) {
			_ = tmp.Close()
			return err
		}
		debuglog.Write("fsutil/write", "Chmod unsupported; file written with default mode", "fs-chmod-unsupported", map[string]interface{}{
			"path": path,
			"err":  err.Error(),
		})
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// isChmodUnsupported reports whether a chmod failure is a "not supported"
// error we should ignore rather than propagate. Chmod can return ENOTSUP,
// ENOSYS, EOPNOTSUPP, or Windows ERROR_INVALID_FUNCTION on some filesystems
// (FUSE, 9p, network mounts) where mode bits aren't tracked.
func isChmodUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
		return true
	}
	// Fallback: Windows ERROR_INVALID_FUNCTION or other platform-specific
	// errors that don't map to standard POSIX codes.
	var pe *os.PathError
	if errors.As(err, &pe) {
		s := pe.Err.Error()
		if strings.Contains(s, "not supported") ||
			strings.Contains(s, "not implemented") ||
			strings.Contains(s, "operation not supported") {
			return true
		}
	}
	s := err.Error()
	return strings.Contains(s, "not supported") ||
		strings.Contains(s, "not implemented") ||
		strings.Contains(s, "operation not supported")
}

func isWithinRoot(resolvedPath, resolvedRoot string) bool {
	if resolvedPath == resolvedRoot {
		return true
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (e *Executor) securePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	absWD, err := filepath.Abs(e.WorkingDir)
	if err != nil {
		return "", err
	}
	resolvedWD, err := evalPath(absWD)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}

	absPath, err := resolveExecutorPath(absWD, path)
	if err != nil {
		return "", err
	}
	resolvedPath, err := evalPath(absPath)
	if err != nil {
		return "", err
	}
	if !isWithinRoot(resolvedPath, resolvedWD) {
		return "", fmt.Errorf("path %s is outside of working directory %s", path, absWD)
	}
	return resolvedPath, nil
}

// SecurePath resolves path under the working directory and rejects escapes.
func (e *Executor) SecurePath(path string) (string, error) {
	return e.securePath(path)
}

// resolveExecutorPath maps a user/model path to an absolute path under the working directory.
func resolveExecutorPath(workingDir, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Abs(path)
	}

	joined, err := filepath.Abs(filepath.Join(workingDir, path))
	if err != nil {
		return "", err
	}

	if fixed, ok := fixDoubledWorkingDirPath(joined, workingDir); ok {
		return fixed, nil
	}
	return joined, nil
}

// fixDoubledWorkingDirPath detects when filepath.Join(WD, path) produced a
// doubled WD prefix (e.g. model passes "a/b/file" → joined to "/a/b/a/b/file").
// When the suffix after the first WD prefix itself starts with the WD path
// (from root), the model intended an absolute-like path; we return the correct
// resolution by treating the suffix as the intended absolute path.
func fixDoubledWorkingDirPath(absPath, workingDir string) (string, bool) {
	wd, err := filepath.Abs(workingDir)
	if err != nil {
		return "", false
	}
	wd = filepath.Clean(wd)
	absPath = filepath.Clean(absPath)

	prefix := wd + string(filepath.Separator)
	if !strings.HasPrefix(absPath, prefix) {
		return "", false
	}
	suffix := strings.TrimPrefix(absPath, prefix)
	wdFromRoot := strings.TrimPrefix(filepath.ToSlash(wd), "/")
	suffixSlash := filepath.ToSlash(suffix)
	// Check if the suffix contains the WD path again (doubled prefix).
	if suffixSlash != wdFromRoot && !strings.HasPrefix(suffixSlash, wdFromRoot+"/") {
		return "", false
	}

	// The suffix is the intended absolute-like path; prepend "/" to resolve.
	candidate := filepath.Clean(string(filepath.Separator) + suffixSlash)
	_, statErr := os.Stat(candidate)
	if statErr != nil {
		// For new files, verify the parent directory exists.
		_, perr := os.Stat(filepath.Dir(candidate))
		if perr != nil {
			return "", false
		}
		return candidate, true
	}
	return candidate, true
}

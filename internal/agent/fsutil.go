package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gogen/internal/ioutil"
)

// validateFileOp validates source/destination paths for CopyFile/MoveFile,
// checks the source is a regular file, and creates the destination parent
// directory. Both operations share this exact preamble.
// It returns the resolved secure paths for both src and dst.
func (e *Executor) validateFileOp(src, dst string) (srcSecure, dstSecure string, err error) {
	if src == "" || dst == "" {
		return "", "", fmt.Errorf("source and destination paths are required")
	}
	srcSecure, err = e.SecurePath(src)
	if err != nil {
		return "", "", err
	}
	dstSecure, err = e.SecurePath(dst)
	if err != nil {
		return "", "", err
	}
	info, err := os.Lstat(srcSecure)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("source is a directory; operation only supports files")
	}
	// Ensure destination parent directory exists.
	dstDir := filepath.Dir(dstSecure)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", "", err
	}
	return srcSecure, dstSecure, nil
}

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

// writeFileAtomic is a convenience wrapper around ioutil.WriteFileAtomic.
func writeFileAtomic(path string, content []byte, perm os.FileMode) error {
	return ioutil.WriteFileAtomic(path, content, perm)
}

func (e *Executor) SecurePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Determine the boundary root: PathBoundary (e.g. $HOME in global mode)
	// or WorkingDir (project mode).
	root := e.PathBoundary
	if root == "" {
		root = e.GetWorkingDir()
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := evalPath(rootAbs)
	if err != nil {
		return "", fmt.Errorf("path boundary: %w", err)
	}

	// Resolve the requested path relative to the working directory (not the
	// boundary), so relative paths work as expected.
	wdAbs, err := filepath.Abs(e.GetWorkingDir())
	if err != nil {
		return "", err
	}
	absPath, err := resolveExecutorPath(wdAbs, path)
	if err != nil {
		return "", err
	}
	resolvedPath, err := evalPath(absPath)
	if err != nil {
		return "", err
	}
	if !isWithinRoot(resolvedPath, resolvedRoot) {
		return "", fmt.Errorf("path %s is outside of allowed boundary %s", path, rootAbs)
	}
	return resolvedPath, nil
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

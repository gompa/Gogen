package ioutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"gogen/internal/debuglog"
)

// WriteFileAtomic writes content to a file atomically using a temp file + rename.
// It creates parent directories as needed, preserves existing file permissions
// when overwriting, and handles unsupported chmod gracefully on some filesystems.
func WriteFileAtomic(path string, content []byte, perm os.FileMode) error {
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
		debuglog.Write("ioutil/write", "Chmod unsupported; file written with default mode", "fs-chmod-unsupported", map[string]interface{}{
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
		if containsAny(s, "not supported", "not implemented", "operation not supported") {
			return true
		}
	}
	s := err.Error()
	return containsAny(s, "not supported", "not implemented", "operation not supported")
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
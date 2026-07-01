package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// MoveFile renames or moves a file within the working directory.
func (e *Executor) MoveFile(src, dst string) (string, error) {
	srcSecure, err := e.securePath(src)
	if err != nil {
		return "", err
	}
	dstSecure, err := e.securePath(dst)
	if err != nil {
		return "", err
	}

	info, err := os.Lstat(srcSecure)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("source is a directory; move_file only moves files")
	}

	// Ensure destination directory exists.
	dstDir := filepath.Dir(dstSecure)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}

	if err := os.Rename(srcSecure, dstSecure); err != nil {
		return "", err
	}
	return fmt.Sprintf("Moved %s to %s", src, dst), nil
}

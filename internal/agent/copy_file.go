package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CopyFile copies a file within the working directory.
// When createDirs is true, destination directories are created as needed.
func (e *Executor) CopyFile(src, dst string) (string, error) {
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
		return "", fmt.Errorf("source is a directory; copy_file only copies files")
	}

	// Ensure destination directory exists.
	dstDir := filepath.Dir(dstSecure)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}

	srcFile, err := os.Open(srcSecure)
	if err != nil {
		return "", err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstSecure, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return "", err
	}
	if err := dstFile.Chmod(info.Mode()); err != nil {
		return "", err
	}

	return fmt.Sprintf("Copied %s to %s", src, dst), nil
}

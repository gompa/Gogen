package agent

import (
	"fmt"
	"io"
	"os"
)

// CopyFile copies a file within the working directory.
// When createDirs is true, destination directories are created as needed.
func (e *Executor) CopyFile(src, dst string) (string, error) {
	srcSecure, dstSecure, err := e.validateFileOp(src, dst)
	if err != nil {
		return "", err
	}

	srcFile, err := os.Open(srcSecure)
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstSecure, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer dstFile.Close()
	srcInfo, err := srcFile.Stat()
	if err != nil {
		return "", fmt.Errorf("stat source: %w", err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return "", err
	}
	if err := dstFile.Chmod(srcInfo.Mode()); err != nil {
		return "", err
	}

	return fmt.Sprintf("Copied %s to %s", src, dst), nil
}

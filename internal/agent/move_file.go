package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// MoveFile renames or moves a file within the working directory.
// When the rename fails because source and destination are on different
// filesystems (EXDEV), the function falls back to copy-then-delete.
func (e *Executor) MoveFile(src, dst string) (string, error) {
	srcSecure, dstSecure, err := e.validateFileOp(src, dst)
	if err != nil {
		return "", err
	}

	if err := os.Rename(srcSecure, dstSecure); err != nil {
		// If the rename failed due to EXDEV (cross-device link), fall
		// back to copy-then-delete so the move works across filesystems.
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
			srcFile, openErr := os.Open(srcSecure)
			if openErr != nil {
				return "", fmt.Errorf("open source for cross-device move: %w", openErr)
			}
			defer srcFile.Close()

			dstFile, createErr := os.OpenFile(dstSecure, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, defaultFilePerm)
			if createErr != nil {
				return "", fmt.Errorf("create destination for cross-device move: %w", createErr)
			}
			defer dstFile.Close()

			if _, copyErr := io.Copy(dstFile, srcFile); copyErr != nil {
				return "", fmt.Errorf("copy for cross-device move: %w", copyErr)
			}

			if removeErr := os.Remove(srcSecure); removeErr != nil {
				return "", fmt.Errorf("destination was written but source cleanup (remove) failed: %w", removeErr)
			}
			return fmt.Sprintf("Moved %s to %s (cross-device: copied then removed source)", src, dst), nil
		}
		return "", err
	}

	return fmt.Sprintf("Moved %s to %s", src, dst), nil
}

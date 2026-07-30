package agent

import (
	"errors"
	"fmt"
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
		// Delegates to CopyFile so permission preservation is consistent.
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
			if _, err := e.CopyFile(src, dst); err != nil {
				return "", fmt.Errorf("cross-device copy: %w", err)
			}
			if err := os.Remove(srcSecure); err != nil {
				return "", fmt.Errorf("copied but source cleanup (remove) failed: %w", err)
			}
			return fmt.Sprintf("Moved %s to %s (cross-device: copied then removed source)", src, dst), nil
		}
		return "", err
	}

	return fmt.Sprintf("Moved %s to %s", src, dst), nil
}

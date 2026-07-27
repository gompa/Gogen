package agent

import (
	"fmt"
	"os"
)

// MoveFile renames or moves a file within the working directory.
func (e *Executor) MoveFile(src, dst string) (string, error) {
	srcSecure, dstSecure, err := e.validateFileOp(src, dst)
	if err != nil {
		return "", err
	}

	if err := os.Rename(srcSecure, dstSecure); err != nil {
		return "", err
	}
	return fmt.Sprintf("Moved %s to %s", src, dst), nil
}

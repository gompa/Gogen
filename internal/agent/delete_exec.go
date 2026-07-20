package agent

import (
	"context"
	"fmt"
	"os"
)

// DeleteFile removes a file after user approval when required.
func (e *Executor) DeleteFile(ctx context.Context, path string) (string, error) {
	secure, err := e.SecurePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(secure)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory; delete_file only removes files")
	}
	if err := e.requireDeleteApproval(ctx, []string{path}, "delete_file"); err != nil {
		return "", err
	}
	if err := os.Remove(secure); err != nil {
		return "", err
	}
	return fmt.Sprintf("Deleted %s", path), nil
}

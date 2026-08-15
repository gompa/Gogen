//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

// isStdinClosedErr reports whether a write to a background job's stdin pipe
// failed because the process closed (or lost) its stdin. On Unix the kernel
// surfaces this as EPIPE; after cmd.Wait closes the pipe, writes return
// os.ErrClosed. Both mean "the process is gone / not listening" and should
// be reported as such instead of as a generic I/O error.
func isStdinClosedErr(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)
}

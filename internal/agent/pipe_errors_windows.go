//go:build windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

// isStdinClosedErr reports whether a write to a background job's stdin pipe
// failed because the process closed (or lost) its stdin. Windows never
// produces EPIPE: closing the read end surfaces as ERROR_BROKEN_PIPE, and a
// pipe whose handle is being torn down as ERROR_NO_DATA. os.ErrClosed covers
// the pipe being closed by cmd.Wait.
func isStdinClosedErr(err error) bool {
	return errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.ERROR_BROKEN_PIPE) ||
		errors.Is(err, syscall.ERROR_NO_DATA)
}

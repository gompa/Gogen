//go:build windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

// errorNoData is ERROR_NO_DATA (232), which surfaces when writing to a pipe
// whose read handle is being torn down. The stdlib syscall package does not
// expose it on Windows (only golang.org/x/sys/windows does); declare it
// locally so the agent package needs no x/sys dependency.
const errorNoData syscall.Errno = 232

// isStdinClosedErr reports whether a write to a background job's stdin pipe
// failed because the process closed (or lost) its stdin. Windows never
// produces EPIPE: closing the read end surfaces as ERROR_BROKEN_PIPE, and a
// pipe whose handle is being torn down as ERROR_NO_DATA. os.ErrClosed covers
// the pipe being closed by cmd.Wait.
func isStdinClosedErr(err error) bool {
	return errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.ERROR_BROKEN_PIPE) ||
		errors.Is(err, errorNoData)
}

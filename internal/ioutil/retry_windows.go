//go:build windows

package ioutil

import (
	"errors"
	"syscall"
)

const (
	// errSharingViolation is ERROR_SHARING_VIOLATION (winerror.h 32). The public
	// syscall package does not export it — it lives in the stdlib's internal
	// syscall/windows package and in golang.org/x/sys/windows — so the literal
	// is declared here. syscall.Errno implements errors.Is by numeric
	// comparison, so this matches exactly what os.ReadFile returns.
	errSharingViolation syscall.Errno = 32
	// errAccessDenied is ERROR_ACCESS_DENIED (5), exported by syscall; named
	// here so both retried codes read the same way.
	errAccessDenied = syscall.ERROR_ACCESS_DENIED
)

// isTransientSharingErr reports whether err is the Windows error a concurrent
// atomic replace (or a delete) produces for a reader:
//
//   - errSharingViolation: the target is open elsewhere without
//     FILE_SHARE_DELETE — what our own reader sees when it opens the target
//     exactly while a writer's rename replaces it, and what the failing Windows
//     CI run reported ("being used by another process").
//   - errAccessDenied: the target is in delete-pending state (its last handle is
//     closing after a remove), which also clears within microseconds.
//
// Both are retried. A genuine ACL denial carries ERROR_ACCESS_DENIED too, so it
// is retried as well and then returned unchanged: the cost is the ~0.5s budget
// on an error path that was going to fail anyway.
func isTransientSharingErr(err error) bool {
	return errors.Is(err, errSharingViolation) ||
		errors.Is(err, errAccessDenied)
}

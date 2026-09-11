//go:build windows

package ioutil

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// TestIsTransientSharingErr pins the Windows predicate mapping: the two codes a
// concurrent atomic replace / delete produces are retried, everything else is
// not. Windows-only because the syscall constants do not exist elsewhere.
func TestIsTransientSharingErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sharing violation", errSharingViolation, true},
		{"sharing violation literal", syscall.Errno(32), true},
		{"access denied", errAccessDenied, true},
		{"wrapped sharing violation", fmt.Errorf("open x: %w", &os.PathError{Op: "open", Path: "x", Err: errSharingViolation}), true},
		{"not exist", syscall.ERROR_FILE_NOT_FOUND, false},
		{"generic", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isTransientSharingErr(tc.err); got != tc.want {
			t.Errorf("%s: isTransientSharingErr(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

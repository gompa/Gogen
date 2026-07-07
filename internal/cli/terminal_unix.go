//go:build !windows

package cli

import (
	"syscall"
	"unsafe"
)

// terminalSize returns (columns, rows, ok).  If the ioctl fails or
// either dimension is zero, ok is false.
func terminalSize(fd int) (int, int, bool) {
	type winsize struct {
		Row, Col, XPixel, YPixel uint16
	}
	ws := winsize{}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 || ws.Col == 0 || ws.Row == 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}

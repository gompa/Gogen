package cli

import (
	"os"
	"strconv"
)

func terminalColumns() int {
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if n, err := strconv.Atoi(cols); err == nil && n > 0 {
			return n
		}
	}
	if w, _, ok := terminalSize(int(os.Stdout.Fd())); ok && w > 0 {
		return w
	}
	return 0
}

// terminalRows returns the terminal height in rows, or 0 on failure.
func terminalRows() int {
	if rows := os.Getenv("LINES"); rows != "" {
		if n, err := strconv.Atoi(rows); err == nil && n > 0 {
			return n
		}
	}
	if _, h, ok := terminalSize(int(os.Stdout.Fd())); ok && h > 0 {
		return h
	}
	return 0
}
